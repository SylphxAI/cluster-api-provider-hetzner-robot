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

// WipeAllDisks discovers all non-removable block devices and wipes them.
// This ensures a clean slate for provisioning — no leftover partition tables,
// BlueStore labels, or filesystem metadata from previous installations.
// Bare metal provisioning = every disk must be clean. Rook-Ceph, for example,
// refuses to provision OSDs on disks with existing BlueStore signatures.
// Without this, every new node requires manual `blkdiscard` — unacceptable.
func (c *Client) WipeAllDisks() (string, error) {
	// Discover all non-removable, non-loop block devices (whole disks only).
	// lsblk -dnpo NAME,TYPE,RM outputs: /dev/nvme0n1 disk 0
	// Filters: TYPE=disk (no partitions), RM=0 (not removable/USB).
	cmd := `set -e; ` +
		`echo 'Discovering block devices...'; ` +
		`lsblk -dnpo NAME,TYPE,RM > /tmp/lsblk.out; ` +
		`DISKS=$(awk '$2 == "disk" && $3 == "0" { print $1 }' /tmp/lsblk.out); ` +
		`rm -f /tmp/lsblk.out; ` +
		`if [ -z "$DISKS" ]; then echo 'No disks found to wipe'; exit 0; fi; ` +
		`for disk in $DISKS; do ` +
		`echo "Wiping $disk..."; ` +
		`if blkdiscard "$disk" 2>/dev/null; then ` +
		`echo "  $disk wiped via blkdiscard (TRIM)"; ` +
		`else ` +
		`echo "  blkdiscard unavailable for $disk, falling back to dd zero (2GB)"; ` +
		`dd if=/dev/zero of="$disk" bs=1M count=2048 conv=notrunc 2>/dev/null; ` +
		`fi; ` +
		`done; ` +
		`sync; ` +
		`echo "All disks wiped"`

	return c.Run(cmd)
}

// InstallTalos installs Talos Linux on the server.
// It downloads the Talos raw disk image from Talos factory and writes it to the disk.
// Then sets the boot order to boot from the disk.
//
// Caller must call WipeAllDisks() before this — InstallTalos only writes the image,
// it does not wipe. Separation of concerns: wiping is a provisioning-level decision
// (wipe everything), writing is install-level (target one specific disk).
// craneVersion is the pinned version of crane used to extract OCI images.
// Update periodically to pick up security fixes.
const craneVersion = "v0.20.2"

// InstallTalos installs Talos Linux on the server using the official Talos OCI installer.
//
// This is the correct, production-grade approach for bare metal Talos provisioning.
// It uses the Talos installer binary (extracted from the Talos Factory OCI image)
// rather than dd'ing a raw disk image followed by shell-based EFI manipulation.
//
// Why OCI installer > dd + efibootmgr:
//
//   - EFI correctness: Talos' own Go bootloader code handles UEFI NVRAM entries
//     using go-efilib, which is type-safe and handles all UEFI firmware quirks
//     (including Hetzner AX servers that don't do auto-discovery).
//
//   - Partition layout: installer creates the canonical Talos GPT layout
//     (BIOS boot + EFI System + BOOT + META + STATE + A + B) with correct UUIDs.
//
//   - Disk zeroing: --zero flag handles secure wipe before partitioning.
//
//   - Maintenance mode: installer leaves the disk in a state where Talos
//     boots directly into maintenance mode on first start, ready for
//     machineconfig via talosctl apply-config --insecure.
//
//   - Future-proof: EFI handling and partition layout improvements in future
//     Talos releases are automatically inherited without CAPHR code changes.
//
// Flow:
//  1. Download crane (static OCI tool binary, ~15 MB)
//  2. Export the Talos Factory installer OCI image as a tar archive
//  3. Extract the `installer` binary from the tar
//  4. Run: installer install --disk <disk> --zero --platform metal
//
// The installer reads machine config from stdin; passing /dev/null skips
// config validation (allowed — node will be in maintenance mode on first boot).
func (c *Client) InstallTalos(factoryURL, schematic, version, disk string) error {
	// Derive the OCI registry hostname from factoryURL.
	// https://factory.talos.dev → factory.talos.dev
	registryHost := factoryURL
	for _, prefix := range []string{"https://", "http://"} {
		registryHost = strings.TrimPrefix(registryHost, prefix)
	}
	// OCI image: factory.talos.dev/installer/<schematic>:<version>
	installerImage := fmt.Sprintf("%s/installer/%s:%s", registryHost, schematic, version)

	// All values are %q-quoted to prevent shell injection.
	installCmd := fmt.Sprintf(
		"set -eu; "+ // dash (rescue /bin/sh) does not support pipefail

			// ── Step 1: Download crane ────────────────────────────────────────────
			// crane is a small (~15 MB) static Go binary for working with OCI images.
			// We use it to pull and export the Talos installer OCI image without
			// needing Docker/podman in rescue mode.
			"echo '=== Step 1: Downloading crane OCI tool ==='; "+
			"CRANE_URL=\"https://github.com/google/go-containerregistry/releases/download/%s/go-containerregistry_Linux_x86_64.tar.gz\"; "+
			"curl -fsSL \"${CRANE_URL}\" -o /tmp/crane.tar.gz; "+
			"tar xz -C /tmp crane -f /tmp/crane.tar.gz; "+
			"rm -f /tmp/crane.tar.gz; "+
			"chmod +x /tmp/crane; "+
			"/tmp/crane version; "+

			// ── Step 2: Export installer OCI image ───────────────────────────────
			// crane export streams the image layers into a single tar archive.
			// The archive contains the installer binary at /installer (static binary).
			"echo '=== Step 2: Extracting Talos installer from OCI image ==='; "+
			"echo 'Image: %s'; "+
			"/tmp/crane export --platform linux/amd64 %q /tmp/talos-installer.tar 2>&1; "+
			"tar xOf /tmp/talos-installer.tar installer > /tmp/talos-installer; "+
			"chmod +x /tmp/talos-installer; "+
			"echo 'Talos installer binary extracted successfully'; "+
			"rm -f /tmp/talos-installer.tar; "+ // free disk space

			// ── Step 3: Run Talos installer ──────────────────────────────────────
			// installer install handles:
			//   • Partition table creation (GPT: BIOS boot, EFI, BOOT, META, STATE, A, B)
			//   • Kernel + initramfs writing to A partition
			//   • EFI boot entry registration via Go go-efilib (UEFI NVRAM)
			//   • --zero: zeroes disk before partitioning (belt-and-suspenders
			//     after WipeAllDisks, ensures no stale BlueStore/filesystem sigs)
			//   • stdin /dev/null: no machine config at install time; node boots
			//     into Talos maintenance mode where CAPHR will apply machineconfig
			"echo '=== Step 3: Running Talos installer ==='; "+
			"echo 'Disk: %s | Platform: metal | Zero: yes'; "+
			"/tmp/talos-installer install "+
			"  --disk %q "+
			"  --zero "+
			"  --platform metal "+
			"  < /dev/null; "+
			"echo '=== Talos installation complete ==='",

		craneVersion,    // crane release version
		installerImage,  // log: OCI image being pulled
		installerImage,  // crane export argument
		disk,            // log: target disk
		disk,            // --disk argument
	)

	out, err := c.Run(installCmd)
	if err != nil {
		return fmt.Errorf("Talos OCI installer failed on %s: %w\nOutput:\n%s", disk, err, out)
	}

	return nil
}

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

