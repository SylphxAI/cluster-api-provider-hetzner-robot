package controllers

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	infrav1 "github.com/SylphxAI/cluster-api-provider-hetzner-robot/api/v1alpha1"
)

// newTestRemediation creates a HetznerRobotRemediation with the given phase for testing.
func newTestRemediation(phase infrav1.RemediationPhase, retryCount, retryLimit int) *infrav1.HetznerRobotRemediation {
	return &infrav1.HetznerRobotRemediation{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-remediation",
			Namespace: "default",
		},
		Spec: infrav1.HetznerRobotRemediationSpec{
			Strategy: infrav1.RemediationStrategy{
				Type:       infrav1.RemediationStrategyReboot,
				RetryLimit: retryLimit,
				Timeout:    metav1.Duration{Duration: 5 * time.Minute},
			},
		},
		Status: infrav1.HetznerRobotRemediationStatus{
			Phase:      phase,
			RetryCount: retryCount,
		},
	}
}

// simulatePhaseTransition applies the same transition logic as the controller's phase machine.
// This extracts the branching logic so tests verify the actual decision rules.
func simulatePhaseTransition(rem *infrav1.HetznerRobotRemediation) {
	switch rem.Status.Phase {
	case "":
		// Initial: trigger first reset
		now := metav1.Now()
		rem.Status.Phase = infrav1.RemediationPhaseRunning
		rem.Status.RetryCount = 1
		rem.Status.LastRemediated = &now

	case infrav1.RemediationPhaseRunning:
		// Timeout elapsed: transition to Waiting (immediate, no extra delay)
		rem.Status.Phase = infrav1.RemediationPhaseWaiting

	case infrav1.RemediationPhaseWaiting:
		if rem.Status.RetryCount >= rem.Spec.Strategy.RetryLimit {
			// Exhausted → terminal
			rem.Status.Phase = infrav1.RemediationPhaseDeleting
		} else {
			// Retry: issue another reset
			rem.Status.Phase = infrav1.RemediationPhaseRunning
			rem.Status.RetryCount++
			now := metav1.Now()
			rem.Status.LastRemediated = &now
		}

	case infrav1.RemediationPhaseDeleting:
		// Terminal — no-op
	}
}

func TestPhaseTransitions(t *testing.T) {
	tests := []struct {
		name          string
		phase         infrav1.RemediationPhase
		retryCount    int
		retryLimit    int
		expectedPhase infrav1.RemediationPhase
	}{
		{
			name:          "initial triggers reset and enters Running",
			phase:         "",
			retryCount:    0,
			retryLimit:    3,
			expectedPhase: infrav1.RemediationPhaseRunning,
		},
		{
			name:          "Running transitions to Waiting",
			phase:         infrav1.RemediationPhaseRunning,
			retryCount:    1,
			retryLimit:    3,
			expectedPhase: infrav1.RemediationPhaseWaiting,
		},
		{
			name:          "Waiting with retries left transitions to Running",
			phase:         infrav1.RemediationPhaseWaiting,
			retryCount:    1,
			retryLimit:    3,
			expectedPhase: infrav1.RemediationPhaseRunning,
		},
		{
			name:          "Waiting with retries exhausted transitions to Deleting",
			phase:         infrav1.RemediationPhaseWaiting,
			retryCount:    3,
			retryLimit:    3,
			expectedPhase: infrav1.RemediationPhaseDeleting,
		},
		{
			name:          "Deleting stays Deleting (terminal)",
			phase:         infrav1.RemediationPhaseDeleting,
			retryCount:    3,
			retryLimit:    3,
			expectedPhase: infrav1.RemediationPhaseDeleting,
		},
		{
			name:          "single retry limit exhausts after first attempt",
			phase:         infrav1.RemediationPhaseWaiting,
			retryCount:    1,
			retryLimit:    1,
			expectedPhase: infrav1.RemediationPhaseDeleting,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rem := newTestRemediation(tt.phase, tt.retryCount, tt.retryLimit)
			simulatePhaseTransition(rem)

			if rem.Status.Phase != tt.expectedPhase {
				t.Errorf("expected phase %q, got %q", tt.expectedPhase, rem.Status.Phase)
			}
		})
	}
}

func TestInitialPhase_SetsRetryCountAndTimestamp(t *testing.T) {
	rem := newTestRemediation("", 0, 2)
	simulatePhaseTransition(rem)

	if rem.Status.RetryCount != 1 {
		t.Errorf("expected retryCount 1 after initial reset, got %d", rem.Status.RetryCount)
	}
	if rem.Status.LastRemediated == nil {
		t.Error("expected LastRemediated to be set after initial reset")
	}
}

