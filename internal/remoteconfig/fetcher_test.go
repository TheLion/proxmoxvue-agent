package remoteconfig

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoad_LiveSucceeds(t *testing.T) {
	want := Config{
		SchemaVersion:          1,
		SupabaseBaseURL:        "https://api.proxmoxvue.app",
		SupabasePublishableKey: "sb_publishable_test",
		IssuedAt:               time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC),
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(want)
	}))
	t.Cleanup(srv.Close)

	cache := filepath.Join(t.TempDir(), "rc.json")
	f := NewFetcher(cache)
	f.Endpoint = srv.URL

	got, source := f.Load(context.Background())
	if source != SourceLive {
		t.Fatalf("source: got %s, want live", source)
	}
	if got.SupabaseBaseURL != want.SupabaseBaseURL {
		t.Errorf("base url mismatch: got %q", got.SupabaseBaseURL)
	}
}

func TestLoad_FallsBackToCache(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "rc.json")
	cached := Config{
		SchemaVersion:          1,
		SupabaseBaseURL:        "https://cached.example.com",
		SupabasePublishableKey: "cached_key",
		IssuedAt:               time.Now(),
	}
	data, _ := json.Marshal(cached)
	if err := os.WriteFile(cache, data, 0o644); err != nil {
		t.Fatal(err)
	}

	f := NewFetcher(cache)
	f.Endpoint = "http://127.0.0.1:1" // refused

	got, source := f.Load(context.Background())
	if source != SourceCache {
		t.Fatalf("source: got %s, want cache", source)
	}
	if got.SupabaseBaseURL != cached.SupabaseBaseURL {
		t.Errorf("got %q, want %q", got.SupabaseBaseURL, cached.SupabaseBaseURL)
	}
}

func TestLoad_FallsBackToBakedIn(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "rc.json")
	f := NewFetcher(cache)
	f.Endpoint = "http://127.0.0.1:1"

	got, source := f.Load(context.Background())
	if source != SourceBakedIn {
		t.Fatalf("source: got %s, want baked-in", source)
	}
	if got.SupabaseBaseURL != BakedInDefault.SupabaseBaseURL {
		t.Error("baked-in mismatch")
	}
}
