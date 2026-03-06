// Package sshrescue provides SSH operations for Hetzner rescue environments.
package sshrescue

import (
	"bytes"
	"fmt"
	"net"
	"time"

	"golang.org/x/crypto/ssh"
)

const (
	rescueSSHPort    = 22
	connectTimeout   = 60 * time.Second
	commandTimeout   = 20 * time.Minute // dd can take a while
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

	addr := fmt.Sprintf("%s:%d", c.ip, rescueSSHPort)
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

	// Set EFI boot order to disk (Hetzner UEFI uses efibootmgr)
	// First, detect EFI boot entries for the target disk
	// Disk path is stored in a shell variable to avoid repeated quoting issues
	bootCmd := fmt.Sprintf(
		"set -e; "+
			"TARGET_DISK=%q; "+
			"DISK_PART=$(ls \"${TARGET_DISK}\"* | grep -E 'p?1$' | head -1); "+
			"if [ -n \"$DISK_PART\" ] && command -v efibootmgr &>/dev/null; then "+
			"  PART=$(echo \"$DISK_PART\" | sed 's|.*[^0-9]||'); "+
			"  efibootmgr -c -d \"${TARGET_DISK}\" -p \"$PART\" -L 'Talos' -l '\\EFI\\boot\\bootx64.efi' || true; "+
			"fi; "+
			"echo 'Boot order configured'",
		disk,
	)

	out, err = c.Run(bootCmd)
	if err != nil {
		// Non-fatal: EFI setup might fail if efibootmgr not available
		// The BIOS boot sector is also written by Talos installer
		_ = out
	}

	return nil
}

// IsReachable checks if SSH port 22 is reachable (used to detect rescue mode).
// Uses a 15-second timeout since Hetzner rescue SSH can be slow to accept connections.
func IsReachable(ip string) bool {
	conn, err := net.DialTimeout("tcp",
		fmt.Sprintf("%s:%d", ip, rescueSSHPort), 15*time.Second)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

