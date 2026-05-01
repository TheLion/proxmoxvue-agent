package supabase

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// PushSnapshot inserts a single row into public.status_snapshots.
// The payload is stored as-is in the jsonb column; captured_at defaults
// server-side. RLS checks dat cluster_id in JWT.cluster_ids zit.
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

// postRow POSTs a single row to a PostgREST endpoint under /rest/v1.
// On 401 it refreshes once and retries.
func (c *Client) postRow(ctx context.Context, path string, body []byte) error {
	attempt := func(token string) (int, string, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.restBase+path, bytes.NewReader(body))
		if err != nil {
			return 0, "", fmt.Errorf("build request: %w", err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("apikey", PublishableKey)
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Prefer", "return=minimal")

		resp, err := c.httpClient.Do(req)
		if err != nil {
			return 0, "", fmt.Errorf("do request: %w", err)
		}
		defer resp.Body.Close()

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
