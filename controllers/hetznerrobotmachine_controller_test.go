package controllers

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	infrav1 "github.com/SylphxAI/cluster-api-provider-hetzner-robot/api/v1alpha1"
)

func TestIsPermanentError(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		expected bool
	}{
		// --- nil ---
		{
			name:     "nil error returns false",
			err:      nil,
			expected: false,
		},

		// --- unmarshal errors (config parsing) ---
		{
			name:     "unmarshal error is permanent",
			err:      fmt.Errorf("failed to unmarshal YAML config"),
			expected: true,
		},
		{
			name:     "unmarshal mixed case is permanent (lowercased match)",
			err:      fmt.Errorf("json: cannot Unmarshal string into Go value"),
			expected: true,
		},

		// --- invalid errors (validation) ---
		{
			name:     "invalid field error is permanent",
			err:      fmt.Errorf("invalid value for spec.talosVersion"),
			expected: true,
		},
		{
			name:     "invalid config is permanent",
			err:      fmt.Errorf("machine config is Invalid"),
			expected: true,
		},

		// --- secret not found (both keywords required) ---
		{
			name:     "secret not found is permanent",
			err:      fmt.Errorf("secret \"bootstrap-data\" not found"),
			expected: true,
		},
		{
			name:     "Secret not found mixed case is permanent",
			err:      fmt.Errorf("Secret default/my-secret Not Found in namespace"),
			expected: true,
		},
		{
			name:     "secret alone without not found is not permanent",
			err:      fmt.Errorf("secret retrieval timed out"),
			expected: false,
		},
		{
			name:     "not found alone without secret is not permanent",
			err:      fmt.Errorf("resource not found"),
			expected: false,
		},

		// --- must specify either ---
		{
			name:     "must specify either is permanent",
			err:      fmt.Errorf("must specify either talosImageURL or talosVersion"),
			expected: true,
		},

		// --- transient errors (SSH, network, API) ---
		{
			name:     "SSH connection refused is transient",
			err:      fmt.Errorf("ssh: connect to host 1.2.3.4 port 22: Connection refused"),
			expected: false,
		},
		{
			name:     "SSH timeout is transient",
			err:      fmt.Errorf("ssh: handshake failed: read tcp: i/o timeout"),
			expected: false,
		},
		{
			name:     "connection reset is transient",
			err:      fmt.Errorf("read: connection reset by peer"),
			expected: false,
		},
		{
			name:     "API rate limit is transient",
			err:      fmt.Errorf("robot API: 429 Too Many Requests"),
			expected: false,
		},
		{
			name:     "context deadline exceeded is transient",
			err:      fmt.Errorf("context deadline exceeded"),
			expected: false,
		},
		{
			name:     "no available host is transient",
			err:      fmt.Errorf("no available HetznerRobotHost found"),
			expected: false,
		},
		{
			name:     "random error is transient",
			err:      fmt.Errorf("something unexpected happened"),
			expected: false,
		},
		{
			name:     "empty error message is transient",
			err:      fmt.Errorf(""),
			expected: false,
		},

		// --- edge cases: keyword overlap ---
		{
			name:     "error containing both unmarshal and transient context is still permanent",
			err:      fmt.Errorf("failed to unmarshal: connection reset"),
			expected: true,
		},
		{
			name:     "wrapped error with invalid is permanent",
			err:      fmt.Errorf("resolve host: build robot client: invalid credentials"),
			expected: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isPermanentError(tt.err)
			if got != tt.expected {
				errMsg := "<nil>"
				if tt.err != nil {
					errMsg = tt.err.Error()
				}
				t.Errorf("isPermanentError(%q) = %v, want %v", errMsg, got, tt.expected)
			}
		})
	}
}

