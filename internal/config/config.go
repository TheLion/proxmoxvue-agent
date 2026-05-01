// Package config defines the on-disk configuration format for the agent.
//
// The String() methods on credential-bearing structs are deliberate: they
// ensure that fmt.Printf("%+v", cfg) and similar wrapper-style logging
// never leak secrets. See config_test.go for the guard.
package config

import (
	"fmt"
	"log/slog"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

type File struct {
	Supabase SupabaseConfig `yaml:"supabase"`
	Proxmox  ProxmoxConfig  `yaml:"proxmox,omitempty"`
	Agent    AgentConfig    `yaml:"agent,omitempty"`
}

type SupabaseConfig struct {
	ProjectRef   string `yaml:"project_ref"`
	ClusterID    string `yaml:"cluster_id"`
	RefreshToken string `yaml:"refresh_token"`
}

func (c SupabaseConfig) String() string {
	masked := "<unset>"
	if c.RefreshToken != "" {
		masked = "[REDACTED]"
	}
	return fmt.Sprintf("{ProjectRef:%s ClusterID:%s RefreshToken:%s}", c.ProjectRef, c.ClusterID, masked)
}

type ProxmoxConfig struct {
	APIURL         string `yaml:"api_url,omitempty"`
	APITokenID     string `yaml:"api_token_id,omitempty"`
	APITokenSecret string `yaml:"api_token_secret,omitempty"`
	VerifyTLS      bool   `yaml:"verify_tls"`
}

func (c ProxmoxConfig) String() string {
	masked := "<unset>"
	if c.APITokenSecret != "" {
		masked = "[REDACTED]"
	}
	return fmt.Sprintf("{APIURL:%s APITokenID:%s APITokenSecret:%s VerifyTLS:%t}",
		c.APIURL, c.APITokenID, masked, c.VerifyTLS)
}

type AgentConfig struct {
	PollIntervalSeconds int    `yaml:"poll_interval_seconds,omitempty"`
	LogLevel            string `yaml:"log_level,omitempty"`
}

// ParseLogLevel mapt config-strings (case-insensitive) naar slog.Level.
// Lege string defaultt naar info. Onbekende waarde levert een error op
// zodat de caller fail-fast kan exit'en.
func ParseLogLevel(s string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "info":
		return slog.LevelInfo, nil
	case "debug":
		return slog.LevelDebug, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return slog.LevelInfo, fmt.Errorf("invalid log_level %q (valid: debug, info, warn, error)", s)
	}
}

func Save(path string, f File) error {
	data, err := yaml.Marshal(f)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

func Load(path string) (File, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return File{}, fmt.Errorf("read %s: %w", path, err)
	}
	var f File
	if err := yaml.Unmarshal(data, &f); err != nil {
		return File{}, fmt.Errorf("unmarshal: %w", err)
	}
	return f, nil
}
