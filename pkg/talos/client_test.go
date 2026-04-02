package talos

import (
	"context"
	"fmt"
	"net"
	"testing"
	"time"
)

// ─── isAlreadyBootstrapped tests ─────────────────────────────────────────────

func TestIsAlreadyBootstrapped_AlreadyBootstrapped(t *testing.T) {
	err := fmt.Errorf("rpc error: code = FailedPrecondition desc = cluster is already bootstrapped")
	if !isAlreadyBootstrapped(err) {
		t.Error("expected true for 'already bootstrapped' error")
	}
}

func TestIsAlreadyBootstrapped_AlreadyExists(t *testing.T) {
	err := fmt.Errorf("rpc error: code = AlreadyExists desc = etcd member already exists")
	if !isAlreadyBootstrapped(err) {
		t.Error("expected true for 'AlreadyExists' error")
	}
}

func TestIsAlreadyBootstrapped_EtcdAlreadyRunning(t *testing.T) {
	err := fmt.Errorf("etcd is already running on this node")
	if !isAlreadyBootstrapped(err) {
		t.Error("expected true for 'etcd is already running' error")
	}
}

func TestIsAlreadyBootstrapped_NilError(t *testing.T) {
	if isAlreadyBootstrapped(nil) {
		t.Error("expected false for nil error")
	}
}

func TestIsAlreadyBootstrapped_UnrelatedError(t *testing.T) {
	cases := []error{
		fmt.Errorf("connection refused"),
		fmt.Errorf("deadline exceeded"),
		fmt.Errorf("permission denied"),
		fmt.Errorf("rpc error: code = Unavailable desc = transport is closing"),
	}
	for _, err := range cases {
		if isAlreadyBootstrapped(err) {
			t.Errorf("expected false for unrelated error %q", err)
		}
	}
}

// ─── IsInMaintenanceMode tests ───────────────────────────────────────────────

func TestIsInMaintenanceMode_ListenerActive(t *testing.T) {
	// Start a TCP listener on a random port, then reconfigure to use port 50000 format
	// We use a listener on a random port and test tcpReachable directly
	// since IsInMaintenanceMode hardcodes port 50000.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("start listener: %v", err)
	}
	defer ln.Close()

	// Extract the port the OS assigned
	port := ln.Addr().(*net.TCPAddr).Port

	ctx := context.Background()
	if !tcpReachable(ctx, "127.0.0.1", port) {
		t.Error("expected tcpReachable=true with active listener")
	}
}

