package config

import (
	"fmt"
	"strings"
	"testing"
)

// TestSupabaseConfigStringRedactsToken guards against accidentally reintroducing
// plain-token rendering in logs via %+v formatting.
func TestSupabaseConfigStringRedactsToken(t *testing.T) {
	cfg := SupabaseConfig{
		ProjectRef:   "abc",
		HostID:       "host-uuid",
		RefreshToken: "super-secret-token-xyz",
	}
	rendered := fmt.Sprintf("%+v", cfg)
	if strings.Contains(rendered, "super-secret-token-xyz") {
		t.Fatalf("refresh_token leaked in Stringer: %s", rendered)
	}
	if !strings.Contains(rendered, "[REDACTED]") {
		t.Fatalf("expected [REDACTED] marker, got: %s", rendered)
	}
}

func TestProxmoxConfigStringRedactsSecret(t *testing.T) {
	cfg := ProxmoxConfig{
		APIURL:         "https://pve.local:8006",
		APITokenID:     "token-id",
		APITokenSecret: "super-secret-api-token",
	}
	rendered := fmt.Sprintf("%+v", cfg)
	if strings.Contains(rendered, "super-secret-api-token") {
		t.Fatalf("api_token_secret leaked in Stringer: %s", rendered)
	}
}

func TestUnsetTokenShowsUnset(t *testing.T) {
	cfg := SupabaseConfig{ProjectRef: "abc"}
	rendered := cfg.String()
	if !strings.Contains(rendered, "<unset>") {
		t.Fatalf("expected <unset> marker for empty token, got: %s", rendered)
	}
}
