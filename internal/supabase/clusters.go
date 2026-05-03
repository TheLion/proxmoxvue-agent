package supabase

import (
	"context"
	"encoding/json"
	"fmt"
)

// UploadClusterPublicKey schrijft de HPKE public key naar
// `clusters.public_key` voor de eigen cluster_id. Idempotent: bij gelijke
// content gewoon overschrijven, geen check-and-skip nodig (Postgres
// vergelijking op bytea is goedkoop). RLS-policy "clusters: agent update"
// staat dit toe vanuit de agent-rol.
//
// Postgres' bytea-kolom verwacht in JSON via PostgREST een hex-encoded
// string met `\x`-prefix; PostgREST decodet die automatisch.
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

// bytesToBytea encodeert raw bytes naar het PostgREST-bytea-formaat
// `\xDEADBEEF...` zodat de waarde in een bytea-kolom landt.
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