func TestIsInMaintenanceMode_NoListener(t *testing.T) {
	// Find a port that is definitely not listening by binding, getting the port,
	// then closing immediately
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("start listener: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close() // close immediately so nothing is listening

	ctx := context.Background()
	if tcpReachable(ctx, "127.0.0.1", port) {
		t.Error("expected tcpReachable=false with no listener")
	}
}

func TestIsInMaintenanceMode_ContextCancelled(t *testing.T) {
	// Use a pre-cancelled context — the dial should fail immediately
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	start := time.Now()
	result := tcpReachable(ctx, "127.0.0.1", 50000)
	elapsed := time.Since(start)

	if result {
		t.Error("expected tcpReachable=false with cancelled context")
	}
	// Should return almost immediately, not wait the full 5s timeout
	if elapsed > 2*time.Second {
		t.Errorf("took %v, expected near-instant return with cancelled context", elapsed)
	}
}

func TestIsInMaintenanceMode_PlainTCPNotSufficient(t *testing.T) {
	// IsInMaintenanceMode performs a gRPC+TLS probe, not just a TCP connect.
	// A plain TCP listener without TLS causes a transport error, which the
	// function conservatively treats as NOT maintenance mode. This is correct
	// behavior — we verify it here.
	ln, err := net.Listen("tcp", "127.0.0.1:50000")
	if err != nil {
		t.Skipf("cannot bind port 50000 (may be in use): %v", err)
	}
	defer ln.Close()

	ctx := context.Background()
	// Should return false: plain TCP listener ≠ Talos maintenance gRPC server
	if IsInMaintenanceMode(ctx, "127.0.0.1") {
		t.Error("IsInMaintenanceMode should return false for plain TCP (not gRPC+TLS)")
	}
}

func TestIsK8sAPIUp_Integration(t *testing.T) {
	// Start a listener on port 6443 if available, test the actual exported function
	ln, err := net.Listen("tcp", "127.0.0.1:6443")
	if err != nil {
		t.Skipf("cannot bind port 6443 (may be in use): %v", err)
	}
	defer ln.Close()

	ctx := context.Background()
	if !IsK8sAPIUp(ctx, "127.0.0.1") {
		t.Error("IsK8sAPIUp should return true with listener on 6443")
	}
}

func TestIsK8sAPIUp_NoListener(t *testing.T) {
	// Use a high ephemeral port that is unlikely to have anything on 6443
	// by testing against a non-routable IP with short timeout
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	// 198.51.100.1 is TEST-NET-2 (RFC 5737), guaranteed non-routable
	if IsK8sAPIUp(ctx, "198.51.100.1") {
		t.Error("IsK8sAPIUp should return false for non-routable address")
	}
}

// ─── IsTransientBootstrapError tests ────────────────────────────────────────

func TestIsTransientBootstrapError_Nil(t *testing.T) {
	if IsTransientBootstrapError(nil) {
		t.Error("expected false for nil error")
	}
}

func TestIsTransientBootstrapError_EmptyError(t *testing.T) {
	if IsTransientBootstrapError(fmt.Errorf("")) {
		t.Error("expected false for empty error message")
	}
}

func TestIsTransientBootstrapError_EachTransientString(t *testing.T) {
	// Every substring the function checks, each must return true individually.
	transients := []string{
		"connection refused",
		"connection reset",
		"EOF",
		"deadline exceeded",
		"Unavailable",
		"transport:",
		"tls:",
		"certificate required",
		"i/o timeout",
	}
	for _, s := range transients {
		err := fmt.Errorf("some prefix: %s and suffix", s)
		if !IsTransientBootstrapError(err) {
			t.Errorf("expected true for error containing %q", s)
		}
	}
}

func TestIsTransientBootstrapError_ExactSubstrings(t *testing.T) {
	// Minimal errors with just the substring itself.
	transients := []string{
		"connection refused",
		"connection reset",
		"EOF",
		"deadline exceeded",
		"Unavailable",
		"transport:",
		"tls:",
		"certificate required",
		"i/o timeout",
	}
	for _, s := range transients {
		err := fmt.Errorf("%s", s)
		if !IsTransientBootstrapError(err) {
			t.Errorf("expected true for exact error %q", s)
		}
	}
}

func TestIsTransientBootstrapError_CaseSensitive(t *testing.T) {
	// The function uses case-sensitive strings.Contains, so wrong-case must NOT match.
	wrongCase := []string{
		"Connection Refused",  // capital C and R
		"CONNECTION REFUSED",  // all caps
		"Connection Reset",    // capital C and R
		"eof",                 // lowercase
		"Eof",                 // mixed
		"Deadline Exceeded",   // capital D and E
		"UNAVAILABLE",         // all caps
		"unavailable",         // all lowercase
		"Transport:",          // capital T
		"TLS:",                // all caps
		"Tls:",                // mixed
		"Certificate Required", // capital C and R
		"I/O Timeout",         // capital I/O and T
		"I/O TIMEOUT",         // all caps
	}
	for _, s := range wrongCase {
		err := fmt.Errorf("%s", s)
		if IsTransientBootstrapError(err) {
			t.Errorf("expected false for wrong-case error %q (case-sensitive matching)", s)
		}
	}
}

func TestIsTransientBootstrapError_UnrelatedErrors(t *testing.T) {
	unrelated := []error{
		fmt.Errorf("permission denied"),
		fmt.Errorf("not found"),
		fmt.Errorf("rpc error: code = Internal desc = something broke"),
		fmt.Errorf("cluster is already bootstrapped"),
		fmt.Errorf("AlreadyExists"),
		fmt.Errorf("etcd is already running on this node"),
		fmt.Errorf("node is healthy"),
		fmt.Errorf("invalid configuration"),
	}
	for _, err := range unrelated {
		if IsTransientBootstrapError(err) {
			t.Errorf("expected false for unrelated error %q", err)
		}
	}
}

func TestIsTransientBootstrapError_AlreadyBootstrappedNotTransient(t *testing.T) {
	// "already bootstrapped" errors should NOT be transient — they are permanent successes.
	cases := []error{
		fmt.Errorf("rpc error: code = FailedPrecondition desc = cluster is already bootstrapped"),
		fmt.Errorf("rpc error: code = AlreadyExists desc = etcd member already exists"),
		fmt.Errorf("etcd is already running on this node"),
	}
	for _, err := range cases {
		if IsTransientBootstrapError(err) {
			t.Errorf("expected false for already-bootstrapped error %q", err)
		}
	}
}

func TestIsTransientBootstrapError_WrappedErrors(t *testing.T) {
	// fmt.Errorf with %w wrapping: Error() includes the inner message,
	// so the function should still detect the transient substring.
	inner := fmt.Errorf("dial tcp 10.0.0.1:50000: connection refused")
	wrapped := fmt.Errorf("Bootstrap on 10.0.0.1: %w", inner)
	if !IsTransientBootstrapError(wrapped) {
		t.Errorf("expected true for wrapped error %q", wrapped)
	}
}

func TestIsTransientBootstrapError_MultipleSubstrings(t *testing.T) {
	// Error containing multiple transient substrings — should still return true.
	err := fmt.Errorf("transport: connection refused after deadline exceeded")
	if !IsTransientBootstrapError(err) {
		t.Errorf("expected true for error with multiple transient substrings %q", err)
	}
}

// ─── isAlreadyBootstrapped edge cases ───────────────────────────────────────

func TestIsAlreadyBootstrapped_WrappedError(t *testing.T) {
	inner := fmt.Errorf("cluster is already bootstrapped")
	wrapped := fmt.Errorf("Bootstrap on 10.0.0.1 failed: %w", inner)
	if !isAlreadyBootstrapped(wrapped) {
		t.Errorf("expected true for wrapped error containing 'already bootstrapped': %q", wrapped)
	}
}

func TestIsAlreadyBootstrapped_WrappedAlreadyExists(t *testing.T) {
	inner := fmt.Errorf("rpc error: code = AlreadyExists desc = member exists")
	wrapped := fmt.Errorf("outer: %w", inner)
	if !isAlreadyBootstrapped(wrapped) {
		t.Errorf("expected true for wrapped AlreadyExists error: %q", wrapped)
	}
}

func TestIsAlreadyBootstrapped_WrappedEtcdRunning(t *testing.T) {
	inner := fmt.Errorf("etcd is already running on this node")
	wrapped := fmt.Errorf("bootstrap attempt: %w", inner)
	if !isAlreadyBootstrapped(wrapped) {
		t.Errorf("expected true for wrapped etcd running error: %q", wrapped)
	}
}

func TestIsAlreadyBootstrapped_BuriedInLongMessage(t *testing.T) {
	// The substring "already bootstrapped" buried in a very long error message.
	prefix := "rpc error: code = FailedPrecondition desc = long context about the node 10.0.0.1 "
	suffix := " which has been running for 72 hours with various services including etcd, kubelet, and containerd"
	err := fmt.Errorf("%scluster is already bootstrapped%s", prefix, suffix)
	if !isAlreadyBootstrapped(err) {
		t.Errorf("expected true for long error with 'already bootstrapped' buried in middle")
	}
}

func TestIsAlreadyBootstrapped_BuriedAlreadyExists(t *testing.T) {
	err := fmt.Errorf("lots of preamble text here about the node and its state: rpc error: code = AlreadyExists desc = etcd member already registered in the cluster roster for region us-east-1")
	if !isAlreadyBootstrapped(err) {
		t.Errorf("expected true for long error with 'AlreadyExists' buried in middle")
	}
}

// ─── IsUp tests ─────────────────────────────────────────────────────────────

func TestIsUp_ActiveListener(t *testing.T) {
	// Bind port 50000 for a real IsUp test through the exported function.
	ln, err := net.Listen("tcp", "127.0.0.1:50000")
	if err != nil {
		t.Skipf("cannot bind port 50000 (may be in use): %v", err)
	}
	defer ln.Close()

	ctx := context.Background()
	if !IsUp(ctx, "127.0.0.1") {
		t.Error("IsUp should return true with listener on port 50000")
	}
}

func TestIsUp_NoListener(t *testing.T) {
	// Use a non-routable IP with a short context timeout to avoid waiting 5 seconds.
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	// 198.51.100.1 is TEST-NET-2 (RFC 5737), guaranteed non-routable
	if IsUp(ctx, "198.51.100.1") {
		t.Error("IsUp should return false for non-routable address")
	}
}

func TestIsUp_CancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	start := time.Now()
	result := IsUp(ctx, "127.0.0.1")
	elapsed := time.Since(start)

	if result {
		t.Error("IsUp should return false with cancelled context")
	}
	if elapsed > 2*time.Second {
		t.Errorf("took %v, expected near-instant return with cancelled context", elapsed)
	}
}

// ─── IsInMaintenanceMode context cancellation ───────────────────────────────

func TestIsInMaintenanceMode_ContextCancelledMidCheck(t *testing.T) {
	// A pre-cancelled context should cause IsInMaintenanceMode to return false
	// quickly because the gRPC dial or the RPC call will fail with "context canceled".
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	start := time.Now()
	result := IsInMaintenanceMode(ctx, "127.0.0.1")
	elapsed := time.Since(start)

	if result {
		t.Error("IsInMaintenanceMode should return false with cancelled context")
	}
	// Should not wait the full 8-second probe timeout.
	if elapsed > 3*time.Second {
		t.Errorf("took %v, expected near-instant return with cancelled context", elapsed)
	}
}

func TestIsInMaintenanceMode_ShortTimeout(t *testing.T) {
	// A very short timeout context should cause the function to return false
	// (the 8s internal timeout is capped by the parent context's deadline).
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Millisecond)
	defer cancel()

	// Small sleep to ensure the context has expired by the time we call.
	time.Sleep(5 * time.Millisecond)

	result := IsInMaintenanceMode(ctx, "127.0.0.1")
	if result {
		t.Error("IsInMaintenanceMode should return false with expired context")
	}
}
