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
// InstallTalos installs Talos Linux on the server by writing a pre-built
// raw disk image from Talos Factory.
//
// Uses the raw image approach instead of the OCI installer binary because
// the installer binary (v1.12+) requires a full container environment
// (/proc, /sys, mount namespaces, /usr/install/ assets) that's impractical
// to replicate in a Hetzner rescue SSH session.
//
// The raw image includes the complete Talos GPT layout (EFI, BOOT, META,
// STATE, A, B) with kernel + initramfs. After dd, the node boots into
// Talos maintenance mode ready for machineconfig via talosctl apply-config.
//
// Flow:
//  1. Download raw disk image from Talos Factory (xz-compressed, ~500MB)
//  2. Decompress + dd to target disk (~4GB, takes ~4 seconds on NVMe)
//  3. Set up UEFI boot entry via efibootmgr (fallback: UEFI auto-discovery)
func (c *Client) InstallTalos(factoryURL, schematic, version, disk string) error {
	// Step 1: Download raw Talos disk image from Factory
	// Uses the pre-built raw image instead of OCI installer because the
	// installer binary requires a full container environment (/proc, /sys,
	// mount namespaces) that's impractical to set up in rescue SSH.
	// The raw image includes the full GPT partition layout (EFI, BOOT,
	// META, STATE, A, B) with kernel + initramfs pre-written.
	imageURL := fmt.Sprintf("%s/image/%s/%s/metal-amd64.raw.xz", factoryURL, schematic, version)
	if out, err := c.Run(fmt.Sprintf(
		"curl -fsSL %q -o /tmp/talos.raw.xz",
		imageURL,
	)); err != nil {
		return fmt.Errorf("download Talos image: %w\nOutput: %s", err, out)
	}

	// Step 2: Decompress and write to disk
	if out, err := c.Run(fmt.Sprintf(
		"xz -d /tmp/talos.raw.xz && dd if=/tmp/talos.raw of=%q bs=4M conv=fsync && rm -f /tmp/talos.raw",
		disk,
	)); err != nil {
		return fmt.Errorf("write Talos image to %s: %w\nOutput: %s", disk, err, out)
	}

	// Step 3: Set up UEFI boot entry via efibootmgr
	// The raw image has an EFI System Partition at partition 1 with
	// the Talos bootloader. Most UEFI firmware auto-discovers it via
	// the fallback path, but explicitly creating a boot entry is more reliable.
	efiCmd := fmt.Sprintf(
		"efibootmgr -c -d %q -p 1 -L Talos -l '\\EFI\\boot\\bootx64.efi' 2>&1 || echo 'efibootmgr not available, relying on UEFI fallback'",
		disk,
	)
	if out, err := c.Run(efiCmd); err != nil {
		// Non-fatal: UEFI fallback boot usually works
		_ = out
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

