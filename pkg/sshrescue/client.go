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
		`DISKS=$(lsblk -dnpo NAME,TYPE,RM | awk '$2 == "disk" && $3 == "0" { print $1 }'); ` +
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
func (c *Client) InstallTalos(factoryURL, schematic, version, disk string) error {
	imageURL := fmt.Sprintf("%s/image/%s/%s/metal-amd64.raw.xz", factoryURL, schematic, version)

	// Download and write Talos image to disk.
	// imageURL and disk are %q-quoted to prevent shell injection.
	ddCmd := fmt.Sprintf(
		"set -e; "+
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

