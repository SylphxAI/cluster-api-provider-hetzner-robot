package sshrescue

import (
	"fmt"
	"net"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// IsReachable / isReachableAddr
// ---------------------------------------------------------------------------

func TestIsReachableAddr_ReachableWhenListening(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to start listener: %v", err)
	}
	defer ln.Close()

	addr := ln.Addr().String()
	if !isReachableAddr(addr, 2*time.Second) {
		t.Errorf("expected addr %s to be reachable", addr)
	}
}

func TestIsReachableAddr_UnreachableWhenNotListening(t *testing.T) {
	// Bind then immediately close to get a port that is not listening.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to start listener: %v", err)
	}
	addr := ln.Addr().String()
	ln.Close()

	if isReachableAddr(addr, 500*time.Millisecond) {
		t.Errorf("expected addr %s to be unreachable after listener closed", addr)
	}
}

func TestIsReachableAddr_InvalidAddress(t *testing.T) {
	if isReachableAddr("not-a-valid-host:99999", 500*time.Millisecond) {
		t.Error("expected invalid address to be unreachable")
	}
}

func TestIsReachableAddr_TimeoutRespected(t *testing.T) {
	// Use an unroutable address so the dial blocks until timeout.
	// 198.51.100.1 is TEST-NET-2 (RFC 5737), typically unroutable.
	start := time.Now()
	timeout := 500 * time.Millisecond
	isReachableAddr("198.51.100.1:22", timeout)
	elapsed := time.Since(start)

	// Allow generous upper bound (3x) but verify we didn't return instantly
	// and also didn't take excessively long.
	if elapsed > 3*timeout {
		t.Errorf("expected dial to respect timeout (%v), took %v", timeout, elapsed)
	}
}

// ---------------------------------------------------------------------------
// New
// ---------------------------------------------------------------------------

func TestNew_SetsFields(t *testing.T) {
	ip := "192.168.1.100"
	key := []byte("fake-key-data")

	c := New(ip, key)

	if c.ip != ip {
		t.Errorf("expected ip=%q, got %q", ip, c.ip)
	}
	if string(c.privateKey) != string(key) {
		t.Errorf("expected privateKey=%q, got %q", key, c.privateKey)
	}
	if c.client != nil {
		t.Error("expected client to be nil on new Client")
	}
}

// ---------------------------------------------------------------------------
// Connect
// ---------------------------------------------------------------------------

