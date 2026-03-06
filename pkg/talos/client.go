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
// TLS handshake succeeds without client certificate — maintenance endpoint is unauthed).
// Returns false if Talos is in running mode (requires client cert) or unreachable.
func IsInMaintenanceMode(ctx context.Context, ip string) bool {
	dialer := &net.Dialer{}
	connCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	conn, err := tls.DialWithDialer(dialer, "tcp",
		net.JoinHostPort(ip, strconv.Itoa(TalosAPIPort)),
		&tls.Config{InsecureSkipVerify: true}, //nolint:gosec // intentional for maintenance check
	)
	if err != nil {
		// "certificate required" → running mode (not maintenance)
		// other errors → unreachable
		return false
	}
	conn.Close()
	return true // Connected without client cert → maintenance mode
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