func TestComputeBackoff(t *testing.T) {
	tests := []struct {
		name       string
		retryCount int
		expected   time.Duration
	}{
		// retryCount=0: exp = max(0-1, 0) = 0 → 30s * 2^0 = 30s
		{
			name:       "retryCount 0 yields 30s (exp clamped to 0)",
			retryCount: 0,
			expected:   30 * time.Second,
		},
		// retryCount=1: exp = 0 → 30s * 2^0 = 30s
		{
			name:       "retryCount 1 yields 30s",
			retryCount: 1,
			expected:   30 * time.Second,
		},
		// retryCount=2: exp = 1 → 30s * 2^1 = 60s
		{
			name:       "retryCount 2 yields 60s",
			retryCount: 2,
			expected:   60 * time.Second,
		},
		// retryCount=3: exp = 2 → 30s * 2^2 = 120s
		{
			name:       "retryCount 3 yields 120s",
			retryCount: 3,
			expected:   120 * time.Second,
		},
		// retryCount=4: exp = 3 → 30s * 2^3 = 240s
		{
			name:       "retryCount 4 yields 240s",
			retryCount: 4,
			expected:   240 * time.Second,
		},
		// retryCount=5: exp = 4 → 30s * 2^4 = 480s → capped at 300s
		{
			name:       "retryCount 5 yields 300s (capped at 5 min)",
			retryCount: 5,
			expected:   5 * time.Minute,
		},
		// retryCount=6: exp = 5 → 30s * 2^5 = 960s → capped at 300s
		{
			name:       "retryCount 6 capped at 5 min",
			retryCount: 6,
			expected:   5 * time.Minute,
		},
		// retryCount=10: exp = 9 → 30s * 2^9 = 15360s → capped at 300s
		{
			name:       "retryCount 10 capped at 5 min",
			retryCount: 10,
			expected:   5 * time.Minute,
		},
		// retryCount=11: exp = 10 → 30s * 2^10 = 30720s → capped at 300s
		{
			name:       "retryCount 11 exponent at cap boundary (exp=10)",
			retryCount: 11,
			expected:   5 * time.Minute,
		},
		// retryCount=12: exp would be 11 but clamped to 10 → same as retryCount=11
		{
			name:       "retryCount 12 exponent clamped to 10",
			retryCount: 12,
			expected:   5 * time.Minute,
		},
		// retryCount=100: exp clamped to 10, result capped at 5 min
		{
			name:       "retryCount 100 overflow safety (exp clamped, duration capped)",
			retryCount: 100,
			expected:   5 * time.Minute,
		},
		// retryCount=1000: extreme value, still safe
		{
			name:       "retryCount 1000 extreme overflow safety",
			retryCount: 1000,
			expected:   5 * time.Minute,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := computeBackoff(tt.retryCount)
			if got != tt.expected {
				t.Errorf("computeBackoff(%d) = %v, want %v", tt.retryCount, got, tt.expected)
			}
		})
	}
}

func TestComputeBackoff_Monotonic(t *testing.T) {
	// Verify backoff is monotonically non-decreasing across a range of retry counts.
	var prev time.Duration
	for i := 0; i <= 20; i++ {
		got := computeBackoff(i)
		if got < prev {
			t.Errorf("backoff decreased at retryCount=%d: %v < %v", i, got, prev)
		}
		if got < 30*time.Second {
			t.Errorf("backoff below minimum 30s at retryCount=%d: %v", i, got)
		}
		if got > 5*time.Minute {
			t.Errorf("backoff exceeded 5 min cap at retryCount=%d: %v", i, got)
		}
		prev = got
	}
}

func TestComputeBackoff_NegativeRetryCount(t *testing.T) {
	// Negative retryCount: exp = retryCount-1 < 0, clamped to 0 → 30s * 2^0 = 30s.
	got := computeBackoff(-1)
	if got != 30*time.Second {
		t.Errorf("computeBackoff(-1) = %v, want 30s", got)
	}

	got = computeBackoff(-100)
	if got != 30*time.Second {
		t.Errorf("computeBackoff(-100) = %v, want 30s", got)
	}
}