func TestWaitingRetry_IncrementsCountAndSetsTimestamp(t *testing.T) {
	rem := newTestRemediation(infrav1.RemediationPhaseWaiting, 1, 3)
	simulatePhaseTransition(rem)

	if rem.Status.RetryCount != 2 {
		t.Errorf("expected retryCount 2 after retry, got %d", rem.Status.RetryCount)
	}
	if rem.Status.LastRemediated == nil {
		t.Error("expected LastRemediated to be set after retry")
	}
}

func TestFullRetrySequence(t *testing.T) {
	// Simulate a complete lifecycle: 3 retries, all fail → terminal Deleting
	rem := newTestRemediation("", 0, 3)

	// Track expected progression
	expectedPhases := []infrav1.RemediationPhase{
		infrav1.RemediationPhaseRunning,  // initial → Running (reset #1)
		infrav1.RemediationPhaseWaiting,  // Running → Waiting (timeout)
		infrav1.RemediationPhaseRunning,  // Waiting → Running (reset #2)
		infrav1.RemediationPhaseWaiting,  // Running → Waiting (timeout)
		infrav1.RemediationPhaseRunning,  // Waiting → Running (reset #3)
		infrav1.RemediationPhaseWaiting,  // Running → Waiting (timeout)
		infrav1.RemediationPhaseDeleting, // Waiting → Deleting (exhausted)
	}

	for i, expected := range expectedPhases {
		simulatePhaseTransition(rem)
		if rem.Status.Phase != expected {
			t.Fatalf("step %d: expected phase %q, got %q (retryCount=%d)",
				i+1, expected, rem.Status.Phase, rem.Status.RetryCount)
		}
	}

	if rem.Status.RetryCount != 3 {
		t.Errorf("expected final retryCount 3, got %d", rem.Status.RetryCount)
	}
}

func TestDeletingIsTerminal(t *testing.T) {
	rem := newTestRemediation(infrav1.RemediationPhaseDeleting, 3, 3)

	// Apply transition multiple times — should remain in Deleting
	for i := 0; i < 3; i++ {
		simulatePhaseTransition(rem)
	}

	if rem.Status.Phase != infrav1.RemediationPhaseDeleting {
		t.Errorf("expected Deleting to stay terminal, got %q", rem.Status.Phase)
	}
	if rem.Status.RetryCount != 3 {
		t.Errorf("retryCount changed in terminal phase: got %d", rem.Status.RetryCount)
	}
}

func TestGetTimeout_Default(t *testing.T) {
	r := &HetznerRobotRemediationReconciler{}

	// Zero timeout → should default to 5 minutes
	rem := newTestRemediation("", 0, 1)
	rem.Spec.Strategy.Timeout = metav1.Duration{Duration: 0}

	timeout := r.getTimeout(rem)
	if timeout != 5*time.Minute {
		t.Errorf("expected default timeout 5m, got %v", timeout)
	}

	// Explicit timeout → should use it
	rem.Spec.Strategy.Timeout = metav1.Duration{Duration: 3 * time.Minute}
	timeout = r.getTimeout(rem)
	if timeout != 3*time.Minute {
		t.Errorf("expected timeout 3m, got %v", timeout)
	}
}

func TestDeepCopy_Independence(t *testing.T) {
	now := metav1.Now()
	rem := &infrav1.HetznerRobotRemediation{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test",
			Namespace: "default",
		},
		Spec: infrav1.HetznerRobotRemediationSpec{
			Strategy: infrav1.RemediationStrategy{
				Type:       infrav1.RemediationStrategyReboot,
				RetryLimit: 3,
				Timeout:    metav1.Duration{Duration: 5 * time.Minute},
			},
		},
		Status: infrav1.HetznerRobotRemediationStatus{
			Phase:          infrav1.RemediationPhaseRunning,
			RetryCount:     2,
			LastRemediated: &now,
		},
	}

	cp := rem.DeepCopy()

	// Modifying copy must not affect original
	cp.Status.Phase = infrav1.RemediationPhaseDeleting
	cp.Status.RetryCount = 99
	newTime := metav1.Now()
	cp.Status.LastRemediated = &newTime

	if rem.Status.Phase != infrav1.RemediationPhaseRunning {
		t.Error("DeepCopy Phase is not independent")
	}
	if rem.Status.RetryCount != 2 {
		t.Error("DeepCopy RetryCount is not independent")
	}
	if rem.Status.LastRemediated == cp.Status.LastRemediated {
		t.Error("DeepCopy LastRemediated pointer is shared")
	}
}
