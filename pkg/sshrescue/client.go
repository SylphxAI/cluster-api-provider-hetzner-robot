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

// HardwareInfo contains all hardware details collected from rescue in a single SSH call.
type HardwareInfo struct {
	PrimaryMAC string            // MAC of the default-route NIC
	GatewayIP  string            // Default gateway IP
	NVMeDisks  []string          // All NVMe whole-disk devices (e.g., /dev/nvme0n1)
	CephDisks  map[string]bool   // NVMe disks with ceph_bluestore signatures
	ByIDPaths  map[string]string // Maps bare device → /dev/disk/by-id/... stable path
}

// DetectHardware runs a single SSH command to collect all hardware details
// (MAC, gateway, NVMe disks, Ceph signatures, stable by-id paths) from the
// rescue environment. This replaces multiple sequential SSH calls with one
// round-trip, reducing rescue provisioning latency.
func (c *Client) DetectHardware() (*HardwareInfo, error) {
	cmd := `echo "MAC=$(ip link show $(ip route show default | awk '{print $5}') | grep ether | awk '{print $2}')"
echo "GATEWAY=$(ip route | grep default | awk '{print $3}' | head -1)"
for d in $(lsblk -dn -o NAME,TYPE | awk '$2=="disk" && $1~/^nvme/ {print "/dev/"$1}'); do
  echo "DISK=$d"
  if blkid "$d"* 2>/dev/null | grep -q ceph_bluestore; then
    echo "CEPH=$d"
  fi
done
for link in /dev/disk/by-id/nvme-*; do
  [ -L "$link" ] || continue
  case "$link" in *-part*) continue;; esac
  target=$(readlink -f "$link")
  echo "BYID=$target=$link"
done`

	out, err := c.Run(cmd)
	if err != nil {
		return nil, fmt.Errorf("detect hardware: %w\nOutput: %s", err, out)
	}

	return ParseHardwareOutput(out)
}

// ParseHardwareOutput parses the labeled output from DetectHardware into a
// HardwareInfo struct. Returns an error if MAC or GATEWAY is missing.
func ParseHardwareOutput(output string) (*HardwareInfo, error) {
	hw := &HardwareInfo{
		CephDisks: make(map[string]bool),
		ByIDPaths: make(map[string]string),
	}

	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		switch {
		case strings.HasPrefix(line, "MAC="):
			hw.PrimaryMAC = strings.TrimPrefix(line, "MAC=")

		case strings.HasPrefix(line, "GATEWAY="):
			hw.GatewayIP = strings.TrimPrefix(line, "GATEWAY=")

		case strings.HasPrefix(line, "DISK="):
			hw.NVMeDisks = append(hw.NVMeDisks, strings.TrimPrefix(line, "DISK="))

		case strings.HasPrefix(line, "CEPH="):
			hw.CephDisks[strings.TrimPrefix(line, "CEPH=")] = true

		case strings.HasPrefix(line, "BYID="):
			// Format: BYID=/dev/nvme0n1=/dev/disk/by-id/nvme-Samsung_SSD_990_PRO_S123
			rest := strings.TrimPrefix(line, "BYID=")
			parts := strings.SplitN(rest, "=", 2)
			if len(parts) == 2 {
				hw.ByIDPaths[parts[0]] = parts[1]
			}
		}
	}

	if hw.PrimaryMAC == "" {
		return nil, fmt.Errorf("parse hardware output: MAC address not found")
	}
	if hw.GatewayIP == "" {
		return nil, fmt.Errorf("parse hardware output: gateway IP not found")
	}

	return hw, nil
}

// ResolveInstallDiskFromInfo determines the correct install disk using
// pre-collected hardware info. This is a pure function — no SSH required.
//
// Logic:
//  1. Find NVMe disks that do NOT have Ceph BlueStore data
//  2. If no safe disks exist, return an error
//  3. If the configured disk is among the safe disks, prefer it
//  4. Otherwise, return the first safe disk
func ResolveInstallDiskFromInfo(hw *HardwareInfo, configuredDisk string) (string, error) {
	var safes []string
	for _, d := range hw.NVMeDisks {
		if !hw.CephDisks[d] {
			safes = append(safes, d)
		}
	}

	if len(safes) == 0 {
		if len(hw.NVMeDisks) == 0 {
			return configuredDisk, nil // no NVMe disks found — fall back to configured
		}
		return "", fmt.Errorf("all NVMe disks have Ceph BlueStore data — cannot determine install disk safely")
	}

	// Prefer the configured disk if it's in the safe list.
	for _, d := range safes {
		if d == configuredDisk {
			return configuredDisk, nil
		}
	}

	return safes[0], nil
}

