package supabase

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// ReadCommand is één rij uit public.read_commands zoals de agent die nodig
// heeft. Endpoint en params worden door de dispatcher gevalideerd tegen een
// whitelist; deze struct doet zelf geen check.
type ReadCommand struct {
	ID        int64           `json:"id"`
	HostID    string          `json:"host_id"`
	Endpoint  string          `json:"endpoint"`
	Params    json.RawMessage `json:"params"`
	Status    string          `json:"status"`
	ExpiresAt time.Time       `json:"expires_at"`
}

// ClaimReadCommand zet status=pending → claimed atomair, identiek aan
// ClaimCommand maar dan op de read_commands-tabel.
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

// CompleteReadCommand schrijft status + result + completed_at. Bij failed
// wordt result genegeerd en hoort de caller errMsg te zetten; result mag
// nil zijn.
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
