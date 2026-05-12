package supabase

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// ReadCommand is one row from public.read_commands as the agent
// needs it. Endpoint and params are validated against a whitelist by
// the dispatcher; this struct does no checks of its own.
type ReadCommand struct {
	ID        int64           `json:"id"`
	HostID    string          `json:"host_id,omitempty"`
	Endpoint  string          `json:"endpoint"`
	Params    json.RawMessage `json:"params"`
	Status    string          `json:"status"`
	ExpiresAt time.Time       `json:"expires_at"`
}

// ClaimReadCommand atomically flips status=pending → claimed,
// identical to ClaimCommand but against the read_commands table.
func (c *Client) ClaimReadCommand(ctx context.Context, id int64) (bool, error) {
	body := map[string]any{
		"status":     "claimed",
		"claimed_at": time.Now().UTC().Format(time.RFC3339Nano),
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return false, fmt.Errorf("marshal claim: %w", err)
	}
	path := fmt.Sprintf("/read_commands?id=eq.%d&status=eq.pending", id)
	returned, err := c.patchRowReturning(ctx, path, raw)
	if err != nil {
		return false, err
	}
	return len(returned) > 0, nil
}

// PendingReadCommands returns rows still in `pending` for this cluster.
// Used by the catch-up flow on (re)connect to recover events that were
// missed by the Realtime WS (silent disconnect, NAT eviction, etc.).
// The dispatcher's TTL-check decides whether each row is actually
// processed or marked expired — this method just hands them over.
func (c *Client) PendingReadCommands(ctx context.Context, clusterID string) ([]ReadCommand, error) {
	path := fmt.Sprintf(
		"/read_commands?cluster_id=eq.%s&status=eq.pending&select=id,host_id,endpoint,params,status,expires_at&order=id.asc&limit=200",
		clusterID,
	)
	raw, err := c.getRows(ctx, path)
	if err != nil {
		return nil, err
	}
	var rows []ReadCommand
	if err := json.Unmarshal(raw, &rows); err != nil {
		return nil, fmt.Errorf("decode pending read_commands: %w", err)
	}
	return rows, nil
}

// CompleteReadCommand writes status + result + completed_at. On
// failed, result is ignored and the caller is expected to set
// errMsg; result may be nil.
func (c *Client) CompleteReadCommand(ctx context.Context, id int64, status string, result json.RawMessage, errMsg string) error {
	body := map[string]any{
		"status":       status,
		"completed_at": time.Now().UTC().Format(time.RFC3339Nano),
	}
	if result != nil {
		body["result"] = result
	}
	if errMsg != "" {
		body["error"] = errMsg
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal complete: %w", err)
	}
	path := fmt.Sprintf("/read_commands?id=eq.%d", id)
	return c.patchRow(ctx, path, raw)
}
