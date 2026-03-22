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

	// Re-read partition table so kernel sees the new GPT written by dd.
	// Sleep 2s to let the kernel settle — some NVMe controllers are slow to
	// register new partition nodes after partprobe.
	if _, err := c.Run(fmt.Sprintf("partprobe %q 2>/dev/null || true; sleep 2; echo 'Partition table re-read'", disk)); err != nil {
		// partprobe failure is non-fatal — kernel may already have the table
		_ = err
	}

	// EFI boot entry setup — required on Hetzner AX bare metal.
	//
	// AX servers (AX162-R etc.) do NOT perform UEFI auto-discovery of new ESPs.
	// Their default boot order is PXE-first. Without an explicit UEFI NVRAM entry
	// for the Talos disk, the server will PXE-loop indefinitely after rescue exits,
	// because PXE fails (rescue inactive) and there is no disk fallback entry.
	//
	// The previous concern about "invalid entries causing hangs" was about stale
	// entries persisting across wipe cycles. We mitigate this by:
	//   1. Deleting ALL existing 'Talos' NVRAM entries before creating a new one
	//   2. Creating the entry AFTER dd so it always points to a valid GPT+ESP
	//
	// Boot order strategy: [Talos, ...existing entries (PXE preserved)]
	//   - Talos boots normally after install ✅
	//   - Hetzner rescue (PXE) still works for future re-provisions ✅
	//     (rescue activation forces PXE for one boot only, overriding NVRAM order)
	bootCmd := fmt.Sprintf(
		"set -euo pipefail; "+
			"TARGET_DISK=%q; "+
			"echo 'Configuring EFI boot entry for Talos...'; "+
			"if ! command -v efibootmgr &>/dev/null; then echo 'WARNING: efibootmgr not available, skipping'; exit 0; fi; "+
			// Find EFI partition device path via lsblk (handles nvme0n1p1 and sda1 naming)
			"EFI_PART_DEV=$(lsblk -lno NAME,FSTYPE \"${TARGET_DISK}\" 2>/dev/null | awk '$2==\"vfat\"{print \"/dev/\"$1}' | head -1 || true); "+
			"if [ -z \"$EFI_PART_DEV\" ]; then "+
			"  EFI_PART_DEV=$(blkid -o device \"${TARGET_DISK}\"* 2>/dev/null | while read dev; do blkid -s TYPE -o value \"$dev\" 2>/dev/null | grep -q vfat && echo \"$dev\"; done | head -1 || true); "+
			"fi; "+
			"if [ -z \"$EFI_PART_DEV\" ]; then "+
			"  echo 'WARNING: Could not detect EFI partition, using default ${TARGET_DISK}p1'; "+
			"  EFI_PART_DEV=\"${TARGET_DISK}p1\"; "+
			"fi; "+
			"EFI_PART_NUM=$(echo \"$EFI_PART_DEV\" | grep -oE '[0-9]+$'); "+
			"echo \"EFI partition: ${EFI_PART_DEV} (part ${EFI_PART_NUM})\"; "+
			// Mount EFI partition to find the Talos EFI binary path
			"mkdir -p /tmp/efi_mount; "+
			"EFI_LOADER='\\\\EFI\\\\boot\\\\bootx64.efi'; "+
			"if mount \"${EFI_PART_DEV}\" /tmp/efi_mount 2>/dev/null; then "+
			"  TALOS_EFI=$(find /tmp/efi_mount/EFI/Linux -maxdepth 1 -iname 'Talos-*.efi' 2>/dev/null | sort | tail -1); "+
			"  if [ -n \"$TALOS_EFI\" ]; then "+
			"    REL=${TALOS_EFI#/tmp/efi_mount}; "+
			"    EFI_LOADER=$(echo \"$REL\" | sed 's|/|\\\\\\\\|g'); "+
			"    echo \"Talos EFI binary: $TALOS_EFI -> $EFI_LOADER\"; "+
			"  else "+
			"    echo \"Talos-*.efi not found, using fallback: $EFI_LOADER\"; "+
			"  fi; "+
			"  umount /tmp/efi_mount; "+
			"else "+
			"  echo \"WARNING: Could not mount ${EFI_PART_DEV}, using fallback loader\"; "+
			"fi; "+
			// Preserve existing boot order (PXE entries etc.)
			"OLD_ORDER=$(efibootmgr | grep '^BootOrder:' | awk '{print $2}' | tr ',' ' '); "+
			// Remove stale Talos entries from previous provisions to avoid UEFI hangs
			"for NUM in $(efibootmgr | grep -i 'talos' | sed 's/Boot\\([0-9A-Fa-f]*\\).*/\\1/' || true); do "+
			"  echo \"Removing stale entry: Boot${NUM}\"; efibootmgr -b \"$NUM\" -B || true; "+
			"done; "+
			// Create new entry
			"echo \"Creating: disk=${TARGET_DISK} part=${EFI_PART_NUM} loader=${EFI_LOADER}\"; "+
			"CREATE_OUT=$(efibootmgr -c -d \"${TARGET_DISK}\" -p \"${EFI_PART_NUM}\" -L 'Talos' -l \"${EFI_LOADER}\" 2>&1); "+
			"echo \"efibootmgr output: $CREATE_OUT\"; "+
			"NEW_ENTRY=$(echo \"$CREATE_OUT\" | grep 'Boot[0-9A-Fa-f].*\\* Talos' | head -1 | sed 's/Boot\\([0-9A-Fa-f]*\\).*/\\1/'); "+
			"if [ -z \"$NEW_ENTRY\" ]; then "+
			"  echo 'ERROR: efibootmgr create failed — server will PXE-loop'; efibootmgr; exit 1; "+
			"fi; "+
			// Set boot order: Talos first, PXE preserved
			"NEW_ORDER=$(echo \"$NEW_ENTRY $(echo $OLD_ORDER | tr ' ' '\\n' | grep -iv \"$NEW_ENTRY\" | tr '\\n' ' ')\" | xargs | tr ' ' ','); "+
			"efibootmgr -o \"$NEW_ORDER\"; "+
			"echo \"SUCCESS: boot order=$NEW_ORDER\"; efibootmgr",
		disk,
	)

	if out, err := c.Run(bootCmd); err != nil {
		// EFI setup failure is a hard error: without a valid UEFI boot entry,
		// the server will PXE-loop and never reach Talos maintenance mode.
		return fmt.Errorf("EFI boot entry setup failed (server will PXE-loop): %w\nOutput:\n%s", err, out)
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

