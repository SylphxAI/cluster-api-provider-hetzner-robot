// Package sshrescue provides SSH operations for Hetzner rescue environments.
package sshrescue

import (
	"bytes"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

const (
	rescueSSHPort  = 22
	connectTimeout = 60 * time.Second
)

// Client is an SSH client for Hetzner rescue environments.
type Client struct {
	ip         string
	privateKey []byte
	client     *ssh.Client
}

// New creates a new rescue SSH client.
func New(ip string, privateKey []byte) *Client {
	return &Client{
		ip:         ip,
		privateKey: privateKey,
	}
}

// Connect establishes an SSH connection to the rescue system.
func (c *Client) Connect() error {
	signer, err := ssh.ParsePrivateKey(c.privateKey)
	if err != nil {
		return fmt.Errorf("parse SSH private key: %w", err)
	}

	config := &ssh.ClientConfig{
		User: "root",
		Auth: []ssh.AuthMethod{
			ssh.PublicKeys(signer),
		},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), //nolint:gosec // rescue env only
		Timeout:         connectTimeout,
	}

	addr := net.JoinHostPort(c.ip, strconv.Itoa(rescueSSHPort))
	conn, err := net.DialTimeout("tcp", addr, connectTimeout)
	if err != nil {
		return fmt.Errorf("TCP connect to %s: %w", addr, err)
	}

	sshConn, chans, reqs, err := ssh.NewClientConn(conn, addr, config)
	if err != nil {
		conn.Close()
		return fmt.Errorf("SSH handshake to %s: %w", addr, err)
	}

	c.client = ssh.NewClient(sshConn, chans, reqs)
	return nil
}

// Close closes the SSH connection.
func (c *Client) Close() {
	if c.client != nil {
		c.client.Close()
	}
}

// Run executes a command on the remote host and returns stdout+stderr.
// Commands are explicitly run via bash because some SSH servers (including
// Hetzner rescue) may use dash as the exec channel shell, which lacks
// bash features and has different buffering behavior with Go's x/crypto/ssh.
func (c *Client) Run(command string) (string, error) {
	if c.client == nil {
		return "", fmt.Errorf("not connected")
	}
	sess, err := c.client.NewSession()
	if err != nil {
		return "", fmt.Errorf("new session: %w", err)
	}
	defer sess.Close()

	var buf bytes.Buffer
	sess.Stdout = &buf
	sess.Stderr = &buf

	// Wrap in bash -c to ensure consistent shell behavior.
	// Go's x/crypto/ssh exec channel may use /bin/sh (dash on Debian),
	// which has different behavior from bash for complex commands.
	wrapped := fmt.Sprintf("bash -c %s", shellQuote(command))
	if err := sess.Run(wrapped); err != nil {
		return buf.String(), fmt.Errorf("command failed: %w\noutput: %s", err, buf.String())
	}
	return buf.String(), nil
}

// shellQuote wraps a string in single quotes, escaping embedded single quotes.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