// WipeAllDisks wipes all specified disks in parallel using a single SSH command.
// Each disk undergoes a thorough wipe: wipefs + sgdisk + dd + blkdiscard.
// blkdiscard alone is NOT enough — NVMe TRIM is advisory, the controller may
// delay data erasure. Talos STATE partition survives TRIM and boots with old config.
//
// The parallel approach (background subshells + wait) is significantly faster than
// sequential wiping when multiple disks are present, as each wipe is I/O-bound.
func (c *Client) WipeAllDisks(disks []string) (string, error) {
	if len(disks) == 0 {
		return "", fmt.Errorf("wipe all disks: no disks specified")
	}

	// Build the disk list for the shell for-loop.
	// Each disk path is shell-quoted to prevent injection.
	var quotedDisks []string
	for _, d := range disks {
		quotedDisks = append(quotedDisks, shellQuote(d))
	}

	cmd := fmt.Sprintf(
		`for d in %s; do `+
			`(wipefs -af "$d" 2>/dev/null; `+
			`sgdisk --zap-all "$d" 2>/dev/null; `+
			`dd if=/dev/zero of="$d" bs=1M count=100 conv=notrunc 2>/dev/null; `+
			`blkdiscard "$d" 2>/dev/null; `+
			`sync; `+
			`echo "WIPED=$d") & `+
			`done; `+
			`wait; `+
			`echo "ALL_DONE"`,
		strings.Join(quotedDisks, " "),
	)

	out, err := c.Run(cmd)
	if err != nil {
		return out, fmt.Errorf("wipe all disks: %w\nOutput: %s", err, out)
	}

	// Verify all disks were wiped by checking for WIPED= lines.
	wiped := make(map[string]bool)
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "WIPED=") {
			wiped[strings.TrimPrefix(line, "WIPED=")] = true
		}
	}

	var missing []string
	for _, d := range disks {
		if !wiped[d] {
			missing = append(missing, d)
		}
	}
	if len(missing) > 0 {
		return out, fmt.Errorf("wipe all disks: %d disk(s) not confirmed wiped: %s", len(missing), strings.Join(missing, ", "))
	}

	return out, nil
}

// InstallTalos installs Talos Linux by writing the factory raw disk image
// directly to the target disk. This is the fastest and most reliable method:
//
//	curl raw.xz | xzcat | dd of=/dev/nvme0n1
//
// The Talos Factory produces pre-built raw disk images with the correct GPT
// layout (BIOS boot + EFI + BOOT + META + STATE + A/B) and schematic-specific
// extensions baked in. No OCI export, no unshare/chroot, no crane dependency.
//
// EFI boot order is handled by the caller's post-install efibootmgr script
// (iterated 20+ times, battle-tested on Hetzner AX firmware). The OCI
// installer's go-efilib entries were always overwritten by efibootmgr anyway.
//
// Flow:
//  1. Download + write raw image via curl | xzcat | dd (single pipeline)
//  2. Caller handles EFI boot order via efibootmgr (not this function's job)
func (c *Client) InstallTalos(factoryURL, schematic, version, disk, customImageURL string) error {
	var imageURL, decompressCmd string

	if customImageURL != "" {
		// Custom image URL — supports both .zst and .xz
		imageURL = customImageURL
		if strings.HasSuffix(imageURL, ".zst") {
			decompressCmd = "zstdcat"
		} else {
			decompressCmd = "xzcat"
		}
	} else {
		// Talos Factory raw image URL:
		// https://factory.talos.dev/image/{schematic}/{version}/metal-amd64.raw.xz
		imageURL = fmt.Sprintf("%s/image/%s/%s/metal-amd64.raw.xz",
			strings.TrimRight(factoryURL, "/"), schematic, version)
		decompressCmd = "xzcat"
	}

	// Single pipeline: download compressed image → decompress → write to disk.
	// dd uses 4M block size for optimal NVMe throughput.
	// conv=notrunc prevents dd from truncating the device node.
	installCmd := fmt.Sprintf(
		"curl -fsSL %q | %s | dd of=%q bs=4M conv=notrunc status=progress 2>&1",
		imageURL, decompressCmd, disk,
	)
	if out, err := c.Run(installCmd); err != nil {
		return fmt.Errorf("write raw image to %s: %w\nOutput: %s", disk, err, out)
	}

	return nil
}