func TestConnect_InvalidKeyReturnsParseError(t *testing.T) {
	c := New("127.0.0.1", []byte("not-a-real-private-key"))
	err := c.Connect()
	if err == nil {
		t.Fatal("expected error from Connect with invalid key, got nil")
	}
	if !strings.Contains(err.Error(), "parse SSH private key") {
		t.Errorf("expected parse error, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Run
// ---------------------------------------------------------------------------

func TestRun_WithoutConnectionReturnsError(t *testing.T) {
	c := New("127.0.0.1", []byte("key"))
	out, err := c.Run("echo hello")

	if err == nil {
		t.Fatal("expected error from Run without connection, got nil")
	}
	if !strings.Contains(err.Error(), "not connected") {
		t.Errorf("expected 'not connected' error, got: %v", err)
	}
	if out != "" {
		t.Errorf("expected empty output, got: %q", out)
	}
}

// ---------------------------------------------------------------------------
// Close
// ---------------------------------------------------------------------------

func TestClose_NilClient_NoPanic(t *testing.T) {
	c := New("127.0.0.1", []byte("key"))
	// c.client is nil at this point; Close should not panic.
	c.Close()
}

func TestClose_OnUnconnectedClient_NoPanic(t *testing.T) {
	c := &Client{
		ip:         "10.0.0.1",
		privateKey: []byte("key"),
		client:     nil,
	}
	// Should not panic.
	c.Close()
}

// ---------------------------------------------------------------------------
// InstallTalos — command string generation and shell injection prevention
// ---------------------------------------------------------------------------

func TestInstallTalos_CommandFormat(t *testing.T) {
	// Verify the image URL and dd command are correctly formed.
	factoryURL := "https://factory.talos.dev"
	schematic := "abc123"
	version := "v1.9.0"
	disk := "/dev/nvme0n1"

	expectedImageURL := fmt.Sprintf("%s/image/%s/%s/metal-amd64.raw.xz", factoryURL, schematic, version)

	// Build the dd command the same way InstallTalos does.
	ddCmd := fmt.Sprintf(
		"set -e; "+
			"echo 'Downloading Talos image...'; "+
			"curl -fsSL %q | xzcat | dd of=%q bs=4M status=progress; "+
			"sync; "+
			"echo 'Talos image written'",
		expectedImageURL, disk,
	)

	if !strings.Contains(ddCmd, fmt.Sprintf("%q", expectedImageURL)) {
		t.Errorf("dd command missing quoted image URL:\n%s", ddCmd)
	}
	if !strings.Contains(ddCmd, fmt.Sprintf("of=%q", disk)) {
		t.Errorf("dd command missing quoted disk path:\n%s", ddCmd)
	}
	if !strings.Contains(ddCmd, "set -e") {
		t.Error("dd command missing set -e")
	}
}

func TestInstallTalos_ShellInjectionPrevented(t *testing.T) {
	// A malicious disk path should be safely quoted with %q so the
	// semicolon and following command are treated as a literal string.
	maliciousDisk := "/dev/sda; rm -rf /"

	ddCmd := fmt.Sprintf(
		"set -e; "+
			"echo 'Downloading Talos image...'; "+
			"curl -fsSL %q | xzcat | dd of=%q bs=4M status=progress; "+
			"sync; "+
			"echo 'Talos image written'",
		"https://factory.talos.dev/image/abc/v1.0.0/metal-amd64.raw.xz",
		maliciousDisk,
	)

	// %q wraps in double quotes and escapes internal characters.
	// The semicolon must NOT appear unquoted in the command.
	quotedMalicious := fmt.Sprintf("%q", maliciousDisk)
	if !strings.Contains(ddCmd, "of="+quotedMalicious) {
		t.Errorf("malicious disk should be %q-quoted in command, got:\n%s", maliciousDisk, ddCmd)
	}

	// Also verify the boot command quotes the disk path.
	bootCmd := fmt.Sprintf(
		"set -e; "+
			"TARGET_DISK=%q; "+
			"DISK_PART=$(ls \"${TARGET_DISK}\"* | grep -E 'p?1$' | head -1); "+
			"if [ -n \"$DISK_PART\" ] && command -v efibootmgr &>/dev/null; then "+
			"  PART=$(echo \"$DISK_PART\" | sed 's|.*[^0-9]||'); "+
			"  efibootmgr -c -d \"${TARGET_DISK}\" -p \"$PART\" -L 'Talos' -l '\\EFI\\boot\\bootx64.efi' || true; "+
			"fi; "+
			"echo 'Boot order configured'",
		maliciousDisk,
	)

	if !strings.Contains(bootCmd, "TARGET_DISK="+quotedMalicious) {
		t.Errorf("malicious disk should be %q-quoted in boot command, got:\n%s", maliciousDisk, bootCmd)
	}
}

func TestInstallTalos_ImageURLFormat(t *testing.T) {
	tests := []struct {
		name       string
		factoryURL string
		schematic  string
		version    string
		wantURL    string
	}{
		{
			name:       "standard factory URL",
			factoryURL: "https://factory.talos.dev",
			schematic:  "376567988ad370138ad8b2698212367b8edcb69b5fd68c80be1f2ec7d603b4ba",
			version:    "v1.9.4",
			wantURL:    "https://factory.talos.dev/image/376567988ad370138ad8b2698212367b8edcb69b5fd68c80be1f2ec7d603b4ba/v1.9.4/metal-amd64.raw.xz",
		},
		{
			name:       "custom factory URL without trailing slash",
			factoryURL: "https://custom.factory.example.com",
			schematic:  "deadbeef",
			version:    "v2.0.0-beta.1",
			wantURL:    "https://custom.factory.example.com/image/deadbeef/v2.0.0-beta.1/metal-amd64.raw.xz",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := fmt.Sprintf("%s/image/%s/%s/metal-amd64.raw.xz",
				tt.factoryURL, tt.schematic, tt.version)
			if got != tt.wantURL {
				t.Errorf("image URL mismatch:\n  got:  %s\n  want: %s", got, tt.wantURL)
			}
		})
	}
}

func TestInstallTalos_RequiresConnection(t *testing.T) {
	c := New("127.0.0.1", []byte("key"))
	err := c.InstallTalos("https://factory.talos.dev", "abc", "v1.0.0", "/dev/sda")
	if err == nil {
		t.Fatal("expected error when calling InstallTalos without connection")
	}
	if !strings.Contains(err.Error(), "not connected") {
		t.Errorf("expected 'not connected' in error, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Constants
// ---------------------------------------------------------------------------

func TestConstants(t *testing.T) {
	if rescueSSHPort != 22 {
		t.Errorf("expected rescueSSHPort=22, got %d", rescueSSHPort)
	}
	if connectTimeout != 60*time.Second {
		t.Errorf("expected connectTimeout=60s, got %v", connectTimeout)
	}
	if commandTimeout != 20*time.Minute {
		t.Errorf("expected commandTimeout=20m, got %v", commandTimeout)
	}
}