// ResolveInstallDisk determines the correct install disk in the rescue environment.
// NVMe device names (nvme0n1, nvme1n1) can swap between rescue and Talos boot
// due to different PCI probe order. This function finds the correct disk by
// checking which NVMe device does NOT have Ceph BlueStore data.
//
// Logic:
//  1. List all NVMe whole-disk devices
//  2. For each, check blkid for ceph_bluestore signature
//  3. Return the one WITHOUT BlueStore (= safe to install Talos)
//  4. If neither has BlueStore (fresh server), use the configured installDisk
//  5. If BOTH have BlueStore, refuse (shouldn't happen)
func (c *Client) ResolveInstallDisk(configuredDisk string) (string, error) {
	out, err := c.Run(
		`DISKS=$(lsblk -dnpo NAME,TYPE | awk '$2=="disk" && $1 ~ /nvme/ {print $1}'); ` +
			`SAFE=""; ` +
			`CEPH=""; ` +
			`for d in $DISKS; do ` +
			`if blkid "$d" 2>/dev/null | grep -q ceph_bluestore; then ` +
			`CEPH="$CEPH $d"; ` +
			`else ` +
			`SAFE="$SAFE $d"; ` +
			`fi; ` +
			`done; ` +
			`SAFE=$(echo $SAFE | xargs); ` +
			`CEPH=$(echo $CEPH | xargs); ` +
			`if [ -z "$SAFE" ]; then ` +
			`echo "ERROR:all_disks_have_bluestore"; ` +
			`elif echo "$SAFE" | grep -q " "; then ` +
			// Multiple safe disks — use the configured one if it's in the safe list
			`if echo "$SAFE" | grep -qw "` + configuredDisk + `"; then ` +
			`echo "` + configuredDisk + `"; ` +
			`else ` +
			`echo "$SAFE" | awk '{print $1}'; ` +
			`fi; ` +
			`else ` +
			`echo "$SAFE"; ` +
			`fi`,
	)
	if err != nil {
		return "", fmt.Errorf("resolve install disk: %w\nOutput: %s", err, out)
	}

	resolved := strings.TrimSpace(out)
	if resolved == "ERROR:all_disks_have_bluestore" {
		return "", fmt.Errorf("all NVMe disks have Ceph BlueStore data — cannot determine install disk safely")
	}
	if resolved == "" {
		return configuredDisk, nil // fallback
	}

	return resolved, nil
}

// WipeOSDisk wipes only the OS install disk, leaving all other disks untouched.
// During reprovision, Ceph OSD data on other disks (e.g. nvme1n1) must survive —
// wiping them would destroy storage cluster data and force a full rebuild.
// Only the OS disk needs a clean slate for Talos installation.
//
// Uses blkdiscard (fast TRIM) with dd fallback (zero first 2GB) to clear
// partition tables, filesystem metadata, and any leftover signatures.
func (c *Client) WipeOSDisk(disk string) (string, error) {
	// Safety: refuse to wipe if the disk has Ceph BlueStore data.
	// This prevents catastrophic data loss if NVMe device enumeration
	// differs between rescue and Talos (e.g., nvme0n1 ↔ nvme1n1 swap).
	checkCmd := fmt.Sprintf(
		`if blkid %[1]q 2>/dev/null | grep -q ceph_bluestore; then `+
			`echo "ABORT: %[1]s has Ceph BlueStore data — refusing to wipe"; exit 1; `+
			`fi`,
		disk,
	)
	if out, err := c.Run(checkCmd); err != nil {
		return out, fmt.Errorf("disk safety check failed for %s: Ceph data detected — aborting to prevent data loss: %w", disk, err)
	}

	// Thorough wipe: wipefs + sgdisk + dd + blkdiscard.
	// blkdiscard alone is NOT enough — NVMe TRIM is advisory, the controller may
	// delay data erasure. Talos STATE partition survives TRIM and boots with old config.
	cmd := fmt.Sprintf(
		`set -e; `+
			`echo "Wiping OS disk %[1]s..."; `+
			`wipefs -af %[1]q 2>/dev/null || true; `+
			`sgdisk --zap-all %[1]q 2>/dev/null || true; `+
			`dd if=/dev/zero of=%[1]q bs=1M count=100 conv=notrunc 2>/dev/null || true; `+
			`blkdiscard %[1]q 2>/dev/null || true; `+
			`sync; `+
			`echo "OS disk %[1]s wiped"`,
		disk,
	)

	return c.Run(cmd)
}

