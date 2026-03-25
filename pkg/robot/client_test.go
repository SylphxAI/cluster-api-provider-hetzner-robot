package robot

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// testCredentials used throughout all tests.
const (
	testUser = "test-user"
	testPass = "test-pass"
)

// newTestClient creates a Client pointed at the given httptest server.
func newTestClient(serverURL string) *Client {
	return NewWithBaseURL(testUser, testPass, serverURL)
}

// assertBasicAuth verifies the request carries correct Basic Auth credentials.
// Returns false and fails the test if auth is missing or wrong.
func assertBasicAuth(t *testing.T, r *http.Request) {
	t.Helper()
	user, pass, ok := r.BasicAuth()
	if !ok {
		t.Fatal("expected Basic Auth header, got none")
	}
	if user != testUser || pass != testPass {
		t.Fatalf("unexpected credentials: got %q:%q, want %q:%q", user, pass, testUser, testPass)
	}
}

// --- NewWithBaseURL / New constructors ---

func TestNew_UsesDefaultBaseURL(t *testing.T) {
	c := New("u", "p")
	if c.baseURL != robotBaseURL {
		t.Fatalf("New() baseURL = %q, want %q", c.baseURL, robotBaseURL)
	}
}

func TestNewWithBaseURL_SetsCustomURL(t *testing.T) {
	c := NewWithBaseURL("u", "p", "http://localhost:9999")
	if c.baseURL != "http://localhost:9999" {
		t.Fatalf("NewWithBaseURL() baseURL = %q, want %q", c.baseURL, "http://localhost:9999")
	}
}

// --- GetServer ---

func TestGetServer_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertBasicAuth(t, r)
		if r.Method != http.MethodGet {
			t.Fatalf("expected GET, got %s", r.Method)
		}
		if r.URL.Path != "/server/123" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"server": map[string]any{
				"server_number": 123,
				"server_name":   "my-server",
				"server_ip":     "1.2.3.4",
				"product":       "EX44",
				"dc":            "FSN1-DC14",
				"status":        "ready",
				"cancelled":     false,
			},
		})
	}))
	defer srv.Close()

	c := newTestClient(srv.URL)
	info, err := c.GetServer(context.Background(), 123)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if info.ServerNumber != 123 {
		t.Errorf("ServerNumber = %d, want 123", info.ServerNumber)
	}
	if info.ServerName != "my-server" {
		t.Errorf("ServerName = %q, want %q", info.ServerName, "my-server")
	}
	if info.ServerIP != "1.2.3.4" {
		t.Errorf("ServerIP = %q, want %q", info.ServerIP, "1.2.3.4")
	}
	if info.Product != "EX44" {
		t.Errorf("Product = %q, want %q", info.Product, "EX44")
	}
	if info.Datacenter != "FSN1-DC14" {
		t.Errorf("Datacenter = %q, want %q", info.Datacenter, "FSN1-DC14")
	}
	if info.Status != "ready" {
		t.Errorf("Status = %q, want %q", info.Status, "ready")
	}
	if info.Cancelled {
		t.Error("Cancelled = true, want false")
	}
}

