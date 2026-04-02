package controllers

import (
	"fmt"
	"testing"
	"time"
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
