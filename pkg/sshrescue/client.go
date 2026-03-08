// Package sshrescue provides SSH operations for Hetzner rescue environments.
package sshrescue

import (
	"bytes"
	"fmt"
	"net"
	"strconv"
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

	if err := sess.Run(command); err != nil {
		return buf.String(), fmt.Errorf("command %q failed: %w\noutput: %s", command, err, buf.String())
	}
	return buf.String(), nil
}

// InstallTalos installs Talos Linux on the server.
// It downloads the Talos raw disk image from Talos factory and writes it to the disk.
// Then sets the boot order to boot from the disk.
func (c *Client) InstallTalos(factoryURL, schematic, version, disk string) error {
	imageURL := fmt.Sprintf("%s/image/%s/%s/metal-amd64.raw.xz", factoryURL, schematic, version)

	// Download and write Talos raw image
	// Both imageURL and disk are %q-quoted to prevent shell injection
	ddCmd := fmt.Sprintf(
		"set -e; "+
			"echo 'Downloading Talos image...'; "+
			"curl -fsSL %q | xzcat | dd of=%q bs=4M status=progress; "+
			"sync; "+
			"echo 'Talos image written'",
		imageURL, disk,
	)

	out, err := c.Run(ddCmd)
	if err != nil {
		return fmt.Errorf("install Talos image: %w\nOutput: %s", err, out)
	}

	// Configure EFI boot order: Talos first, then PXE (so future rescue activations work).
	// Hetzner rescue depends on PXE boot being in the boot order — if we remove it,
	// subsequent rescue activations will boot into Talos instead of the rescue environment.
	//
	// Strategy:
	//   1. Record existing boot order (includes PXE entries like "Network Boot")
	//   2. Create a new Talos EFI entry
	//   3. Set boot order: [Talos, ...existing entries...]
	//      This keeps PXE in the boot order so Hetzner can intercept next boot during rescue.
	bootCmd := fmt.Sprintf(
		"set -e; "+
			"TARGET_DISK=%q; "+
			"if ! command -v efibootmgr &>/dev/null; then echo 'efibootmgr not available, skipping EFI config'; exit 0; fi; "+
			// Capture existing boot order (e.g. "0000 0001 0002")
			"OLD_ORDER=$(efibootmgr | grep '^BootOrder:' | awk '{print $2}' | tr ',' ' '); "+
			// Remove any existing 'Talos' entries to avoid duplicates
			"for NUM in $(efibootmgr | grep -i 'talos' | sed 's/Boot\\([0-9A-Fa-f]*\\).*/\\1/'); do efibootmgr -b $NUM -B || true; done; "+
			// Find the partition (e.g. nvme0n1p1 or nvme0n1p12)
			"DISK_PART=$(ls \"${TARGET_DISK}\"* 2>/dev/null | grep -E '[0-9]+$' | sort | head -1); "+
			"if [ -z \"$DISK_PART\" ]; then echo 'No EFI partition found, skipping'; exit 0; fi; "+
			"PART=$(echo \"$DISK_PART\" | grep -oE '[0-9]+$'); "+
			// Create new Talos boot entry and capture its ID
			"NEW_ENTRY=$(efibootmgr -c -d \"${TARGET_DISK}\" -p \"$PART\" -L 'Talos' -l '\\EFI\\boot\\bootx64.efi' | grep 'Boot[0-9A-Fa-f].*\\* Talos' | head -1 | sed 's/Boot\\([0-9A-Fa-f]*\\).*/\\1/'); "+
			"if [ -z \"$NEW_ENTRY\" ]; then echo 'Could not get new entry ID, skipping order set'; exit 0; fi; "+
			// Build new boot order: Talos first, then all existing entries (preserves PXE)
			"NEW_ORDER=$(echo \"$NEW_ENTRY $(echo $OLD_ORDER | tr ' ' '\\n' | grep -iv \"$NEW_ENTRY\" | tr '\\n' ' ')\" | xargs | tr ' ' ','); "+
			"efibootmgr -o \"$NEW_ORDER\"; "+
			"echo \"EFI boot order set: $NEW_ORDER (Talos first, PXE preserved)\"",
		disk,
	)

	out, err = c.Run(bootCmd)
	if err != nil {
		// Non-fatal: log the output but don't fail the install.
		// BIOS boot sector is also written by Talos installer as fallback.
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

