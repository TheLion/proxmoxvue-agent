// Package proxmox is a minimal HTTP client for the Proxmox VE REST API.
// The agent only needs a tiny slice of the API (version + cluster
// resources for snapshot payloads, plus action endpoints in later
// iterations), so this is handwritten rather than pulling a third-party
// SDK.
package proxmox

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type Config struct {
	APIURL         string
	APITokenID     string
	APITokenSecret string
	VerifyTLS      bool
}

func (c Config) Valid() error {
	if strings.TrimSpace(c.APIURL) == "" {
		return fmt.Errorf("proxmox.api_url is empty")
	}
	if !strings.HasPrefix(c.APIURL, "https://") && !strings.HasPrefix(c.APIURL, "http://") {
		return fmt.Errorf("proxmox.api_url must start with https:// or http://")
	}
	if strings.TrimSpace(c.APITokenID) == "" {
		return fmt.Errorf("proxmox.api_token_id is empty")
	}
	if strings.TrimSpace(c.APITokenSecret) == "" {
		return fmt.Errorf("proxmox.api_token_secret is empty")
	}
	return nil
}

type Client struct {
	cfg        Config
	httpClient *http.Client
	authHeader string
}

func New(cfg Config) *Client {
	transport := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: !cfg.VerifyTLS}, // #nosec G402 -- Proxmox default self-signed cert, opt-in verify
	}
	return &Client{
		cfg:        cfg,
		httpClient: &http.Client{Timeout: 15 * time.Second, Transport: transport},
		authHeader: fmt.Sprintf("PVEAPIToken=%s=%s", cfg.APITokenID, cfg.APITokenSecret),
	}
}

// Version calls /api2/json/version. Used as a startup health-check.
func (c *Client) Version(ctx context.Context) (map[string]any, error) {
	var wrapper struct {
		Data map[string]any `json:"data"`
	}
	if err := c.getJSON(ctx, "/api2/json/version", &wrapper); err != nil {
		return nil, fmt.Errorf("proxmox version: %w", err)
	}
	return wrapper.Data, nil
}

// ClusterResources returns the decoded `data` array from
// /api2/json/cluster/resources. Each entry contains node/VM/LXC/storage
// info flattened by Proxmox itself.
func (c *Client) ClusterResources(ctx context.Context) (json.RawMessage, error) {
	var wrapper struct {
		Data json.RawMessage `json:"data"`
	}
	if err := c.getJSON(ctx, "/api2/json/cluster/resources", &wrapper); err != nil {
		return nil, fmt.Errorf("proxmox cluster/resources: %w", err)
	}
	return wrapper.Data, nil
}

// GetRaw doet een generieke GET op een willekeurig Proxmox-pad en geeft de
// volledige response-body terug. Gebruikt door de read-RPC dispatcher; alle
// path-validatie gebeurt daar (whitelist), niet hier — deze functie is een
// dumme passthrough.
func (c *Client) GetRaw(ctx context.Context, path string) (json.RawMessage, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.cfg.APIURL+path, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", c.authHeader)
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("do request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("status %d: %s", resp.StatusCode, truncate(string(body), 200))
	}
	return body, nil
}

func (c *Client) getJSON(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.cfg.APIURL+path, nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", c.authHeader)
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("do request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("status %d: %s", resp.StatusCode, truncate(string(body), 200))
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("parse response: %w", err)
	}
	return nil
}

// deleteJSON DELETE't een resource. Gebruikt door snapshot.delete — Proxmox
// returnt een UPID-envelope zoals de POST-endpoints.
func (c *Client) deleteJSON(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.cfg.APIURL+path, nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", c.authHeader)
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("do request: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("status %d: %s", resp.StatusCode, truncate(string(raw), 200))
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("parse response: %w", err)
		}
	}
	return nil
}

// postForm POST't een url.Values als application/x-www-form-urlencoded body —
// het formaat dat Proxmox-write-endpoints (snapshot, vm-create, etc.) eisen.
func (c *Client) postForm(ctx context.Context, path string, form url.Values, out any) error {
	body := strings.NewReader(form.Encode())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.APIURL+path, body)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", c.authHeader)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("do request: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("status %d: %s", resp.StatusCode, truncate(string(raw), 200))
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("parse response: %w", err)
		}
	}
	return nil
}

func (c *Client) postJSON(ctx context.Context, path string, body []byte, out any) error {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.APIURL+path, reader)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", c.authHeader)
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("do request: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("status %d: %s", resp.StatusCode, truncate(string(raw), 200))
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("parse response: %w", err)
		}
	}
	return nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
