// Package talos provides utilities for interacting with Talos Linux nodes.
package talos

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"time"
)

const (
	// TalosAPIPort is the Talos maintenance API port (gRPC).
	TalosAPIPort = 50000
)

// IsInMaintenanceMode checks if a Talos node is in maintenance mode
// by testing TCP connectivity to port 50000.
func IsInMaintenanceMode(ctx context.Context, ip string) bool {
	dialer := &net.Dialer{}
	dialCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	conn, err := dialer.DialContext(dialCtx, "tcp", fmt.Sprintf("%s:%d", ip, TalosAPIPort))
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// ApplyConfig applies a Talos machineconfig to a node in maintenance mode.
// Uses talosctl CLI which must be available at /usr/local/bin/talosctl.
func ApplyConfig(ctx context.Context, ip string, configData []byte) error {
	// Write config to a temp file
	tmpFile, err := os.CreateTemp("", "talos-mc-*.yaml")
	if err != nil {
		return fmt.Errorf("create temp file for machineconfig: %w", err)
	}
	tmpPath := tmpFile.Name()
	defer os.Remove(tmpPath) //nolint:errcheck

	if _, err := tmpFile.Write(configData); err != nil {
		tmpFile.Close()
		return fmt.Errorf("write machineconfig: %w", err)
	}
	tmpFile.Close()

	talosctlBin := findTalosctl()
	applyCmd := exec.CommandContext(ctx, talosctlBin,
		"apply-config",
		"--insecure",
		"--nodes", ip,
		"--file", tmpPath,
	)

	var out bytes.Buffer
	applyCmd.Stdout = &out
	applyCmd.Stderr = &out

	if err := applyCmd.Run(); err != nil {
		return fmt.Errorf("talosctl apply-config on %s: %w\noutput: %s", ip, err, out.String())
	}
	return nil
}

// Bootstrap triggers etcd initialization on an init control plane node.
// This must be called exactly once on the first control plane after apply-config.
// Calling it on already-bootstrapped nodes or worker nodes returns an error which is safe to ignore.
func Bootstrap(ctx context.Context, ip string, talosConfigPath string) error {
	talosctlBin := findTalosctl()
	cmd := exec.CommandContext(ctx, talosctlBin,
		"--talosconfig", talosConfigPath,
		"--nodes", ip,
		"bootstrap",
	)

	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out

	if err := cmd.Run(); err != nil {
		outStr := out.String()
		// Already bootstrapped is not a real error
		if strings.Contains(outStr, "already bootstrapped") || strings.Contains(outStr, "AlreadyExists") {
			return nil
		}
		return fmt.Errorf("talosctl bootstrap on %s: %w\noutput: %s", ip, err, outStr)
	}
	return nil
}

// IsK8sAPIUp checks if the Kubernetes API server (port 6443) is reachable.
func IsK8sAPIUp(ctx context.Context, ip string) bool {
	dialer := &net.Dialer{}
	dialCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	conn, err := dialer.DialContext(dialCtx, "tcp", fmt.Sprintf("%s:%d", ip, 6443))
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// findTalosctl locates the talosctl binary.
func findTalosctl() string {
	for _, p := range []string{
		"/usr/local/bin/talosctl",
		"/usr/bin/talosctl",
		"talosctl",
	} {
		if _, err := exec.LookPath(p); err == nil {
			return p
		}
	}
	return "talosctl"
}
