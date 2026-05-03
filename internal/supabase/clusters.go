package supabase

import (
	"context"
	"encoding/json"
	"fmt"
)

// UploadClusterPublicKey writes the HPKE public key to
// `clusters.public_key` for the agent's own cluster_id. Idempotent:
// just overwrite on identical content, no check-and-skip needed
// (Postgres bytea comparison is cheap). RLS policy "clusters: agent
// update" allows this from the agent role.
//
// Postgres' bytea column expects a hex-encoded string with `\x` prefix
// in JSON via PostgREST; PostgREST decodes that automatically.
func (c *Client) UploadClusterPublicKey(ctx context.Context, clusterID string, publicKey []byte) error {
	body, err := json.Marshal(map[string]any{"public_key": bytesToBytea(publicKey)})
	if err != nil {
		return fmt.Errorf("marshal public_key body: %w", err)
	}
	if err := c.patchRow(ctx, "/clusters?id=eq."+clusterID, body); err != nil {
		return fmt.Errorf("upload cluster public_key: %w", err)
	}
	return nil
}

// bytesToBytea encodes raw bytes to PostgREST's bytea format
// `\xDEADBEEF...` so the value lands in a bytea column.
func bytesToBytea(b []byte) string {
	const hexdigits = "0123456789abcdef"
	out := make([]byte, 2+len(b)*2)
	out[0] = '\\'
	out[1] = 'x'
	for i, v := range b {
		out[2+i*2] = hexdigits[v>>4]
		out[2+i*2+1] = hexdigits[v&0x0f]
	}
	return string(out)
}
