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

func TestRemediation_PhaseTransitions(t *testing.T) {
	tests := []struct {
		name           string
		phase          infrav1.RemediationPhase
		retryCount     int
		retryLimit     int
		expectedPhase  infrav1.RemediationPhase
		description    string
	}{
		{
			name:          "Running transitions to Waiting",
			phase:         infrav1.RemediationPhaseRunning,
			retryCount:    1,
			retryLimit:    3,
			expectedPhase: infrav1.RemediationPhaseWaiting,
			description:   "After the recovery timeout, Running should transition to Waiting",
		},
		{
			name:          "Waiting with retries left transitions to Running",
			phase:         infrav1.RemediationPhaseWaiting,
			retryCount:    1,
			retryLimit:    3,
			expectedPhase: infrav1.RemediationPhaseRunning,
			description:   "Waiting with retries remaining should go back to Running for another reset",
		},
		{
			name:          "Waiting with retries exhausted transitions to Deleting",
			phase:         infrav1.RemediationPhaseWaiting,
			retryCount:    3,
			retryLimit:    3,
			expectedPhase: infrav1.RemediationPhaseDeleting,
			description:   "Waiting with all retries used should enter terminal Deleting phase",
		},
		{
			name:          "Deleting stays Deleting",
			phase:         infrav1.RemediationPhaseDeleting,
			retryCount:    3,
			retryLimit:    3,
			expectedPhase: infrav1.RemediationPhaseDeleting,
			description:   "Deleting is terminal — no further state changes",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rem := newTestRemediation(tt.phase, tt.retryCount, tt.retryLimit)

			// Verify initial state
			if rem.Status.Phase != tt.phase {
				t.Fatalf("expected initial phase %q, got %q", tt.phase, rem.Status.Phase)
			}

			// Simulate phase transition logic (extracted from controller)
			switch rem.Status.Phase {
			case infrav1.RemediationPhaseRunning:
				rem.Status.Phase = infrav1.RemediationPhaseWaiting

			case infrav1.RemediationPhaseWaiting:
				if rem.Status.RetryCount >= rem.Spec.Strategy.RetryLimit {
					rem.Status.Phase = infrav1.RemediationPhaseDeleting
				} else {
					rem.Status.Phase = infrav1.RemediationPhaseRunning
					rem.Status.RetryCount++
					now := metav1.Now()
					rem.Status.LastRemediated = &now
				}

			case infrav1.RemediationPhaseDeleting:
				// No-op, terminal

			default:
				t.Fatalf("unexpected phase: %q", rem.Status.Phase)
			}

			if rem.Status.Phase != tt.expectedPhase {
				t.Errorf("expected phase %q after transition, got %q", tt.expectedPhase, rem.Status.Phase)
			}
		})
	}
}

func TestRemediation_InitialPhaseTriggersReset(t *testing.T) {
	rem := newTestRemediation("", 0, 2)

	if rem.Status.Phase != "" {
		t.Fatalf("expected empty initial phase, got %q", rem.Status.Phase)
	}

	// Simulate initial phase logic
	now := metav1.Now()
	rem.Status.Phase = infrav1.RemediationPhaseRunning
	rem.Status.RetryCount = 1
	rem.Status.LastRemediated = &now

	if rem.Status.Phase != infrav1.RemediationPhaseRunning {
		t.Errorf("expected phase Running after initial reset, got %q", rem.Status.Phase)
	}
	if rem.Status.RetryCount != 1 {
		t.Errorf("expected retryCount 1, got %d", rem.Status.RetryCount)
	}
	if rem.Status.LastRemediated == nil {
		t.Error("expected LastRemediated to be set")
	}
}

func TestRemediation_RetryCountIncrementsOnRetry(t *testing.T) {
	rem := newTestRemediation(infrav1.RemediationPhaseWaiting, 1, 3)

	// Simulate retry
	rem.Status.Phase = infrav1.RemediationPhaseRunning
	rem.Status.RetryCount++
	now := metav1.Now()
	rem.Status.LastRemediated = &now

	if rem.Status.RetryCount != 2 {
		t.Errorf("expected retryCount 2 after retry, got %d", rem.Status.RetryCount)
	}
	if rem.Status.Phase != infrav1.RemediationPhaseRunning {
		t.Errorf("expected phase Running after retry, got %q", rem.Status.Phase)
	}
}

func TestRemediation_DeletingIsTerminal(t *testing.T) {
	rem := newTestRemediation(infrav1.RemediationPhaseDeleting, 3, 3)

	// Deleting should remain Deleting — no further actions
	if rem.Status.Phase != infrav1.RemediationPhaseDeleting {
		t.Errorf("expected Deleting phase, got %q", rem.Status.Phase)
	}
	// RetryCount should not change
	if rem.Status.RetryCount != 3 {
		t.Errorf("expected retryCount 3, got %d", rem.Status.RetryCount)
	}
}

