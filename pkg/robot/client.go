// Package robot provides a client for the Hetzner Robot API.
package robot

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const robotBaseURL = "https://robot-ws.your-server.de"

// Client is the Hetzner Robot API client.
type Client struct {
	username   string
	password   string
	baseURL    string
	httpClient *http.Client
}

// New creates a new Robot API client.
func New(username, password string) *Client {
	return &Client{
		username: username,
		password: password,
		baseURL:  robotBaseURL,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// NewWithBaseURL creates a new Robot API client with a custom base URL.
// This is primarily useful for testing with httptest servers.
func NewWithBaseURL(username, password, baseURL string) *Client {
	return &Client{
		username: username,
		password: password,
		baseURL:  baseURL,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// ServerInfo contains information about a server.
type ServerInfo struct {
	ServerNumber int    `json:"server_number"`
	ServerName   string `json:"server_name"`
	ServerIP     string `json:"server_ip"`
	Product      string `json:"product"`
	Datacenter   string `json:"dc"`
	Status       string `json:"status"`
	Cancelled    bool   `json:"cancelled"`
}

// RescueInfo contains information about the rescue system.
type RescueInfo struct {
	ServerIP       string `json:"server_ip"`
	ServerNumber   int    `json:"server_number"`
	OS             string `json:"os"`
	Arch           int    `json:"arch"`
	Active         bool   `json:"active"`
	Password       string `json:"password"`
	AuthorizedKeys []any  `json:"authorized_key"`
	HostKey        []any  `json:"host_key"`
}

// GetServer returns information about a server by its ID.
func (c *Client) GetServer(ctx context.Context, serverID int) (*ServerInfo, error) {
	var result struct {
		Server ServerInfo `json:"server"`
	}
	if err := c.get(ctx, fmt.Sprintf("/server/%d", serverID), &result); err != nil {
		return nil, err
	}
	return &result.Server, nil
}

// GetServerByIP returns information about a server by its IP.
func (c *Client) GetServerByIP(ctx context.Context, ip string) (*ServerInfo, error) {
	var result struct {
		Server ServerInfo `json:"server"`
	}
	if err := c.get(ctx, fmt.Sprintf("/server/%s", ip), &result); err != nil {
		return nil, err
	}
	return &result.Server, nil
}

// ActivateRescue activates rescue mode for a server.
// Returns the rescue password.
func (c *Client) ActivateRescue(ctx context.Context, serverID int, sshKeyFingerprint string) (*RescueInfo, error) {
	data := url.Values{}
	data.Set("os", "linux")
	data.Set("arch", "64")
	if sshKeyFingerprint != "" {
		data.Set("authorized_key", sshKeyFingerprint)
	}

	var result struct {
		Rescue RescueInfo `json:"rescue"`
	}
	if err := c.post(ctx, fmt.Sprintf("/boot/%d/rescue", serverID), data, &result); err != nil {
		return nil, fmt.Errorf("activate rescue: %w", err)
	}
	return &result.Rescue, nil
}

// GetRescueStatus returns the current rescue mode status.
func (c *Client) GetRescueStatus(ctx context.Context, serverID int) (*RescueInfo, error) {
	var result struct {
		Rescue RescueInfo `json:"rescue"`
	}
	if err := c.get(ctx, fmt.Sprintf("/boot/%d/rescue", serverID), &result); err != nil {
		return nil, err
	}
	return &result.Rescue, nil
}

// DeactivateRescue deactivates rescue mode.
func (c *Client) DeactivateRescue(ctx context.Context, serverID int) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete,
		fmt.Sprintf("%s/boot/%d/rescue", c.baseURL, serverID), nil)
	if err != nil {
		return err
	}
	req.SetBasicAuth(c.username, c.password)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("robot API error %d: %s", resp.StatusCode, string(body))
	}
	return nil
}

// ResetType defines the type of hardware reset.
type ResetType string

const (
	ResetTypeSoftware ResetType = "sw"
	ResetTypeHardware ResetType = "hw"
	ResetTypePower    ResetType = "power"
)

// ResetServer sends a reset/reboot signal to the server.
func (c *Client) ResetServer(ctx context.Context, serverID int, resetType ResetType) error {
	data := url.Values{}
	data.Set("type", string(resetType))
	var result any
	if err := c.post(ctx, fmt.Sprintf("/reset/%d", serverID), data, &result); err != nil {
		return fmt.Errorf("reset server %d: %w", serverID, err)
	}
	return nil
}

// SetServerName updates the name of a server in Robot.
func (c *Client) SetServerName(ctx context.Context, serverID int, name string) error {
	data := url.Values{}
	data.Set("server_name", name)
	var result any
	return c.post(ctx, fmt.Sprintf("/server/%d", serverID), data, &result)
}

// get performs a GET request to the Robot API.
func (c *Client) get(ctx context.Context, path string, result any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return err
	}
	req.SetBasicAuth(c.username, c.password)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("robot API GET %s: %w", path, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode >= 400 {
		return fmt.Errorf("robot API error %d on GET %s: %s", resp.StatusCode, path, string(body))
	}
	return json.Unmarshal(body, result)
}

// post performs a POST request to the Robot API.
func (c *Client) post(ctx context.Context, path string, data url.Values, result any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path,
		strings.NewReader(data.Encode()))
	if err != nil {
		return err
	}
	req.SetBasicAuth(c.username, c.password)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("robot API POST %s: %w", path, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode >= 400 {
		return fmt.Errorf("robot API error %d on POST %s: %s", resp.StatusCode, path, string(body))
	}
	if result != nil {
		return json.Unmarshal(body, result)
	}
	return nil
}
