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

	// Wipe existing partition table and Talos STATE partition before writing.
	// Without this, a previous Talos install's STATE partition (containing the old
	// machineconfig) may survive the dd and cause the new Talos to boot in full mode
	// instead of maintenance mode.
	//
	// Uses blkdiscard (NVMe TRIM) to fully erase the disk — fast, complete, and the
	// only reliable method for NVMe. BlueStore (Ceph) writes labels at 1GB offset
	// (0x40000000) which survives standard dd wipes. blkdiscard erases all blocks
	// at firmware level. Falls back to 2GB dd zero for non-NVMe/non-TRIM disks.
	//
	// Both imageURL and disk are %q-quoted to prevent shell injection
	ddCmd := fmt.Sprintf(
		"set -e; "+
			"echo 'Wiping disk...'; "+
			"if blkdiscard %[2]q 2>/dev/null; then "+
			"echo 'Disk wiped via blkdiscard (TRIM)'; "+
			"else "+
			"echo 'blkdiscard unavailable, falling back to dd zero (2GB)'; "+
			"dd if=/dev/zero of=%[2]q bs=1M count=2048 conv=notrunc 2>/dev/null; "+
			"fi; "+
			"sync; "+
			"echo 'Downloading Talos image...'; "+
			"curl -fsSL %[1]q | xzcat | dd of=%[2]q bs=4M status=progress; "+
			"sync; "+
			"echo 'Talos image written'",
		imageURL, disk,
	)

	out, err := c.Run(ddCmd)
	if err != nil {
		return fmt.Errorf("install Talos image: %w\nOutput: %s", err, out)
	}

	// EFI boot configuration.
	//
	// Do NOT use efibootmgr to manipulate boot order. The previous approach of
	// creating custom boot entries and reordering caused servers to become
	// completely unbootable when the entry references became invalid (e.g., after
	// blkdiscard wiped the GPT that the entry pointed to). UEFI hangs on invalid
	// entries instead of falling through.
	//
	// Instead, rely on two mechanisms:
	//   1. UEFI auto-discovery: UEFI firmware automatically finds and boots
	//      /EFI/boot/bootx64.efi on any ESP. Talos' dd image includes this.
	//   2. BMC PXE override: Hetzner's BMC forces one-shot PXE boot when rescue
	//      is activated, bypassing UEFI boot order entirely. The controller
	//      deactivates rescue before rebooting after install, so PXE doesn't
	//      intercept the Talos boot.
	//
	// This is simpler and more robust than efibootmgr manipulation.
	// Re-read partition table so UEFI sees the new GPT from the dd image.
	out, _ = c.Run(fmt.Sprintf("partprobe %s 2>/dev/null; echo 'Partition table refreshed'", disk))

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

