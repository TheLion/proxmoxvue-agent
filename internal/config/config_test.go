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

func TestEnsureLoggingDefaults(t *testing.T) {
	cfg := File{}
	changed := EnsureLoggingDefaults(&cfg)
	if !changed {
		t.Fatal("expected changed=true on empty config")
	}
	if cfg.Agent.LogFilePath != DefaultLogFilePath {
		t.Errorf("LogFilePath=%q", cfg.Agent.LogFilePath)
	}
	if cfg.Agent.LogMaxSizeMB != 10 || cfg.Agent.LogMaxBackups != 5 || cfg.Agent.LogMaxAgeDays != 30 {
		t.Errorf("defaults not applied: %+v", cfg.Agent)
	}

	// Tweede call op already-filled config moet idempotent zijn.
	if EnsureLoggingDefaults(&cfg) {
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

func TestSave_WritesHeaderComments(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yml")
	if err := Save(path, File{}); err != nil {
		t.Fatal(err)
	}
	data, err := readFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"log_file_path", "log_max_size_mb", "log_max_backups", "log_max_age_days"} {
		if !strings.Contains(data, key) {
			t.Errorf("header missing key %q in:\n%s", key, data)
		}
	}
}