func TestResolveHostForDeleteDoesNotClaimAvailableSelectorHost(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	if err := infrav1.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}

	hrm := &infrav1.HetznerRobotMachine{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "phantom-cp",
			Namespace: "caphr",
		},
		Spec: infrav1.HetznerRobotMachineSpec{
			HostSelector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"role": "control-plane"},
			},
		},
	}
	availableHost := &infrav1.HetznerRobotHost{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "future-cp-host",
			Namespace: "caphr",
			Labels:    map[string]string{"role": "control-plane"},
		},
		Spec: infrav1.HetznerRobotHostSpec{
			ServerID: 12345,
		},
		Status: infrav1.HetznerRobotHostStatus{
			State: infrav1.HostStateAvailable,
		},
	}
	client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(hrm, availableHost).Build()
	reconciler := &HetznerRobotMachineReconciler{Client: client, Scheme: scheme}

	host, shouldRelease, err := reconciler.resolveHostForDelete(ctx, hrm)
	if err != nil {
		t.Fatalf("resolveHostForDelete returned error: %v", err)
	}
	if host != nil {
		t.Fatalf("resolveHostForDelete claimed or returned host %q; want nil", host.Name)
	}
	if shouldRelease {
		t.Fatal("resolveHostForDelete returned shouldRelease=true for an unclaimed selector host")
	}

	after := &infrav1.HetznerRobotHost{}
	if err := client.Get(ctx, clientKey("caphr", "future-cp-host"), after); err != nil {
		t.Fatalf("get host after resolve: %v", err)
	}
	if after.Status.State != infrav1.HostStateAvailable {
		t.Fatalf("host state changed to %q; want Available", after.Status.State)
	}
	if after.Status.MachineRef != nil {
		t.Fatalf("host MachineRef changed to %#v; want nil", after.Status.MachineRef)
	}
}

func TestResolveHostForDeleteFindsAlreadyClaimedSelectorHost(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	if err := infrav1.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}

	hrm := &infrav1.HetznerRobotMachine{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "claimed-cp",
			Namespace: "caphr",
		},
		Spec: infrav1.HetznerRobotMachineSpec{
			HostSelector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"role": "control-plane"},
			},
		},
	}
	claimedHost := &infrav1.HetznerRobotHost{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "claimed-cp-host",
			Namespace: "caphr",
			Labels:    map[string]string{"role": "control-plane"},
		},
		Spec: infrav1.HetznerRobotHostSpec{
			ServerID: 12345,
		},
		Status: infrav1.HetznerRobotHostStatus{
			State: infrav1.HostStateClaimed,
			MachineRef: &infrav1.MachineReference{
				Name:      "claimed-cp",
				Namespace: "caphr",
			},
		},
	}
	client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(hrm, claimedHost).Build()
	reconciler := &HetznerRobotMachineReconciler{Client: client, Scheme: scheme}

	host, shouldRelease, err := reconciler.resolveHostForDelete(ctx, hrm)
	if err != nil {
		t.Fatalf("resolveHostForDelete returned error: %v", err)
	}
	if host == nil || host.Name != "claimed-cp-host" {
		t.Fatalf("resolveHostForDelete returned host %v; want claimed-cp-host", host)
	}
	if !shouldRelease {
		t.Fatal("resolveHostForDelete returned shouldRelease=false for a host claimed by this HRM")
	}
}

func TestResolveHostForDeleteUsesStatusHostRef(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	if err := infrav1.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}

	hrm := &infrav1.HetznerRobotMachine{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "adopted-cp",
			Namespace: "caphr",
		},
		Status: infrav1.HetznerRobotMachineStatus{
			HostRef: "cp-host",
		},
	}
	hostRef := &infrav1.HetznerRobotHost{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cp-host",
			Namespace: "caphr",
		},
		Spec: infrav1.HetznerRobotHostSpec{
			ServerID: 12345,
		},
		Status: infrav1.HetznerRobotHostStatus{
			State: infrav1.HostStateProvisioned,
		},
	}
	client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(hrm, hostRef).Build()
	reconciler := &HetznerRobotMachineReconciler{Client: client, Scheme: scheme}

	host, shouldRelease, err := reconciler.resolveHostForDelete(ctx, hrm)
	if err != nil {
		t.Fatalf("resolveHostForDelete returned error: %v", err)
	}
	if host == nil || host.Name != "cp-host" {
		t.Fatalf("resolveHostForDelete returned host %v; want cp-host", host)
	}
	if !shouldRelease {
		t.Fatal("resolveHostForDelete returned shouldRelease=false for status.hostRef")
	}
}

