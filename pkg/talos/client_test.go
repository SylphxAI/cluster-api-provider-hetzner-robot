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
