package supabase

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
)

// PushSnapshot inserts a single row into public.status_snapshots.
// The payload is stored as-is in the jsonb column; captured_at defaults
// server-side. RLS checks that cluster_id is in JWT.cluster_ids.
func (c *Client) PushSnapshot(ctx context.Context, clusterID string, payload json.RawMessage) error {
	body, err := json.Marshal(map[string]any{
		"cluster_id": clusterID,
		"payload":    payload,
	})
	if err != nil {
		return fmt.Errorf("marshal snapshot body: %w", err)
	}

	if err := c.postRow(ctx, "/status_snapshots", body); err != nil {
		return fmt.Errorf("push snapshot: %w", err)
	}
	return nil
}

// getRows GETs a PostgREST endpoint and returns the body bytes (a JSON
// array). On 401 it refreshes once and retries. Caller decodes.
func (c *Client) getRows(ctx context.Context, path string) ([]byte, error) {
	attempt := func(token string) (int, []byte, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.restBase+path, nil)
		if err != nil {
			return 0, nil, fmt.Errorf("build request: %w", err)
		}
		req.Header.Set("apikey", c.publishableKey)
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Accept", "application/json")

		resp, err := c.httpClient.Do(req)
		if err != nil {
			return 0, nil, fmt.Errorf("do request: %w", err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, raw, nil
	}

	token, err := c.access(ctx)
	if err != nil {
		return nil, err
	}
	status, raw, err := attempt(token)
	if err != nil {
		return nil, err
	}
	if status == http.StatusUnauthorized {
		token, err = c.refresh(ctx)
		if err != nil {
			return nil, err
		}
		status, raw, err = attempt(token)
		if err != nil {
			return nil, err
		}
	}
	if status < 200 || status >= 300 {
		return nil, fmt.Errorf("GET %s: status %d: %s", path, status, string(raw))
	}
	return raw, nil
}

// postRow POSTs a single row to a PostgREST endpoint under /rest/v1.
// On 401 it refreshes once and retries.
func (c *Client) postRow(ctx context.Context, path string, body []byte) error {
	attempt := func(token string) (int, string, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.restBase+path, bytes.NewReader(body))
		if err != nil {
			return 0, "", fmt.Errorf("build request: %w", err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("apikey", c.publishableKey)
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Prefer", "return=minimal")

		resp, err := c.httpClient.Do(req)
		if err != nil {
			return 0, "", fmt.Errorf("do request: %w", err)
		}
		defer resp.Body.Close()
		slog.Debug("supabase HTTP", "method", req.Method, "url", req.URL.String(), "status", resp.StatusCode)

		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return resp.StatusCode, "", nil
		}
		raw, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(raw), nil
	}

	token, err := c.access(ctx)
	if err != nil {
		return err
	}

	status, respBody, err := attempt(token)
	if err != nil {
		return err
	}
	if status == http.StatusUnauthorized {
		token, err = c.refresh(ctx)
		if err != nil {
			return err
		}
		status, respBody, err = attempt(token)
		if err != nil {
			return err
		}
	}
	if status < 200 || status >= 300 {
		return fmt.Errorf("unexpected status %d: %s", status, respBody)
	}
	return nil
}