func TestResolveHostForDeleteDoesNotUseUnclaimedSpecHostRef(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	if err := infrav1.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}

	hrm := &infrav1.HetznerRobotMachine{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "unclaimed-cp",
			Namespace: "caphr",
		},
		Spec: infrav1.HetznerRobotMachineSpec{
			HostRef: &corev1.LocalObjectReference{Name: "cp-host"},
		},
	}
	hostRef := &infrav1.HetznerRobotHost{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cp-host",
			Namespace: "caphr",
		},
		Spec: infrav1.HetznerRobotHostSpec{
			ServerID: 12345,
		},
		Status: infrav1.HetznerRobotHostStatus{
			State: infrav1.HostStateAvailable,
		},
	}
	client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(hrm, hostRef).Build()
	reconciler := &HetznerRobotMachineReconciler{Client: client, Scheme: scheme}

	host, shouldRelease, err := reconciler.resolveHostForDelete(ctx, hrm)
	if err != nil {
		t.Fatalf("resolveHostForDelete returned error: %v", err)
	}
	if host != nil {
		t.Fatalf("resolveHostForDelete returned unclaimed spec.hostRef host %q; want nil", host.Name)
	}
	if shouldRelease {
		t.Fatal("resolveHostForDelete returned shouldRelease=true for an unclaimed spec.hostRef host")
	}
}

func TestAuthorizeDestructiveProvisioning(t *testing.T) {
	tests := []struct {
		name    string
		host    infrav1.HetznerRobotHost
		wantErr string
	}{
		{
			name: "compute host with explicit clean slate policy is allowed",
			host: infrav1.HetznerRobotHost{
				ObjectMeta: metav1.ObjectMeta{Name: "worker-1"},
				Spec: infrav1.HetznerRobotHostSpec{
					LifecycleClass:                infrav1.HostLifecycleClassCompute,
					DestructiveProvisioningPolicy: infrav1.DestructiveProvisioningPolicyAlwaysCleanSlate,
				},
			},
		},
		{
			name: "missing lifecycle class fails closed",
			host: infrav1.HetznerRobotHost{
				ObjectMeta: metav1.ObjectMeta{Name: "unknown-1"},
				Spec: infrav1.HetznerRobotHostSpec{
					DestructiveProvisioningPolicy: infrav1.DestructiveProvisioningPolicyAlwaysCleanSlate,
				},
			},
			wantErr: "lifecycleClass is required",
		},
		{
			name: "missing policy fails closed",
			host: infrav1.HetznerRobotHost{
				ObjectMeta: metav1.ObjectMeta{Name: "worker-2"},
				Spec: infrav1.HetznerRobotHostSpec{
					LifecycleClass: infrav1.HostLifecycleClassCompute,
				},
			},
			wantErr: "policy",
		},
		{
			name: "maintenance mode fails closed",
			host: infrav1.HetznerRobotHost{
				ObjectMeta: metav1.ObjectMeta{Name: "worker-3"},
				Spec: infrav1.HetznerRobotHostSpec{
					LifecycleClass:                infrav1.HostLifecycleClassCompute,
					DestructiveProvisioningPolicy: infrav1.DestructiveProvisioningPolicyAlwaysCleanSlate,
					MaintenanceMode:               true,
				},
			},
			wantErr: "maintenanceMode=true",
		},
		{
			name: "control-plane host is denied by default",
			host: infrav1.HetznerRobotHost{
				ObjectMeta: metav1.ObjectMeta{Name: "cp-1"},
				Spec: infrav1.HetznerRobotHostSpec{
					LifecycleClass:                infrav1.HostLifecycleClassControlPlane,
					DestructiveProvisioningPolicy: infrav1.DestructiveProvisioningPolicyNeverDestructiveByDefault,
				},
			},
			wantErr: "control-plane",
		},
		{
			name: "storage host requires external release that is not implemented yet",
			host: infrav1.HetznerRobotHost{
				ObjectMeta: metav1.ObjectMeta{Name: "storage-1"},
				Spec: infrav1.HetznerRobotHostSpec{
					LifecycleClass:                infrav1.HostLifecycleClassStorage,
					DestructiveProvisioningPolicy: infrav1.DestructiveProvisioningPolicyRequiresExternalRelease,
				},
			},
			wantErr: "external storage release",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := authorizeDestructiveProvisioning(&tt.host)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("authorizeDestructiveProvisioning returned error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("authorizeDestructiveProvisioning error = %v; want substring %q", err, tt.wantErr)
			}
		})
	}
}