// InstallFlatcar installs Flatcar Container Linux using the official flatcar-install
// script. This is the ONLY reliable method on Hetzner bare metal — raw DD does not
// work because Hetzner UEFI firmware requires an explicit EFI boot entry created via
// efibootmgr, which flatcar-install handles with the -u flag.
//
// After flatcar-install, we add the r-dm-data partition for devmapper thin-pool.
// This must happen BEFORE first boot so Flatcar's auto-grow of ROOT stops at r-dm-data
// instead of filling the entire disk.
//
// Flow:
//  1. Download flatcar-install script + install gawk dependency
//  2. Write Ignition JSON to a temp file
//  3. Run flatcar-install -d <disk> -C <channel> -i <ignition> -u (creates EFI entry)
//  4. Add r-dm-data partition (200GB) at end of disk via sgdisk
//  5. Caller handles EFI boot order + reboot
func (c *Client) InstallFlatcar(channel, disk, customImageURL string, ignitionJSON []byte) error {
	if channel == "" {
		channel = "stable"
	}

	// Step 1: Download flatcar-install and install gawk (required dependency).
	setupCmd := `
		curl -fsSL -o /tmp/flatcar-install https://raw.githubusercontent.com/flatcar/init/flatcar-master/bin/flatcar-install && \
		chmod +x /tmp/flatcar-install && \
		apt-get install -y -qq gawk 2>&1 | tail -1 && \
		echo "SETUP_OK"
	`
	if out, err := c.Run(setupCmd); err != nil {
		return fmt.Errorf("setup flatcar-install: %w\nOutput: %s", err, out)
	}

	// Step 2: Write Ignition config to temp file (avoids shell escaping issues).
	writeIgnCmd := fmt.Sprintf(`cat > /tmp/flatcar-ignition.json << 'IGNEOF'
%s
IGNEOF
echo "IGN_OK"`, string(ignitionJSON))
	if out, err := c.Run(writeIgnCmd); err != nil {
		return fmt.Errorf("write Ignition temp file: %w\nOutput: %s", err, out)
	}

	// Step 3: Run flatcar-install with -u (UEFI boot entry creation).
	// -d: target disk
	// -C: release channel (stable/beta/alpha)
	// -i: Ignition config file (written to OEM partition)
	// -u: Create UEFI boot entry via efibootmgr — CRITICAL for Hetzner firmware
	installArgs := fmt.Sprintf("-d %s -C %s -i /tmp/flatcar-ignition.json -u", disk, channel)
	if customImageURL != "" {
		// Download custom image and use -f for local file install
		dlCmd := fmt.Sprintf(
			"curl -fsSL -o /tmp/flatcar-custom.bin.bz2 %q && echo DL_OK",
			customImageURL,
		)
		if out, err := c.Run(dlCmd); err != nil {
			return fmt.Errorf("download custom Flatcar image: %w\nOutput: %s", err, out)
		}
		installArgs = fmt.Sprintf("-d %s -f /tmp/flatcar-custom.bin.bz2 -i /tmp/flatcar-ignition.json -u", disk)
	}

	installCmd := fmt.Sprintf("/tmp/flatcar-install %s 2>&1", installArgs)
	if out, err := c.Run(installCmd); err != nil {
		return fmt.Errorf("flatcar-install on %s: %w\nOutput: %s", disk, err, out)
	}

	// Step 4: Add r-dm-data partition (200GB) at END of disk for devmapper thin-pool.
	// flatcar-install writes the stock image with ROOT (p9) as the last partition.
	// We expand GPT and add p10 at the very end. On first boot, Flatcar auto-grows
	// ROOT to fill space BEFORE p10 (not the whole disk).
	createPartCmd := fmt.Sprintf(`
		sgdisk -e %q 2>&1 && \
		DISK_END=$(sgdisk -p %q | grep "last usable sector" | awk '{print $NF}') && \
		SIZE_SECTORS=$((200 * 1024 * 1024 * 1024 / 512)) && \
		START=$((DISK_END - SIZE_SECTORS + 1)) && \
		sgdisk -n 10:${START}:${DISK_END} -t 10:8300 -c 10:r-dm-data %q 2>&1 && \
		partprobe %q 2>&1 && sleep 1 && \
		echo "PARTITION_CREATED"
	`, disk, disk, disk, disk)
	if out, err := c.Run(createPartCmd); err != nil {
		return fmt.Errorf("create r-dm-data partition on %s: %w\nOutput: %s", disk, err, out)
	}

	return nil
}

