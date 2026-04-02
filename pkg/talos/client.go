// Package talos provides utilities for interacting with Talos Linux nodes
// using native gRPC — no talosctl binary dependency.
//
// Uses proper proto types from siderolabs/talos/pkg/machinery for type-safe
// gRPC calls. No hand-rolled proto encoding.
package talos

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	machinepb "github.com/siderolabs/talos/pkg/machinery/api/machine"
)

const (
	// TalosAPIPort is the Talos machine API port (gRPC, both maintenance and configured mode).
	TalosAPIPort = 50000
)

// ─── Connectivity checks ──────────────────────────────────────────────────────

// IsUp checks if the Talos API port (50000) is reachable.
// Returns true for both maintenance mode and running mode.
func IsUp(ctx context.Context, ip string) bool {
	return tcpReachable(ctx, ip, TalosAPIPort)
}

// IsInMaintenanceMode checks if Talos is in maintenance mode (port 50000 open,
// gRPC call accepted without client certificate).
//
// Bug fix: TLS 1.3 completes the handshake even for full-mode Talos (which uses
// post-handshake client auth). A plain TLS dial succeeds in BOTH modes; the
// "certificate required" rejection only surfaces at the gRPC application layer.
// We therefore probe with an actual (intentionally empty) ApplyConfiguration RPC:
//   - Maintenance mode: server accepts the call and returns a validation/parse error
//     (not "certificate required") → maintenance mode confirmed.
//   - Full mode: server rejects at gRPC layer with "certificate required" → not maintenance.
//   - Unreachable/timeout: dial or context error → not maintenance (conservative default).
func IsInMaintenanceMode(ctx context.Context, ip string) bool {
	probeCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()

	conn, err := newInsecureConn(probeCtx, ip)
	if err != nil {
		return false
	}
	defer conn.Close() //nolint:errcheck

	client := machinepb.NewMachineServiceClient(conn)
	// Send an intentionally empty ApplyConfiguration request.
	// Maintenance mode returns a parse/validation error (not "certificate required").
	// Full mode returns "certificate required" at the gRPC transport layer.
	_, err = client.ApplyConfiguration(probeCtx, &machinepb.ApplyConfigurationRequest{})
	if err != nil {
		errMsg := err.Error()
		// Full mode — server demands client cert. Definitely not maintenance.
		if strings.Contains(errMsg, "certificate required") {
			return false
		}
		// Transport/connectivity errors — server may be unreachable, rebooting, or an
		// old Talos instance that's slow to respond. Conservative: treat as NOT maintenance
		// to avoid skipping rescue/install when the node has a stale OS.
		if strings.Contains(errMsg, "deadline exceeded") ||
			strings.Contains(errMsg, "context canceled") ||
			strings.Contains(errMsg, "connection refused") ||
			strings.Contains(errMsg, "connection reset") ||
			strings.Contains(errMsg, "i/o timeout") ||
			strings.Contains(errMsg, "EOF") ||
			strings.Contains(errMsg, "Unavailable") ||
			strings.Contains(errMsg, "transport:") {
			return false
		}
		// Application-level error (e.g. validation, parse) → server IS in maintenance mode
		// and accepted the call without requiring client cert.
		return true
	}
	return true // unexpected success — treat as maintenance mode
}

// WipeEFIPartition connects to a Talos node in maintenance mode and wipes
// the EFI partition (nvme0n1p1) so that PXE rescue can boot on next reset.
// This is needed when reprovisioning a node that already has Talos installed —
// the UKI boot entry in EFI takes priority over PXE.
func WipeEFIPartition(ctx context.Context, ip string) error {
	wipeCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	conn, err := newInsecureConn(wipeCtx, ip)
	if err != nil {
		return fmt.Errorf("connect to %s for EFI wipe: %w", ip, err)
	}
	defer conn.Close() //nolint:errcheck

	client := machinepb.NewMachineServiceClient(conn)

	// Reset with EFI + STATE wipe — this removes the Talos UKI boot entry
	// and clears the STATE partition so Talos can't boot from disk.
	_, err = client.Reset(wipeCtx, &machinepb.ResetRequest{
		Graceful: false,
		Reboot:   true,
		SystemPartitionsToWipe: []*machinepb.ResetPartitionSpec{
			{Label: "EFI"},
			{Label: "BIOS"},
			{Label: "STATE"},
		},
	})
	if err != nil {
		return fmt.Errorf("reset with EFI wipe on %s: %w", ip, err)
	}
	return nil
}