func TestAuthorizeAutomatedHardwareReset(t *testing.T) {
	tests := []struct {
		name    string
		host    infrav1.HetznerRobotHost
		wantErr string
	}{
		{
			name: "compute host with explicit clean slate policy is allowed",
			host: infrav1.HetznerRobotHost{
				ObjectMeta: metav1.ObjectMeta{Name: "worker-1"},
				Spec: infrav1.HetznerRobotHostSpec{
					LifecycleClass:                infrav1.HostLifecycleClassCompute,
					DestructiveProvisioningPolicy: infrav1.DestructiveProvisioningPolicyAlwaysCleanSlate,
				},
			},
		},
		{
			name: "control-plane host requires platform quorum gate",
			host: infrav1.HetznerRobotHost{
				ObjectMeta: metav1.ObjectMeta{Name: "cp-1"},
				Spec: infrav1.HetznerRobotHostSpec{
					LifecycleClass:                infrav1.HostLifecycleClassControlPlane,
					DestructiveProvisioningPolicy: infrav1.DestructiveProvisioningPolicyNeverDestructiveByDefault,
				},
			},
			wantErr: "platform quorum gate",
		},
		{
			name: "storage host requires storage health gate",
			host: infrav1.HetznerRobotHost{
				ObjectMeta: metav1.ObjectMeta{Name: "storage-1"},
				Spec: infrav1.HetznerRobotHostSpec{
					LifecycleClass:                infrav1.HostLifecycleClassStorage,
					DestructiveProvisioningPolicy: infrav1.DestructiveProvisioningPolicyRequiresExternalRelease,
				},
			},
			wantErr: "storage health/release gate",
		},
		{
			name: "maintenance mode fails closed",
			host: infrav1.HetznerRobotHost{
				ObjectMeta: metav1.ObjectMeta{Name: "worker-2"},
				Spec: infrav1.HetznerRobotHostSpec{
					LifecycleClass:                infrav1.HostLifecycleClassCompute,
					DestructiveProvisioningPolicy: infrav1.DestructiveProvisioningPolicyAlwaysCleanSlate,
					MaintenanceMode:               true,
				},
			},
			wantErr: "maintenanceMode=true",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := authorizeAutomatedHardwareReset(&tt.host)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("authorizeAutomatedHardwareReset returned error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("authorizeAutomatedHardwareReset error = %v; want substring %q", err, tt.wantErr)
			}
		})
	}
}

func TestResolveHostSkipsMaintenanceSelectorHost(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	if err := infrav1.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}

	hrm := &infrav1.HetznerRobotMachine{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "worker",
			Namespace: "caphr",
		},
		Spec: infrav1.HetznerRobotMachineSpec{
			HostSelector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"role": "worker"},
			},
		},
	}
	maintenanceHost := &infrav1.HetznerRobotHost{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "worker-maintenance",
			Namespace: "caphr",
			Labels:    map[string]string{"role": "worker"},
		},
		Spec: infrav1.HetznerRobotHostSpec{
			ServerID:        12345,
			MaintenanceMode: true,
		},
		Status: infrav1.HetznerRobotHostStatus{
			State: infrav1.HostStateAvailable,
		},
	}
	client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(hrm, maintenanceHost).Build()
	reconciler := &HetznerRobotMachineReconciler{Client: client, Scheme: scheme}

	host, err := reconciler.resolveHost(ctx, hrm)
	if err == nil {
		t.Fatalf("resolveHost returned host %v; want maintenance-mode denial", host)
	}
	if !strings.Contains(err.Error(), "no non-maintenance Available") {
		t.Fatalf("resolveHost error = %v; want non-maintenance message", err)
	}

	after := &infrav1.HetznerRobotHost{}
	if err := client.Get(ctx, clientKey("caphr", "worker-maintenance"), after); err != nil {
		t.Fatalf("get host after resolve: %v", err)
	}
	if after.Status.MachineRef != nil || after.Status.ConsumerRef != nil {
		t.Fatalf(
			"maintenance host was claimed: machineRef=%#v consumerRef=%#v",
			after.Status.MachineRef,
			after.Status.ConsumerRef,
		)
	}
}