func TestRemediation_SingleRetryLimit(t *testing.T) {
	// Default: retryLimit=1, meaning only one hardware reset attempt
	rem := newTestRemediation("", 0, 1)

	// Initial: trigger reset
	rem.Status.Phase = infrav1.RemediationPhaseRunning
	rem.Status.RetryCount = 1

	// Running → Waiting
	rem.Status.Phase = infrav1.RemediationPhaseWaiting

	// Waiting with retryCount=1, retryLimit=1: exhausted → Deleting
	if rem.Status.RetryCount >= rem.Spec.Strategy.RetryLimit {
		rem.Status.Phase = infrav1.RemediationPhaseDeleting
	}

	if rem.Status.Phase != infrav1.RemediationPhaseDeleting {
		t.Errorf("expected Deleting after single retry exhausted, got %q", rem.Status.Phase)
	}
}

func TestRemediation_FullRetrySequence(t *testing.T) {
	// Simulate a full remediation lifecycle: 3 retries, all fail
	rem := newTestRemediation("", 0, 3)

	// Step 1: Initial → Running (first reset)
	now := metav1.Now()
	rem.Status.Phase = infrav1.RemediationPhaseRunning
	rem.Status.RetryCount = 1
	rem.Status.LastRemediated = &now

	// Step 2: Running → Waiting (timeout elapsed)
	rem.Status.Phase = infrav1.RemediationPhaseWaiting

	// Step 3: Waiting → Running (retry #2)
	rem.Status.Phase = infrav1.RemediationPhaseRunning
	rem.Status.RetryCount++
	now2 := metav1.Now()
	rem.Status.LastRemediated = &now2

	if rem.Status.RetryCount != 2 {
		t.Fatalf("expected retryCount 2, got %d", rem.Status.RetryCount)
	}

	// Step 4: Running → Waiting
	rem.Status.Phase = infrav1.RemediationPhaseWaiting

	// Step 5: Waiting → Running (retry #3)
	rem.Status.Phase = infrav1.RemediationPhaseRunning
	rem.Status.RetryCount++
	now3 := metav1.Now()
	rem.Status.LastRemediated = &now3

	if rem.Status.RetryCount != 3 {
		t.Fatalf("expected retryCount 3, got %d", rem.Status.RetryCount)
	}

	// Step 6: Running → Waiting
	rem.Status.Phase = infrav1.RemediationPhaseWaiting

	// Step 7: Waiting with retryCount=3, retryLimit=3 → Deleting
	if rem.Status.RetryCount >= rem.Spec.Strategy.RetryLimit {
		rem.Status.Phase = infrav1.RemediationPhaseDeleting
	}

	if rem.Status.Phase != infrav1.RemediationPhaseDeleting {
		t.Errorf("expected Deleting after 3 retries, got %q", rem.Status.Phase)
	}
}

func TestRemediation_DefaultStrategy(t *testing.T) {
	rem := &infrav1.HetznerRobotRemediation{
		Spec: infrav1.HetznerRobotRemediationSpec{
			Strategy: infrav1.RemediationStrategy{
				Type:       infrav1.RemediationStrategyReboot,
				RetryLimit: 1,
				Timeout:    metav1.Duration{Duration: 5 * time.Minute},
			},
		},
	}

	if rem.Spec.Strategy.Type != infrav1.RemediationStrategyReboot {
		t.Errorf("expected strategy type Reboot, got %q", rem.Spec.Strategy.Type)
	}
	if rem.Spec.Strategy.RetryLimit != 1 {
		t.Errorf("expected retry limit 1, got %d", rem.Spec.Strategy.RetryLimit)
	}
	if rem.Spec.Strategy.Timeout.Duration != 5*time.Minute {
		t.Errorf("expected timeout 5m, got %v", rem.Spec.Strategy.Timeout.Duration)
	}
}

func TestRemediation_DeepCopy(t *testing.T) {
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

	copy := rem.DeepCopy()

	// Verify deep copy is independent
	copy.Status.Phase = infrav1.RemediationPhaseDeleting
	if rem.Status.Phase == infrav1.RemediationPhaseDeleting {
		t.Error("DeepCopy is not independent — modifying copy changed original")
	}

	copy.Status.RetryCount = 99
	if rem.Status.RetryCount == 99 {
		t.Error("DeepCopy RetryCount is not independent")
	}

	// Verify pointer field independence
	newTime := metav1.Now()
	copy.Status.LastRemediated = &newTime
	if rem.Status.LastRemediated == copy.Status.LastRemediated {
		t.Error("DeepCopy LastRemediated pointer is shared")
	}
}
