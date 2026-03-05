// Package robot provides a client for the Hetzner Robot API.
package robot

import (
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
	httpClient *http.Client
}

// New creates a new Robot API client.
func New(username, password string) *Client {
	return &Client{
		username: username,
		password: password,
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
	ServerIP         string `json:"server_ip"`
	ServerNumber     int    `json:"server_number"`
	OS               string `json:"os"`
	Arch             int    `json:"arch"`
	Active           bool   `json:"active"`
	Password         string `json:"password"`
	AuthorizedKeys   []interface{} `json:"authorized_key"`
	HostKey          []interface{} `json:"host_key"`
}

// GetServer returns information about a server by its ID.
func (c *Client) GetServer(serverID int) (*ServerInfo, error) {
	var result struct {
		Server ServerInfo `json:"server"`
	}
	if err := c.get(fmt.Sprintf("/server/%d", serverID), &result); err != nil {
		return nil, err
	}
	return &result.Server, nil
}

// GetServerByIP returns information about a server by its IP.
func (c *Client) GetServerByIP(ip string) (*ServerInfo, error) {
	var result struct {
		Server ServerInfo `json:"server"`
	}
	if err := c.get(fmt.Sprintf("/server/%s", ip), &result); err != nil {
		return nil, err
	}
	return &result.Server, nil
}

// ActivateRescue activates rescue mode for a server.
// Returns the rescue password.
func (c *Client) ActivateRescue(serverID int, sshKeyFingerprint string) (*RescueInfo, error) {
	data := url.Values{}
	data.Set("os", "linux")
	data.Set("arch", "64")
	if sshKeyFingerprint != "" {
		data.Set("authorized_key", sshKeyFingerprint)
	}

	var result struct {
		Rescue RescueInfo `json:"rescue"`
	}
	if err := c.post(fmt.Sprintf("/boot/%d/rescue", serverID), data, &result); err != nil {
		return nil, fmt.Errorf("activate rescue: %w", err)
	}
	return &result.Rescue, nil
}

// GetRescueStatus returns the current rescue mode status.
func (c *Client) GetRescueStatus(serverID int) (*RescueInfo, error) {
	var result struct {
		Rescue RescueInfo `json:"rescue"`
	}
	if err := c.get(fmt.Sprintf("/boot/%d/rescue", serverID), &result); err != nil {
		return nil, err
	}
	return &result.Rescue, nil
}

// DeactivateRescue deactivates rescue mode.
func (c *Client) DeactivateRescue(serverID int) error {
	req, err := http.NewRequest(http.MethodDelete,
		fmt.Sprintf("%s/boot/%d/rescue", robotBaseURL, serverID), nil)
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
func (c *Client) ResetServer(serverID int, resetType ResetType) error {
	data := url.Values{}
	data.Set("type", string(resetType))
	var result interface{}
	if err := c.post(fmt.Sprintf("/reset/%d", serverID), data, &result); err != nil {
		return fmt.Errorf("reset server %d: %w", serverID, err)
	}
	return nil
}

// SetServerName updates the name of a server in Robot.
func (c *Client) SetServerName(serverID int, name string) error {
	data := url.Values{}
	data.Set("server_name", name)
	var result interface{}
	return c.post(fmt.Sprintf("/server/%d", serverID), data, &result)
}

// get performs a GET request to the Robot API.
func (c *Client) get(path string, result interface{}) error {
	req, err := http.NewRequest(http.MethodGet, robotBaseURL+path, nil)
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
func (c *Client) post(path string, data url.Values, result interface{}) error {
	req, err := http.NewRequest(http.MethodPost, robotBaseURL+path,
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
