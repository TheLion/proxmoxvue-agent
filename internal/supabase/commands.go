package supabase

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Command is één rij uit public.commands zoals de agent die nodig heeft.
// HostID is informatief (welke node-context iOS bedoelde); cluster_id is
// de claim-key en zit niet in deze struct want de agent kent z'n cluster
// al via config.
type Command struct {
	ID        int64           `json:"id"`
	HostID    string          `json:"host_id,omitempty"`
	Kind      string          `json:"kind"`
	Payload   json.RawMessage `json:"payload"`
	Status    string          `json:"status"`
	ExpiresAt time.Time       `json:"expires_at"`
}

// ClaimCommand probeert de rij met id en status=pending atomair naar
// status=claimed te zetten. Retourneert true als de update een rij raakte
// (wij zijn de claimant), false als de rij al geclaimed/afgerond is.
func (c *Client) ClaimCommand(ctx context.Context, id int64) (bool, error) {
	body := map[string]any{
		"status":     "claimed",
		"claimed_at": time.Now().UTC().Format(time.RFC3339Nano),
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return false, fmt.Errorf("marshal claim: %w", err)
	}
	path := fmt.Sprintf("/commands?id=eq.%d&status=eq.pending", id)
	returned, err := c.patchRowReturning(ctx, path, raw)
	if err != nil {
		return false, err
	}
	return len(returned) > 0, nil
}

// CompleteCommand zet status + result + completed_at in één PATCH.
// status moet "done", "failed" of "expired" zijn.
func (c *Client) CompleteCommand(ctx context.Context, id int64, status string, result map[string]any) error {
	body := map[string]any{
		"status":       status,
		"completed_at": time.Now().UTC().Format(time.RFC3339Nano),
		"result":       result,
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal complete: %w", err)
	}
	path := fmt.Sprintf("/commands?id=eq.%d", id)
	return c.patchRow(ctx, path, raw)
}

// patchRowReturning PATCH't met Prefer: return=representation en decodeert
// de respons als een slice van JSON objecten. Bij 401 wordt één keer de
// token gerefreshed en opnieuw geprobeerd.
func (c *Client) patchRowReturning(ctx context.Context, path string, body []byte) ([]json.RawMessage, error) {
	status, raw, err := c.patchWithAuth(ctx, path, body, "return=representation")
	if err != nil {
		return nil, err
	}
	if status < 200 || status >= 300 {
		return nil, fmt.Errorf("PATCH %s: status %d: %s", path, status, string(raw))
	}
	var rows []json.RawMessage
	if err := json.Unmarshal(raw, &rows); err != nil {
		return nil, fmt.Errorf("parse response: %w", err)
	}
	return rows, nil
}

// patchRow PATCH't met Prefer: return=minimal — geen response-body nodig.
func (c *Client) patchRow(ctx context.Context, path string, body []byte) error {
	status, raw, err := c.patchWithAuth(ctx, path, body, "return=minimal")
	if err != nil {
		return err
	}
	if status < 200 || status >= 300 {
		return fmt.Errorf("PATCH %s: status %d: %s", path, status, string(raw))
	}
	return nil
}

// patchWithAuth voert een PATCH uit met automatische token-refresh-on-401.
func (c *Client) patchWithAuth(ctx context.Context, path string, body []byte, preferHeader string) (int, []byte, error) {
	attempt := func(token string) (int, []byte, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPatch, c.restBase+path, bytes.NewReader(body))
		if err != nil {
			return 0, nil, fmt.Errorf("build request: %w", err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("apikey", PublishableKey)
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Prefer", preferHeader)

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
		return 0, nil, err
	}
	status, raw, err := attempt(token)
	if err != nil {
		return 0, nil, err
	}
	if status == http.StatusUnauthorized {
		token, err = c.refresh(ctx)
		if err != nil {
			return 0, nil, err
		}
		return attempt(token)
	}
	return status, raw, nil
}
