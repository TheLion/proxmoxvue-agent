package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func readFile(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// TestSupabaseConfigStringRedactsToken guards against accidentally reintroducing
// plain-token rendering in logs via %+v formatting.
func TestSupabaseConfigStringRedactsToken(t *testing.T) {
	cfg := SupabaseConfig{
		ProjectRef:   "abc",
		ClusterID:    "cluster-uuid",
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

func TestEffectiveLogRotation_DefaultsApplied(t *testing.T) {
	r := AgentConfig{}.EffectiveLogRotation()
	if r.FilePath != DefaultLogFilePath {
		t.Errorf("FilePath=%q want %q", r.FilePath, DefaultLogFilePath)
	}
	if r.MaxSizeMB != 10 || r.MaxBackups != 5 || r.MaxAgeDays != 30 {
		t.Errorf("defaults wrong: %+v", r)
	}
}

func TestEffectiveLogRotation_UserValuesPreserved(t *testing.T) {
	r := AgentConfig{
		LogFilePath:   "/tmp/custom.log",
		LogMaxSizeMB:  25,
		LogMaxBackups: 2,
		LogMaxAgeDays: 7,
	}.EffectiveLogRotation()
	if r.FilePath != "/tmp/custom.log" || r.MaxSizeMB != 25 || r.MaxBackups != 2 || r.MaxAgeDays != 7 {
		t.Errorf("user values not preserved: %+v", r)
	}
}

func TestValidateLogging_NegativesRejected(t *testing.T) {
	cases := []AgentConfig{
		{LogMaxSizeMB: -1},
		{LogMaxBackups: -1},
		{LogMaxAgeDays: -1},
	}
	for _, c := range cases {
		if err := c.ValidateLogging(); err == nil {
			t.Errorf("expected error for %+v", c)
		}
	}
}

func TestValidateLogging_ZeroIsValid(t *testing.T) {
	if err := (AgentConfig{}).ValidateLogging(); err != nil {
		t.Errorf("zero values should be valid (fall back to defaults), got %v", err)
	}
}

func TestEnsureDefaults(t *testing.T) {
	cfg := File{}
	changed := EnsureDefaults(&cfg)
	if !changed {
		t.Fatal("expected changed=true on empty config")
	}
	if cfg.Agent.PollIntervalSeconds != DefaultPollIntervalSeconds {
		t.Errorf("PollIntervalSeconds=%d", cfg.Agent.PollIntervalSeconds)
	}
	if cfg.Agent.LogLevel != DefaultLogLevel {
		t.Errorf("LogLevel=%q", cfg.Agent.LogLevel)
	}
	if cfg.Agent.LogFilePath != DefaultLogFilePath {
		t.Errorf("LogFilePath=%q", cfg.Agent.LogFilePath)
	}
	if cfg.Agent.LogMaxSizeMB != DefaultLogMaxSizeMB ||
		cfg.Agent.LogMaxBackups != DefaultLogMaxBackups ||
		cfg.Agent.LogMaxAgeDays != DefaultLogMaxAgeDays {
		t.Errorf("defaults not applied: %+v", cfg.Agent)
	}

	// Tweede call op already-filled config moet idempotent zijn.
	if EnsureDefaults(&cfg) {
		t.Error("expected changed=false on fully populated config")
	}
}

func TestSaveLoadRoundtrip_PreservesLoggingFields(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yml")

	cfg := File{
		Supabase: SupabaseConfig{ProjectRef: "ref", ClusterID: "uuid", RefreshToken: "x"},
		Agent: AgentConfig{
			LogLevel:      "debug",
			LogFilePath:   "/tmp/x.log",
			LogMaxSizeMB:  20,
			LogMaxBackups: 3,
			LogMaxAgeDays: 14,
		},
	}
	if err := Save(path, cfg); err != nil {
		t.Fatal(err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Agent != cfg.Agent {
		t.Errorf("roundtrip mismatch: got %+v, want %+v", got.Agent, cfg.Agent)
	}
}

func TestSave_WritesInlineComments(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yml")
	cfg := File{
		Supabase: SupabaseConfig{ProjectRef: "ref", ClusterID: "uuid", RefreshToken: "x"},
	}
	EnsureDefaults(&cfg)
	if err := Save(path, cfg); err != nil {
		t.Fatal(err)
	}
	data, err := readFile(path)
	if err != nil {
		t.Fatal(err)
	}

	// Top-of-file header.
	if !strings.Contains(data, "proxmoxvue-agent config") {
		t.Errorf("expected file header, got:\n%s", data)
	}

	// Each documented key should have its inline comment immediately above
	// the key itself (yaml.v3 puts HeadComments on the line above).
	checks := []struct {
		commentFragment string
		key             string
	}{
		{"Set by --register", "project_ref:"},
		{"Long-lived refresh token", "refresh_token:"},
		{"Tick frequency for the snapshot push loop", "poll_interval_seconds:"},
		{"debug | info | warn | error", "log_level:"},
		{"Plain-text log file", "log_file_path:"},
		{"Rotate the active file", "log_max_size_mb:"},
		{"FIFO, oldest deleted first", "log_max_backups:"},
		{"Delete rotated files older", "log_max_age_days:"},
	}
	for _, c := range checks {
		idx := strings.Index(data, c.commentFragment)
		if idx == -1 {
			t.Errorf("missing comment fragment %q in:\n%s", c.commentFragment, data)
			continue
		}
		rest := data[idx:]
		keyIdx := strings.Index(rest, c.key)
		if keyIdx == -1 {
			t.Errorf("key %q not found after comment %q", c.key, c.commentFragment)
			continue
		}
		// Between comment and key, no other yaml key should appear.
		between := rest[:keyIdx]
		if strings.Count(between, "\n") > 2 {
			t.Errorf("comment %q is not directly above key %q (gap=%q)",
				c.commentFragment, c.key, between)
		}
	}
}