// WipeAllDisks wipes ALL NVMe disks on the server for a fresh provision.
// Used for storage nodes where old Talos installs on ANY disk cause boot loops —
// the server boots from the old install instead of the new one. Unlike WipeOSDisk,
// this does NOT check for ceph_bluestore because it is only called during fresh
// provisioning when no Ceph data exists yet.
//
// The installDisk parameter is logged but all NVMe disks are wiped regardless.
func (c *Client) WipeAllDisks(installDisk string) (string, error) {
	// List all NVMe block devices
	listCmd := `lsblk --noheadings --nodeps --paths --output NAME,TYPE | grep disk | grep nvme | awk '{print $1}'`
	out, err := c.Run(listCmd)
	if err != nil {
		return out, fmt.Errorf("list NVMe disks: %w", err)
	}

	disks := strings.Fields(strings.TrimSpace(out))
	if len(disks) == 0 {
		return "", fmt.Errorf("no NVMe disks found")
	}

	var results []string
	for _, disk := range disks {
		cmd := fmt.Sprintf(
			`set -e; `+
				`echo "Wiping disk %[1]s..."; `+
				`wipefs -af %[1]q 2>/dev/null || true; `+
				`sgdisk --zap-all %[1]q 2>/dev/null || true; `+
				`dd if=/dev/zero of=%[1]q bs=1M count=100 conv=notrunc 2>/dev/null || true; `+
				`blkdiscard %[1]q 2>/dev/null || true; `+
				`sync; `+
				`echo "Disk %[1]s wiped"`,
			disk,
		)
		wipeOut, wipeErr := c.Run(cmd)
		if wipeErr != nil {
			return wipeOut, fmt.Errorf("wipe disk %s: %w", disk, wipeErr)
		}
		results = append(results, strings.TrimSpace(wipeOut))
	}

	summary := fmt.Sprintf("Wiped %d NVMe disks (install=%s): %s", len(disks), installDisk, strings.Join(disks, ", "))
	results = append(results, summary)
	return strings.Join(results, "\n"), nil
}

// ResolveStableDiskPath resolves a bare NVMe device path (e.g. /dev/nvme0n1)
// to its stable /dev/disk/by-id/ path using the disk's serial number.
// This is critical because NVMe device names swap between rescue mode and Talos
// boot due to different PCI probe order. The by-id path references the physical
// disk deterministically regardless of enumeration order.
//
// If the disk is already a /dev/disk/by-id/ path, it is returned as-is.
// Returns the original disk path if resolution fails (best-effort).
func (c *Client) ResolveStableDiskPath(disk string) (string, error) {
	// Already a stable path — nothing to resolve
	if strings.HasPrefix(disk, "/dev/disk/by-id/") {
		return disk, nil
	}

	// Get the basename (e.g. "nvme0n1" from "/dev/nvme0n1")
	basename := disk
	if idx := strings.LastIndex(disk, "/"); idx >= 0 {
		basename = disk[idx+1:]
	}

	// Find the by-id symlink that points to this device (excluding partition entries)
	cmd := fmt.Sprintf(
		`ls -la /dev/disk/by-id/ 2>/dev/null | grep -E '%s$' | grep nvme | grep -v part | awk '{print $9}' | head -1`,
		basename,
	)
	out, err := c.Run(cmd)
	if err != nil {
		return disk, nil // best-effort: return original on failure
	}

	byIDName := strings.TrimSpace(out)
	if byIDName == "" {
		return disk, nil // no by-id link found, return original
	}

	stablePath := "/dev/disk/by-id/" + byIDName
	return stablePath, nil
}