// IsFlatcarUp checks if a Flatcar node is booted and SSH-accessible as the `core` user.
// Returns true if SSH port 22 is reachable AND authentication as `core` succeeds.
// This distinguishes Flatcar (user=core) from rescue (user=root) and Talos (no SSH).
func IsFlatcarUp(ip string, privateKey []byte) bool {
	signer, err := ssh.ParsePrivateKey(privateKey)
	if err != nil {
		return false
	}
	config := &ssh.ClientConfig{
		User: "core",
		Auth: []ssh.AuthMethod{
			ssh.PublicKeys(signer),
		},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), //nolint:gosec // known host
		Timeout:         15 * time.Second,
	}
	addr := net.JoinHostPort(ip, strconv.Itoa(rescueSSHPort))
	conn, err := net.DialTimeout("tcp", addr, 15*time.Second)
	if err != nil {
		return false
	}
	sshConn, chans, reqs, err := ssh.NewClientConn(conn, addr, config)
	if err != nil {
		conn.Close()
		return false
	}
	client := ssh.NewClient(sshConn, chans, reqs)
	client.Close()
	return true
}

// CheckFlatcarBootstrapComplete SSHes into a Flatcar node as `core` and checks
// whether the CAPI bootstrap sentinel file exists, indicating kubeadm join succeeded.
func CheckFlatcarBootstrapComplete(ip string, privateKey []byte) (bool, error) {
	signer, err := ssh.ParsePrivateKey(privateKey)
	if err != nil {
		return false, fmt.Errorf("parse SSH key: %w", err)
	}
	config := &ssh.ClientConfig{
		User: "core",
		Auth: []ssh.AuthMethod{
			ssh.PublicKeys(signer),
		},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), //nolint:gosec // known host
		Timeout:         connectTimeout,
	}
	addr := net.JoinHostPort(ip, strconv.Itoa(rescueSSHPort))
	conn, err := net.DialTimeout("tcp", addr, connectTimeout)
	if err != nil {
		return false, fmt.Errorf("TCP connect to %s: %w", addr, err)
	}
	sshConn, chans, reqs, err := ssh.NewClientConn(conn, addr, config)
	if err != nil {
		conn.Close()
		return false, fmt.Errorf("SSH handshake to %s: %w", addr, err)
	}
	sshClient := ssh.NewClient(sshConn, chans, reqs)
	defer sshClient.Close()

	sess, err := sshClient.NewSession()
	if err != nil {
		return false, fmt.Errorf("new session: %w", err)
	}
	defer sess.Close()

	var buf bytes.Buffer
	sess.Stdout = &buf
	sess.Stderr = &buf

	// Check sentinel file created by CABPK after successful kubeadm join.
	if err := sess.Run("test -f /run/cluster-api/bootstrap-success.complete && echo READY || echo WAITING"); err != nil {
		return false, fmt.Errorf("check bootstrap sentinel: %w\nOutput: %s", err, buf.String())
	}

	return strings.TrimSpace(buf.String()) == "READY", nil
}

// IsReachable checks if SSH port 22 is reachable (used to detect rescue mode).
// Uses a 15-second timeout since Hetzner rescue SSH can be slow to accept connections.
func IsReachable(ip string) bool {
	return isReachableAddr(net.JoinHostPort(ip, strconv.Itoa(rescueSSHPort)), 15*time.Second)
}

// IsRescueMode SSHes into the server and determines whether it is running in
// Hetzner rescue mode or a normal OS (Debian, Talos, etc.).
//
// Detection strategy (any match = rescue):
//  1. Root filesystem is tmpfs — rescue runs entirely in RAM.
//  2. /etc/motd contains "Hetzner Rescue" — Hetzner sets this in every rescue image.
//  3. Hostname is "rescue" — Hetzner rescue default hostname.
//
// The tmpfs check is the most reliable: no normal OS uses tmpfs as root.
// The other checks are fallbacks for edge cases (e.g., custom rescue images).
//
// Returns (true, nil) if rescue is confirmed, (false, nil) if a normal OS is
// detected, or (false, err) if the SSH connection or command fails.
func IsRescueMode(ip string, privateKey []byte) (bool, error) {
	client := New(ip, privateKey)
	if err := client.Connect(); err != nil {
		return false, fmt.Errorf("SSH connect for rescue check on %s: %w", ip, err)
	}
	defer client.Close()

	// Single command checks all three indicators and prints a deterministic result.
	// Any one match is sufficient to confirm rescue mode.
	out, err := client.Run(
		`if mount | grep 'on / ' | grep -q tmpfs; then echo RESCUE; ` +
			`elif grep -qi 'Hetzner Rescue' /etc/motd 2>/dev/null; then echo RESCUE; ` +
			`elif [ "$(hostname)" = "rescue" ]; then echo RESCUE; ` +
			`else echo NORMAL; fi`,
	)
	if err != nil {
		return false, fmt.Errorf("rescue mode detection on %s: %w (output: %s)", ip, err, out)
	}

	return strings.TrimSpace(out) == "RESCUE", nil
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