func TestGetServer_404(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertBasicAuth(t, r)
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"error":{"status":404,"code":"SERVER_NOT_FOUND","message":"Server not found"}}`))
	}))
	defer srv.Close()

	c := newTestClient(srv.URL)
	_, err := c.GetServer(context.Background(), 999)
	if err == nil {
		t.Fatal("expected error for 404, got nil")
	}
	expected := "robot API error 404"
	if got := err.Error(); !contains(got, expected) {
		t.Errorf("error = %q, want it to contain %q", got, expected)
	}
}

func TestGetServer_BasicAuth(t *testing.T) {
	var receivedUser, receivedPass string
	var authPresent bool

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedUser, receivedPass, authPresent = r.BasicAuth()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"server": map[string]any{
				"server_number": 1,
				"server_name":   "",
				"server_ip":     "",
			},
		})
	}))
	defer srv.Close()

	c := newTestClient(srv.URL)
	_, _ = c.GetServer(context.Background(), 1)

	if !authPresent {
		t.Fatal("Basic Auth not present on request")
	}
	if receivedUser != testUser {
		t.Errorf("auth user = %q, want %q", receivedUser, testUser)
	}
	if receivedPass != testPass {
		t.Errorf("auth pass = %q, want %q", receivedPass, testPass)
	}
}

// --- GetServerByIP ---

func TestGetServerByIP_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertBasicAuth(t, r)
		if r.URL.Path != "/server/10.0.0.1" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"server": map[string]any{
				"server_number": 42,
				"server_name":   "ip-lookup",
				"server_ip":     "10.0.0.1",
			},
		})
	}))
	defer srv.Close()

	c := newTestClient(srv.URL)
	info, err := c.GetServerByIP(context.Background(), "10.0.0.1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if info.ServerNumber != 42 {
		t.Errorf("ServerNumber = %d, want 42", info.ServerNumber)
	}
	if info.ServerIP != "10.0.0.1" {
		t.Errorf("ServerIP = %q, want %q", info.ServerIP, "10.0.0.1")
	}
}

// --- ActivateRescue ---

func TestActivateRescue_Success(t *testing.T) {
	tests := []struct {
		name           string
		fingerprint    string
		wantAuthKey    bool
		wantAuthKeyVal string
	}{
		{
			name:           "with SSH fingerprint",
			fingerprint:    "ab:cd:ef:01:23:45",
			wantAuthKey:    true,
			wantAuthKeyVal: "ab:cd:ef:01:23:45",
		},
		{
			name:        "without SSH fingerprint",
			fingerprint: "",
			wantAuthKey: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assertBasicAuth(t, r)
				if r.Method != http.MethodPost {
					t.Fatalf("expected POST, got %s", r.Method)
				}
				if r.URL.Path != "/boot/123/rescue" {
					t.Fatalf("unexpected path: %s", r.URL.Path)
				}
				if ct := r.Header.Get("Content-Type"); ct != "application/x-www-form-urlencoded" {
					t.Fatalf("unexpected Content-Type: %s", ct)
				}

				if err := r.ParseForm(); err != nil {
					t.Fatalf("failed to parse form: %v", err)
				}

				if r.PostForm.Get("os") != "linux" {
					t.Errorf("os = %q, want %q", r.PostForm.Get("os"), "linux")
				}
				if r.PostForm.Get("arch") != "64" {
					t.Errorf("arch = %q, want %q", r.PostForm.Get("arch"), "64")
				}

				authKey := r.PostForm.Get("authorized_key")
				if tt.wantAuthKey {
					if authKey != tt.wantAuthKeyVal {
						t.Errorf("authorized_key = %q, want %q", authKey, tt.wantAuthKeyVal)
					}
				} else {
					if authKey != "" {
						t.Errorf("authorized_key = %q, want empty", authKey)
					}
				}

				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(map[string]any{
					"rescue": map[string]any{
						"server_ip":     "1.2.3.4",
						"server_number": 123,
						"os":            "linux",
						"arch":          64,
						"active":        true,
						"password":      "rescue-pass-123",
					},
				})
			}))
			defer srv.Close()

			c := newTestClient(srv.URL)
			rescue, err := c.ActivateRescue(context.Background(), 123, tt.fingerprint)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if rescue.ServerNumber != 123 {
				t.Errorf("ServerNumber = %d, want 123", rescue.ServerNumber)
			}
			if rescue.Password != "rescue-pass-123" {
				t.Errorf("Password = %q, want %q", rescue.Password, "rescue-pass-123")
			}
			if !rescue.Active {
				t.Error("Active = false, want true")
			}
			// OS and Arch are json.RawMessage (Hetzner returns string or []string / int or []int
			// depending on rescue state). Verify they are non-empty rather than comparing typed values.
			if len(rescue.OS) == 0 {
				t.Error("OS is empty")
			}
			if len(rescue.Arch) == 0 {
				t.Error("Arch is empty")
			}
		})
	}
}

func TestActivateRescue_APIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertBasicAuth(t, r)
		w.WriteHeader(http.StatusConflict)
		w.Write([]byte(`{"error":{"status":409,"code":"RESCUE_ALREADY_ACTIVE","message":"Rescue system already active"}}`))
	}))
	defer srv.Close()

	c := newTestClient(srv.URL)
	_, err := c.ActivateRescue(context.Background(), 123, "")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !contains(err.Error(), "activate rescue") {
		t.Errorf("error = %q, want it to contain %q", err.Error(), "activate rescue")
	}
	if !contains(err.Error(), "409") {
		t.Errorf("error = %q, want it to contain status code 409", err.Error())
	}
}

// --- GetRescueStatus ---

func TestGetRescueStatus_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertBasicAuth(t, r)
		if r.Method != http.MethodGet {
			t.Fatalf("expected GET, got %s", r.Method)
		}
		if r.URL.Path != "/boot/456/rescue" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"rescue": map[string]any{
				"server_ip":     "5.6.7.8",
				"server_number": 456,
				"os":            "linux",
				"arch":          64,
				"active":        false,
				"password":      "",
			},
		})
	}))
	defer srv.Close()

	c := newTestClient(srv.URL)
	rescue, err := c.GetRescueStatus(context.Background(), 456)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rescue.ServerNumber != 456 {
		t.Errorf("ServerNumber = %d, want 456", rescue.ServerNumber)
	}
	if rescue.Active {
		t.Error("Active = true, want false")
	}
	if rescue.ServerIP != "5.6.7.8" {
		t.Errorf("ServerIP = %q, want %q", rescue.ServerIP, "5.6.7.8")
	}
}

func TestGetRescueStatus_APIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertBasicAuth(t, r)
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`internal server error`))
	}))
	defer srv.Close()

	c := newTestClient(srv.URL)
	_, err := c.GetRescueStatus(context.Background(), 456)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !contains(err.Error(), "500") {
		t.Errorf("error = %q, want it to contain status code 500", err.Error())
	}
}

// --- DeactivateRescue ---

func TestDeactivateRescue_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertBasicAuth(t, r)
		if r.Method != http.MethodDelete {
			t.Fatalf("expected DELETE, got %s", r.Method)
		}
		if r.URL.Path != "/boot/789/rescue" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"rescue":{"server_ip":"9.8.7.6","active":false}}`))
	}))
	defer srv.Close()

	c := newTestClient(srv.URL)
	err := c.DeactivateRescue(context.Background(), 789)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestDeactivateRescue_APIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertBasicAuth(t, r)
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"error":{"status":404,"code":"NOT_FOUND","message":"Server not found"}}`))
	}))
	defer srv.Close()

	c := newTestClient(srv.URL)
	err := c.DeactivateRescue(context.Background(), 789)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !contains(err.Error(), "404") {
		t.Errorf("error = %q, want it to contain status code 404", err.Error())
	}
}

// --- ResetServer ---

func TestResetServer_Success(t *testing.T) {
	tests := []struct {
		name      string
		resetType ResetType
		wantParam string
	}{
		{name: "software reset", resetType: ResetTypeSoftware, wantParam: "sw"},
		{name: "hardware reset", resetType: ResetTypeHardware, wantParam: "hw"},
		{name: "power reset", resetType: ResetTypePower, wantParam: "power"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assertBasicAuth(t, r)
				if r.Method != http.MethodPost {
					t.Fatalf("expected POST, got %s", r.Method)
				}
				if r.URL.Path != "/reset/100" {
					t.Fatalf("unexpected path: %s", r.URL.Path)
				}

				if err := r.ParseForm(); err != nil {
					t.Fatalf("failed to parse form: %v", err)
				}

				if r.PostForm.Get("type") != tt.wantParam {
					t.Errorf("type = %q, want %q", r.PostForm.Get("type"), tt.wantParam)
				}

				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(map[string]any{
					"reset": map[string]any{
						"server_number": 100,
						"type":          tt.wantParam,
					},
				})
			}))
			defer srv.Close()

			c := newTestClient(srv.URL)
			err := c.ResetServer(context.Background(), 100, tt.resetType)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestResetServer_APIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertBasicAuth(t, r)
		w.WriteHeader(http.StatusConflict)
		w.Write([]byte(`{"error":{"status":409,"code":"RESET_MANUAL_ACTIVE","message":"Manual reset already active"}}`))
	}))
	defer srv.Close()

	c := newTestClient(srv.URL)
	err := c.ResetServer(context.Background(), 100, ResetTypeSoftware)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !contains(err.Error(), "reset server 100") {
		t.Errorf("error = %q, want it to contain %q", err.Error(), "reset server 100")
	}
	if !contains(err.Error(), "409") {
		t.Errorf("error = %q, want it to contain status code 409", err.Error())
	}
}

// --- SetServerName ---

func TestSetServerName_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertBasicAuth(t, r)
		if r.Method != http.MethodPost {
			t.Fatalf("expected POST, got %s", r.Method)
		}
		if r.URL.Path != "/server/200" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}

		if err := r.ParseForm(); err != nil {
			t.Fatalf("failed to parse form: %v", err)
		}

		if r.PostForm.Get("server_name") != "new-name" {
			t.Errorf("server_name = %q, want %q", r.PostForm.Get("server_name"), "new-name")
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"server": map[string]any{
				"server_number": 200,
				"server_name":   "new-name",
			},
		})
	}))
	defer srv.Close()

	c := newTestClient(srv.URL)
	err := c.SetServerName(context.Background(), 200, "new-name")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// --- Context cancellation ---

func TestContextCancellation_GET(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Simulate a slow endpoint; the request should be cancelled before it completes.
		<-r.Context().Done()
	}))
	defer srv.Close()

	c := newTestClient(srv.URL)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	_, err := c.GetServer(ctx, 1)
	if err == nil {
		t.Fatal("expected error from cancelled context, got nil")
	}
}

func TestContextCancellation_POST(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer srv.Close()

	c := newTestClient(srv.URL)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := c.ActivateRescue(ctx, 1, "")
	if err == nil {
		t.Fatal("expected error from cancelled context, got nil")
	}
}

func TestContextCancellation_DELETE(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer srv.Close()

	c := newTestClient(srv.URL)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := c.DeactivateRescue(ctx, 1)
	if err == nil {
		t.Fatal("expected error from cancelled context, got nil")
	}
}

// --- Auth on every method ---

func TestBasicAuth_AllMethods(t *testing.T) {
	// Verify that every public method sends Basic Auth credentials.
	// Each entry is a function that invokes one method on the client.
	type methodCall struct {
		name string
		call func(c *Client) error
	}

	calls := []methodCall{
		{"GetServer", func(c *Client) error { _, err := c.GetServer(context.Background(), 1); return err }},
		{"GetServerByIP", func(c *Client) error { _, err := c.GetServerByIP(context.Background(), "1.2.3.4"); return err }},
		{"GetRescueStatus", func(c *Client) error { _, err := c.GetRescueStatus(context.Background(), 1); return err }},
		{"ActivateRescue", func(c *Client) error { _, err := c.ActivateRescue(context.Background(), 1, ""); return err }},
		{"DeactivateRescue", func(c *Client) error { return c.DeactivateRescue(context.Background(), 1) }},
		{"ResetServer", func(c *Client) error { return c.ResetServer(context.Background(), 1, ResetTypeSoftware) }},
		{"SetServerName", func(c *Client) error { return c.SetServerName(context.Background(), 1, "n") }},
	}

	for _, mc := range calls {
		t.Run(mc.name, func(t *testing.T) {
			var gotAuth bool
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				u, p, ok := r.BasicAuth()
				if ok && u == testUser && p == testPass {
					gotAuth = true
				}
				w.Header().Set("Content-Type", "application/json")
				// Return a generic valid JSON body that satisfies all methods.
				fmt.Fprint(w, `{"server":{"server_number":1},"rescue":{"server_number":1},"reset":{"server_number":1}}`)
			}))
			defer srv.Close()

			c := newTestClient(srv.URL)
			_ = mc.call(c)

			if !gotAuth {
				t.Errorf("Basic Auth not sent for %s", mc.name)
			}
		})
	}
}

// --- helpers ---

// contains reports whether s contains substr (avoids importing strings in test).
func contains(s, substr string) bool {
	return len(s) >= len(substr) && searchSubstring(s, substr)
}

func searchSubstring(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