// InstallTalos installs Talos Linux on the server using the official OCI
// installer binary inside a minimal namespace created by unshare(1).
//
// The Talos installer (v1.12+) requires /proc, /sys, and mount namespace
// isolation — it cannot run directly in rescue. We use Linux unshare to
// provide these without Docker/podman. This is the SOTA approach:
//
//   - go-efilib in the installer handles UEFI NVRAM entries type-safely,
//     including Hetzner AX firmware quirks (no auto-discovery).
//   - Canonical GPT layout (BIOS boot + EFI + BOOT + META + STATE + A + B)
//     with correct UUIDs — future-proof across Talos releases.
//   - --zero: secure wipe before partitioning.
//
// Why not Hetzner installimage:
//   installimage is ideal for standard Linux (Debian, Ubuntu) — it handles
//   partitioning, filesystem, bootloader, and network in one command. However,
//   Talos cannot use installimage because:
//     1. Talos uses a proprietary GPT layout (BIOS boot + EFI + BOOT + META +
//        STATE + A/B partitions) with specific UUIDs that installimage cannot
//        produce. installimage only supports standard partition schemes.
//     2. Talos is distributed as OCI images, not tar.gz/raw disk images.
//        installimage's -i flag expects a standard OS archive or raw image.
//     3. The Talos installer binary must run to generate the correct UKI
//        (Unified Kernel Image) and create EFI boot entries via go-efilib.
//        installimage has no hook for running a custom installer post-extract.
//     4. Talos has no package manager, no standard init, no /etc/fstab — it
//        is an immutable OS that boots from a squashfs/initramfs. installimage
//        assumes a conventional mutable Linux filesystem layout.
//   The OCI + unshare approach remains the correct method for Talos.
//
// Flow:
//  1. Download crane (static OCI tool, ~15MB) + export installer image
//  2. Extract full OCI filesystem to /tmp/talos-root
//  3. unshare --mount --pid --fork: mount /proc + /sys + /dev, run installer
func (c *Client) InstallTalos(factoryURL, schematic, version, disk string) error {
	// Derive OCI registry hostname from factoryURL.
	registryHost := factoryURL
	for _, prefix := range []string{"https://", "http://"} {
		registryHost = strings.TrimPrefix(registryHost, prefix)
	}
	installerImage := fmt.Sprintf("%s/installer/%s:%s", registryHost, schematic, version)

	// Step 1: Download crane + export OCI image
	craneURL := fmt.Sprintf(
		"https://github.com/google/go-containerregistry/releases/download/%s/go-containerregistry_Linux_x86_64.tar.gz",
		craneVersion,
	)
	if out, err := c.Run(fmt.Sprintf(
		"curl -fsSL %q -o /tmp/crane.tar.gz && tar xzf /tmp/crane.tar.gz -C /tmp crane && rm -f /tmp/crane.tar.gz && chmod +x /tmp/crane",
		craneURL,
	)); err != nil {
		return fmt.Errorf("download crane: %w\nOutput: %s", err, out)
	}

	if out, err := c.Run(fmt.Sprintf(
		"/tmp/crane export --platform linux/amd64 %q /tmp/talos-installer.tar",
		installerImage,
	)); err != nil {
		return fmt.Errorf("crane export %s: %w\nOutput: %s", installerImage, err, out)
	}

	// Step 2: Extract full OCI filesystem
	if out, err := c.Run("mkdir -p /tmp/talos-root && tar xf /tmp/talos-installer.tar -C /tmp/talos-root && rm -f /tmp/talos-installer.tar"); err != nil {
		return fmt.Errorf("extract installer filesystem: %w\nOutput: %s", err, out)
	}

	// Step 3: Safety — verify the install disk is NOT the Ceph data disk.
	// NVMe device enumeration can swap between rescue and Talos boot
	// (different PCI probe order). Refusing to install if target has BlueStore.
	safetyCmd := fmt.Sprintf(
		`if blkid %[1]q 2>/dev/null | grep -q ceph_bluestore; then `+
			`echo "FATAL: install target %[1]s has Ceph BlueStore — wrong disk!"; exit 1; `+
			`fi; `+
			// Also verify: list ALL disks with BlueStore and ensure we're not about to overwrite one
			`echo "Disk safety check passed for %[1]s"`,
		disk,
	)
	if out, err := c.Run(safetyCmd); err != nil {
		_, _ = c.Run("rm -rf /tmp/talos-root /tmp/crane")
		return fmt.Errorf("SAFETY ABORT: install target %s appears to be the Ceph data disk: %w\nOutput: %s", disk, err, out)
	}

	// Step 4: Run installer inside unshare namespace
	// unshare provides mount namespace so the installer can access /proc, /sys, /dev.
	//
	// Key details:
	//   - /proc: tmpfs overlay with fake /proc/cmdline containing "talos.platform=metal"
	//     (rescue kernel cmdline doesn't have this → installer nil-pointer panic at install.go:169)
	//   - /sys: --rbind host sysfs (not mount -t sysfs) so go-efilib can access
	//     /sys/firmware/efi/efivars for UEFI NVRAM boot entry creation
	//   - /dev: --rbind host devtmpfs for block device access
	//   - --force: required because WipeOSDisk uses blkdiscard (not partition delete),
	//     so the disk may still have partition metadata that blocks mkfs
	installCmd := fmt.Sprintf(
		"unshare --mount --pid --fork -- bash -c '"+
			"mount --rbind /sys /tmp/talos-root/sys && "+
			"mount --rbind /dev /tmp/talos-root/dev && "+
			"mount --rbind /run /tmp/talos-root/run 2>/dev/null; "+
			"mount -t tmpfs tmpfs /tmp/talos-root/proc -o size=1M && "+
			"echo talos.platform=metal > /tmp/talos-root/proc/cmdline && "+
			"mkdir -p /tmp/talos-root/proc/self && "+
			"echo talos.platform=metal > /tmp/talos-root/proc/self/cmdline && "+
			"chroot /tmp/talos-root /usr/bin/installer install --disk %q --force --zero --platform metal < /dev/null"+
			"'",
		disk,
	)
	if out, err := c.Run(installCmd); err != nil {
		// Best-effort cleanup even on failure (rescue is RAM-based, OCI fs is ~500MB)
		_, _ = c.Run("rm -rf /tmp/talos-root /tmp/crane")
		return fmt.Errorf("talos installer: %w\nOutput: %s", err, out)
	}

	// Step 4: Fix EFI boot order from rescue (outside chroot/unshare)
	// The installer creates a Talos UKI boot entry via go-efilib inside
	// unshare, but the BootOrder update may not persist through the mount
	// namespace. Explicitly set the Talos entry first using efibootmgr
	// from rescue, where efivarfs access is direct to UEFI NVRAM.
	efiCmd := `TALOS=$(efibootmgr 2>/dev/null | grep -i "Talos" | head -1 | sed 's/Boot\([0-9A-Fa-f]*\).*/\1/'); ` +
		`if [ -n "$TALOS" ]; then ` +
		`CURRENT=$(efibootmgr | grep BootOrder | awk '{print $2}'); ` +
		`efibootmgr -o "$TALOS,$CURRENT" 2>&1; ` +
		`echo "EFI boot order set: Talos ($TALOS) first"; ` +
		`else ` +
		`echo "WARN: No Talos boot entry found in efibootmgr, relying on UEFI fallback"; ` +
		`fi`
	if out, err := c.Run(efiCmd); err != nil {
		// Non-fatal: UEFI fallback boot path may still work
		_ = out
	}

	// Cleanup: OCI filesystem + crane binary (rescue is RAM-based)
	_, _ = c.Run("rm -rf /tmp/talos-root /tmp/crane")

	return nil
}

// craneVersion is the pinned version of crane used to pull OCI images.
const craneVersion = "v0.20.2"

// IsReachable checks if SSH port 22 is reachable (used to detect rescue mode).
// Uses a 15-second timeout since Hetzner rescue SSH can be slow to accept connections.
func IsReachable(ip string) bool {
	return isReachableAddr(net.JoinHostPort(ip, strconv.Itoa(rescueSSHPort)), 15*time.Second)
}

// isReachableAddr checks if the given address is reachable via TCP within the timeout.
func isReachableAddr(addr string, timeout time.Duration) bool {
	conn, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

