// Package talos provides utilities for interacting with Talos Linux nodes
// using native gRPC — no talosctl binary dependency.
package talos

import (
	"context"
	"crypto/tls"
	"encoding/binary"
	"fmt"
	"net"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

const (
	// TalosAPIPort is the Talos machine API port (gRPC, both maintenance and configured mode).
	TalosAPIPort = 50000
)

// ─── Connectivity checks ──────────────────────────────────────────────────────

// IsInMaintenanceMode checks TCP reachability on port 50000.
func IsInMaintenanceMode(ctx context.Context, ip string) bool {
	return tcpReachable(ctx, ip, TalosAPIPort)
}

// IsK8sAPIUp checks if the Kubernetes API server (port 6443) is reachable.
func IsK8sAPIUp(ctx context.Context, ip string) bool {
	return tcpReachable(ctx, ip, 6443)
}

func tcpReachable(ctx context.Context, ip string, port int) bool {
	d := &net.Dialer{}
	c, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	conn, err := d.DialContext(c, "tcp", fmt.Sprintf("%s:%d", ip, port))
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// ─── ApplyConfig (maintenance mode — insecure, no client cert) ────────────────

// ApplyConfig sends a Talos machineconfig to a node in maintenance mode.
// Maintenance mode = port 50000, no mTLS, plain TLS (InsecureSkipVerify).
// Uses raw proto-over-gRPC encoding to avoid importing the full Talos SDK.
func ApplyConfig(ctx context.Context, ip string, configData []byte) error {
	conn, err := newInsecureConn(ctx, ip)
	if err != nil {
		return fmt.Errorf("connect to %s (maintenance): %w", ip, err)
	}
	defer conn.Close() //nolint:errcheck

	// ApplyConfigurationRequest proto (hand-encoded):
	//   field 1 (bytes) = config data
	//   field 4 (uint32) = mode (0 = AUTO, 1 = REBOOT, 2 = NO_REBOOT, 3 = STAGED)
	body := protoBytes(1, configData)
	body = append(body, protoVarint(4, 0)...) // mode = AUTO (reboot if needed)

	resp, err := grpcUnary(ctx, conn, "/machine.MachineService/ApplyConfiguration", body)
	if err != nil {
		return fmt.Errorf("ApplyConfiguration on %s: %w", ip, err)
	}
	_ = resp
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

	// BootstrapRequest proto: no fields needed (empty message)
	resp, err := grpcUnary(ctx, conn, "/machine.MachineService/Bootstrap", nil)
	if err != nil {
		// "already bootstrapped" = success (idempotent)
		if isAlreadyBootstrapped(err) {
			return nil
		}
		return err
	}
	_ = resp
	return nil
}

// ─── gRPC transport helpers ───────────────────────────────────────────────────

// newInsecureConn dials Talos maintenance API (no server cert verification).
func newInsecureConn(ctx context.Context, ip string) (*grpc.ClientConn, error) {
	addr := fmt.Sprintf("%s:%d", ip, TalosAPIPort)
	return grpc.DialContext(ctx, addr, //nolint:staticcheck
		grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{
			InsecureSkipVerify: true, //nolint:gosec // maintenance mode: no CA available
		})),
	)
}

// newAuthenticatedConn dials Talos machine API with mTLS (client cert + server cert verification).
func newAuthenticatedConn(ctx context.Context, ip string, tlsCfg *tls.Config) (*grpc.ClientConn, error) {
	addr := fmt.Sprintf("%s:%d", ip, TalosAPIPort)
	return grpc.DialContext(ctx, addr, //nolint:staticcheck
		grpc.WithTransportCredentials(credentials.NewTLS(tlsCfg)),
	)
}

// grpcUnary sends a hand-encoded proto request and reads the response.
// Uses raw gRPC framing (5-byte length-prefixed messages over HTTP/2).
func grpcUnary(ctx context.Context, conn *grpc.ClientConn, method string, reqBody []byte) ([]byte, error) {
	// Encode as gRPC framing: [compressed(1)] [length(4)] [body]
	frame := make([]byte, 5+len(reqBody))
	frame[0] = 0 // not compressed
	binary.BigEndian.PutUint32(frame[1:5], uint32(len(reqBody)))
	copy(frame[5:], reqBody)

	var respFrame []byte
	err := conn.Invoke(ctx, method, &rawMessage{data: frame}, &rawMessage{data: respFrame},
		grpc.ForceCodec(&rawCodec{}),
	)
	if err != nil {
		return nil, err
	}
	return respFrame, nil
}

// ─── Proto encoding (minimal, field types only) ───────────────────────────────

// protoBytes encodes a proto field of type bytes (wire type 2).
func protoBytes(fieldNum uint32, value []byte) []byte {
	tag := protoTag(fieldNum, 2)
	result := append(tag, protoLen(value)...)
	return append(result, value...)
}

// protoVarint encodes a proto field of type uint32/enum (wire type 0).
func protoVarint(fieldNum uint32, value uint64) []byte {
	tag := protoTag(fieldNum, 0)
	return append(tag, encodeVarint(value)...)
}

func protoTag(fieldNum uint32, wireType uint32) []byte {
	return encodeVarint(uint64(fieldNum<<3 | wireType))
}

func protoLen(b []byte) []byte {
	return append(encodeVarint(uint64(len(b))), b...)
}

func encodeVarint(v uint64) []byte {
	var buf [10]byte
	n := binary.PutUvarint(buf[:], v)
	return buf[:n]
}

// ─── Raw gRPC codec ───────────────────────────────────────────────────────────

// rawCodec passes pre-encoded proto bytes through without re-marshaling.
type rawCodec struct{}

func (rawCodec) Name() string { return "proto" }

func (rawCodec) Marshal(v interface{}) ([]byte, error) {
	if m, ok := v.(*rawMessage); ok {
		return m.data, nil
	}
	return nil, fmt.Errorf("rawCodec: unexpected type %T", v)
}

func (rawCodec) Unmarshal(data []byte, v interface{}) error {
	if m, ok := v.(*rawMessage); ok {
		m.data = data
		return nil
	}
	return fmt.Errorf("rawCodec: unexpected type %T", v)
}

type rawMessage struct {
	data []byte
}

// ─── Error helpers ────────────────────────────────────────────────────────────

func isAlreadyBootstrapped(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return contains(s, "already bootstrapped") ||
		contains(s, "AlreadyExists") ||
		contains(s, "etcd is already running")
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(s) > 0 && stringContains(s, sub))
}

func stringContains(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
