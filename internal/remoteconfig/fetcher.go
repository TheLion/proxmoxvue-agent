// Package remoteconfig fetches the server-driven config JSON used to
// override Supabase URL / publishable key without a binary release.
//
// On every agent start (and every refreshInterval thereafter), Fetch()
// hits the endpoint and writes a fresh cache file. If the endpoint is
// unreachable, the cached file is used. If neither exist, the
// BakedInDefault is returned.
package remoteconfig

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

const (
	DefaultEndpoint        = "https://proxmoxvue.app/config/v1.json"
	DefaultRefreshInterval = 6 * time.Hour
	fetchTimeout           = 8 * time.Second
)

// Config moet **comparable** blijven (alle velden value-types). De
// Watch/Refresh-logica gebruikt `cfg != active` om te detecteren of een
// nieuwe fetch een effective-change is. Voeg geen slices/maps toe zonder
// die check (DeepEqual) te updaten.
type Config struct {
	SchemaVersion          int       `json:"schema_version"`
	SupabaseBaseURL        string    `json:"supabase_base_url"`
	SupabasePublishableKey string    `json:"supabase_publishable_key"`
	SupabaseRealtimeURL    string    `json:"supabase_realtime_url,omitempty"`
	MinAgentVersion        string    `json:"min_agent_version,omitempty"`
	IssuedAt               time.Time `json:"issued_at"`
}

// BakedInDefault is the last-resort config when neither the endpoint nor
// a cache file is available. Update ONLY when shipping a new agent
// release where the Supabase URL+key have not changed in production.
var BakedInDefault = Config{
	SchemaVersion:          1,
	SupabaseBaseURL:        "https://fjesjyoxpkalaudfyebx.supabase.co",
	SupabasePublishableKey: "sb_publishable_zRZhor-u2pIdiSXAGDLlMA_Dl8tbDS5",
	IssuedAt:               time.Time{},
}

type Fetcher struct {
	Endpoint   string
	CachePath  string // bv. /var/lib/proxmoxvue-agent/remote-config.json
	HTTPClient *http.Client
}

func NewFetcher(cachePath string) *Fetcher {
	return &Fetcher{
		Endpoint:   DefaultEndpoint,
		CachePath:  cachePath,
		HTTPClient: &http.Client{Timeout: fetchTimeout},
	}
}

// Source identifies which fallback layer produced the returned Config.
type Source int

const (
	SourceLive Source = iota
	SourceCache
	SourceBakedIn
)

func (s Source) String() string {
	switch s {
	case SourceLive:
		return "live"
	case SourceCache:
		return "cache"
	default:
		return "baked-in"
	}
}

// Load returns the best available config: live > cache > baked-in.
// Live failure is logged by caller; never an error from Load itself.
func (f *Fetcher) Load(ctx context.Context) (Config, Source) {
	if cfg, err := f.fetchLive(ctx); err == nil {
		_ = f.writeCache(cfg)
		return cfg, SourceLive
	}
	if cfg, err := f.readCache(); err == nil {
		return cfg, SourceCache
	}
	return BakedInDefault, SourceBakedIn
}

func (f *Fetcher) fetchLive(ctx context.Context) (Config, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.Endpoint, nil)
	if err != nil {
		return Config{}, err
	}
	resp, err := f.HTTPClient.Do(req)
	if err != nil {
		return Config{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Config{}, fmt.Errorf("status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return Config{}, err
	}
	var cfg Config
	if err := json.Unmarshal(body, &cfg); err != nil {
		return Config{}, err
	}
	if cfg.SchemaVersion != 1 {
		return Config{}, fmt.Errorf("unsupported schema_version %d", cfg.SchemaVersion)
	}
	if cfg.SupabaseBaseURL == "" || cfg.SupabasePublishableKey == "" {
		return Config{}, errors.New("required fields missing")
	}
	return cfg, nil
}

func (f *Fetcher) readCache() (Config, error) {
	data, err := os.ReadFile(f.CachePath)
	if err != nil {
		return Config{}, err
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return Config{}, err
	}
	if cfg.SchemaVersion != 1 {
		return Config{}, errors.New("cached schema_version mismatch")
	}
	return cfg, nil
}

func (f *Fetcher) writeCache(cfg Config) error {
	if err := os.MkdirAll(filepath.Dir(f.CachePath), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	tmp := f.CachePath + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, f.CachePath)
}

// RefreshLoop runs in the background and periodically writes the cache
// file from the live endpoint. Callers DO NOT hot-swap mid-flight — a
// config change becomes effective on the next restart. The loop's only
// job is keeping the cache file warm so a restart picks up the latest.
// Returns when ctx is done.
func (f *Fetcher) RefreshLoop(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if cfg, err := f.fetchLive(ctx); err == nil {
				_ = f.writeCache(cfg)
			}
		}
	}
}