func TestResolveHostSetsConsumerRefOnClaim(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	if err := infrav1.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}

	hrm := &infrav1.HetznerRobotMachine{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "worker",
			Namespace: "caphr",
		},
		Spec: infrav1.HetznerRobotMachineSpec{
			HostRef: &corev1.LocalObjectReference{Name: "worker-host"},
		},
	}
	host := &infrav1.HetznerRobotHost{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "worker-host",
			Namespace: "caphr",
		},
		Spec: infrav1.HetznerRobotHostSpec{
			ServerID: 12345,
		},
		Status: infrav1.HetznerRobotHostStatus{
			State: infrav1.HostStateAvailable,
		},
	}
	client := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&infrav1.HetznerRobotHost{}).
		WithObjects(hrm, host).
		Build()
	reconciler := &HetznerRobotMachineReconciler{Client: client, Scheme: scheme}

	claimed, err := reconciler.resolveHost(ctx, hrm)
	if err != nil {
		t.Fatalf("resolveHost returned error: %v", err)
	}
	if claimed.Name != "worker-host" {
		t.Fatalf("resolveHost returned host %q; want worker-host", claimed.Name)
	}

	after := &infrav1.HetznerRobotHost{}
	if err := client.Get(ctx, clientKey("caphr", "worker-host"), after); err != nil {
		t.Fatalf("get host after resolve: %v", err)
	}
	if after.Status.ConsumerRef == nil || after.Status.ConsumerRef.Name != "worker" {
		t.Fatalf("ConsumerRef = %#v; want worker", after.Status.ConsumerRef)
	}
	if after.Status.MachineRef == nil || after.Status.MachineRef.Name != "worker" {
		t.Fatalf("legacy MachineRef = %#v; want worker", after.Status.MachineRef)
	}
}

func TestReleaseHostTracksLastConsumerRef(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	if err := infrav1.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}

	host := &infrav1.HetznerRobotHost{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "worker-host",
			Namespace: "caphr",
		},
		Spec: infrav1.HetznerRobotHostSpec{
			ServerID: 12345,
		},
		Status: infrav1.HetznerRobotHostStatus{
			State: infrav1.HostStateProvisioned,
			ConsumerRef: &infrav1.MachineReference{
				Name:      "worker",
				Namespace: "caphr",
			},
			MachineRef: &infrav1.MachineReference{
				Name:      "worker",
				Namespace: "caphr",
			},
		},
	}
	client := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&infrav1.HetznerRobotHost{}).
		WithObjects(host).
		Build()
	reconciler := &HetznerRobotMachineReconciler{Client: client, Scheme: scheme}

	if err := reconciler.releaseHost(ctx, "caphr", "worker-host"); err != nil {
		t.Fatalf("releaseHost returned error: %v", err)
	}

	after := &infrav1.HetznerRobotHost{}
	if err := client.Get(ctx, clientKey("caphr", "worker-host"), after); err != nil {
		t.Fatalf("get host after release: %v", err)
	}
	if after.Status.State != infrav1.HostStateAvailable {
		t.Fatalf("state = %q; want Available", after.Status.State)
	}
	if after.Status.ConsumerRef != nil || after.Status.MachineRef != nil {
		t.Fatalf(
			"consumer refs not cleared: consumerRef=%#v machineRef=%#v",
			after.Status.ConsumerRef,
			after.Status.MachineRef,
		)
	}
	if after.Status.LastConsumerRef == nil || after.Status.LastConsumerRef.Name != "worker" {
		t.Fatalf("LastConsumerRef = %#v; want worker", after.Status.LastConsumerRef)
	}
	if after.Status.DirtyReason != "ReleasedAfterMachineDelete" {
		t.Fatalf("DirtyReason = %q; want ReleasedAfterMachineDelete", after.Status.DirtyReason)
	}
}

func clientKey(namespace, name string) client.ObjectKey {
	return client.ObjectKey{Namespace: namespace, Name: name}
}
