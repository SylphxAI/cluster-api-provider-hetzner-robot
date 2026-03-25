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

// WipeOSDisk wipes only the OS install disk, leaving all other disks untouched.
// During reprovision, Ceph OSD data on other disks (e.g. nvme1n1) must survive —
// wiping them would destroy storage cluster data and force a full rebuild.
// Only the OS disk needs a clean slate for Talos installation.
//
// Uses blkdiscard (fast TRIM) with dd fallback (zero first 2GB) to clear
// partition tables, filesystem metadata, and any leftover signatures.
func (c *Client) WipeOSDisk(disk string) (string, error) {
	cmd := fmt.Sprintf(
		`set -e; `+
			`echo "Wiping OS disk %[1]s..."; `+
			`if blkdiscard %[1]q 2>/dev/null; then `+
			`echo "  %[1]s wiped via blkdiscard (TRIM)"; `+
			`else `+
			`echo "  blkdiscard unavailable for %[1]s, falling back to dd zero (2GB)"; `+
			`dd if=/dev/zero of=%[1]q bs=1M count=2048 conv=notrunc 2>/dev/null; `+
			`fi; `+
			`sync; `+
			`echo "OS disk %[1]s wiped"`,
		disk,
	)

	return c.Run(cmd)
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

	// Step 3: Run installer inside unshare namespace
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