// WipeEFIPartitionAuthenticated connects to a Talos node in FULL mode
// (with mTLS authentication) and wipes all system partitions on the system disk.
// Uses Mode=SYSTEM_DISK with empty SystemPartitionsToWipe to wipe ALL partitions
// (EFI, BIOS, STATE, etc.) regardless of label names — avoids Talos v1.12 errors
// where specific partition labels may not exist on all hardware configurations.
// Used when the node booted into full Talos (not maintenance) and
// the maintenance API is not available.
func WipeEFIPartitionAuthenticated(ctx context.Context, ip string, machineConfigYAML []byte) error {
	wipeCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	tlsConfig, err := AdminTLSConfig(machineConfigYAML)
	if err != nil {
		return fmt.Errorf("build TLS config for %s: %w", ip, err)
	}

	conn, err := grpc.DialContext(wipeCtx,
		net.JoinHostPort(ip, strconv.Itoa(TalosAPIPort)),
		grpc.WithTransportCredentials(credentials.NewTLS(tlsConfig)),
		grpc.WithDefaultCallOptions(grpc.WaitForReady(false)),
	)
	if err != nil {
		return fmt.Errorf("connect to %s (authenticated): %w", ip, err)
	}
	defer conn.Close() //nolint:errcheck

	client := machinepb.NewMachineServiceClient(conn)
	// Empty SystemPartitionsToWipe + Mode=SYSTEM_DISK = wipe all system partitions.
	// This avoids Talos v1.12 "VolumeStatuses doesn't exist" errors when specific
	// partition labels (EFI, BIOS) don't exist on the hardware.
	_, err = client.Reset(wipeCtx, &machinepb.ResetRequest{
		Graceful: false,
		Reboot:   true,
		Mode:     machinepb.ResetRequest_SYSTEM_DISK,
	})
	if err != nil {
		return fmt.Errorf("authenticated reset with system disk wipe on %s: %w", ip, err)
	}
	return nil
}

// IsK8sAPIUp checks if the Kubernetes API server (port 6443) is reachable.
func IsK8sAPIUp(ctx context.Context, ip string) bool {
	return tcpReachable(ctx, ip, 6443)
}

func tcpReachable(ctx context.Context, ip string, port int) bool {
	d := &net.Dialer{}
	c, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	conn, err := d.DialContext(c, "tcp", net.JoinHostPort(ip, strconv.Itoa(port)))
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// ─── ApplyConfig (maintenance mode — insecure, no client cert) ────────────────

// ApplyConfig sends a Talos machineconfig to a node in maintenance mode.
// Maintenance mode = port 50000, no mTLS, plain TLS (InsecureSkipVerify).
func ApplyConfig(ctx context.Context, ip string, configData []byte) error {
	conn, err := newInsecureConn(ctx, ip)
	if err != nil {
		return fmt.Errorf("connect to %s (maintenance): %w", ip, err)
	}
	defer conn.Close() //nolint:errcheck

	client := machinepb.NewMachineServiceClient(conn)
	_, err = client.ApplyConfiguration(ctx, &machinepb.ApplyConfigurationRequest{
		Data: configData,
		Mode: machinepb.ApplyConfigurationRequest_AUTO,
	})
	if err != nil {
		return fmt.Errorf("ApplyConfiguration on %s: %w", ip, err)
	}
	return nil
}

// ─── Bootstrap (configured mode — mTLS with admin client cert) ───────────────

// Bootstrap triggers etcd initialization on the first control plane node.
// Must be called exactly once on the init CP after ApplyConfig + reboot.
// Idempotent: already-bootstrapped is treated as success.
func Bootstrap(ctx context.Context, ip string, tlsCfg *tls.Config) error {
	conn, err := newAuthenticatedConn(ctx, ip, tlsCfg)
	if err != nil {
		return fmt.Errorf("connect to %s (authenticated): %w", ip, err)
	}
	defer conn.Close() //nolint:errcheck

	client := machinepb.NewMachineServiceClient(conn)
	_, err = client.Bootstrap(ctx, &machinepb.BootstrapRequest{})
	if err != nil {
		// "already bootstrapped" = success (idempotent)
		if isAlreadyBootstrapped(err) {
			return nil
		}
		return fmt.Errorf("Bootstrap on %s: %w", ip, err)
	}
	return nil
}

// ─── gRPC transport helpers ───────────────────────────────────────────────────

// newInsecureConn dials Talos maintenance API (no server cert verification).
func newInsecureConn(ctx context.Context, ip string) (*grpc.ClientConn, error) {
	addr := net.JoinHostPort(ip, strconv.Itoa(TalosAPIPort))
	return grpc.DialContext(ctx, addr, //nolint:staticcheck
		grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{
			InsecureSkipVerify: true, //nolint:gosec // maintenance mode: no CA available
		})),
	)
}

// newAuthenticatedConn dials Talos machine API with mTLS (client cert + server cert verification).
func newAuthenticatedConn(ctx context.Context, ip string, tlsCfg *tls.Config) (*grpc.ClientConn, error) {
	addr := net.JoinHostPort(ip, strconv.Itoa(TalosAPIPort))
	return grpc.DialContext(ctx, addr, //nolint:staticcheck
		grpc.WithTransportCredentials(credentials.NewTLS(tlsCfg)),
	)
}

// ─── Error helpers ────────────────────────────────────────────────────────────

func isAlreadyBootstrapped(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "already bootstrapped") ||
		strings.Contains(s, "AlreadyExists") ||
		strings.Contains(s, "etcd is already running")
}

// IsTransientBootstrapError returns true for errors that are expected during
// node startup and should be retried with a backoff rather than counted as failures.
// This includes: connection refused, TLS handshake errors (node still booting),
// deadline exceeded, and gRPC Unavailable.
func IsTransientBootstrapError(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "connection refused") ||
		strings.Contains(s, "connection reset") ||
		strings.Contains(s, "EOF") ||
		strings.Contains(s, "deadline exceeded") ||
		strings.Contains(s, "Unavailable") ||
		strings.Contains(s, "transport:") ||
		strings.Contains(s, "tls:") ||
		strings.Contains(s, "certificate required") ||
		strings.Contains(s, "i/o timeout")
}
