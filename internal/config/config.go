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

	// File-logging via lumberjack. Lege LogFilePath valt terug op de default
	// `/var/log/proxmoxvue-agent.log`; de andere lege/0-waarden vallen terug
	// op de defaults uit DefaultLogRotation. Gebruiker kan elk veld los
	// overrullen via config.yml.
	LogFilePath   string `yaml:"log_file_path,omitempty"`
	LogMaxSizeMB  int    `yaml:"log_max_size_mb,omitempty"`
	LogMaxBackups int    `yaml:"log_max_backups,omitempty"`
	LogMaxAgeDays int    `yaml:"log_max_age_days,omitempty"`
}

// LogRotation bundelt de effectieve logging-instellingen na default-fill,
// zodat caller-code ze als één geheel doorgeeft aan lumberjack.
type LogRotation struct {
	FilePath   string
	MaxSizeMB  int
	MaxBackups int
	MaxAgeDays int
}

// DefaultLogFilePath is de default log-locatie als config.yml geen pad geeft.
const DefaultLogFilePath = "/var/log/proxmoxvue-agent.log"

// EffectiveLogRotation returnt de log-instellingen na default-fill. Defaults:
// 10 MB per file, 5 backups bewaard, max 30 dagen oud. Voldoet voor een
// homelab-deploy met routine 30s-pushes (~50-100 KB/dag aan logs).
func (c AgentConfig) EffectiveLogRotation() LogRotation {
	r := LogRotation{
		FilePath:   c.LogFilePath,
		MaxSizeMB:  c.LogMaxSizeMB,
		MaxBackups: c.LogMaxBackups,
		MaxAgeDays: c.LogMaxAgeDays,
	}
	if r.FilePath == "" {
		r.FilePath = DefaultLogFilePath
	}
	if r.MaxSizeMB == 0 {
		r.MaxSizeMB = 10
	}
	if r.MaxBackups == 0 {
		r.MaxBackups = 5
	}
	if r.MaxAgeDays == 0 {
		r.MaxAgeDays = 30
	}
	return r
}

// ValidateLogging blokkeert ongeldige config-waarden bij start. Negatieve
// getallen zijn fout (lumberjack accepteert ze maar het is gegarandeerd
// niet wat de gebruiker bedoelde). Zero-values vallen terug op defaults
// via EffectiveLogRotation; geen error.
func (c AgentConfig) ValidateLogging() error {
	if c.LogMaxSizeMB < 0 {
		return fmt.Errorf("log_max_size_mb must be >= 0, got %d", c.LogMaxSizeMB)
	}
	if c.LogMaxBackups < 0 {
		return fmt.Errorf("log_max_backups must be >= 0, got %d", c.LogMaxBackups)
	}
	if c.LogMaxAgeDays < 0 {
		return fmt.Errorf("log_max_age_days must be >= 0, got %d", c.LogMaxAgeDays)
	}
	return nil
}

// EnsureLoggingDefaults vult ontbrekende logging-velden in de config met
// hun defaults. Returnt true als er iets is gewijzigd zodat de caller weet
// of een Save nodig is. Het idee: na de eerste agent-start staan de keys
// expliciet in config.yml, zodat een gebruiker ze kan vinden + aanpassen
// zonder docs te lezen.
//
// Note: 0 wordt hier als 'unset' behandeld, gelijk aan EffectiveLogRotation.
// Een gebruiker die expliciet 0 wil persisten kan dat niet — dat is geen
// zinvolle waarde voor deze velden (size 0 = onmiddellijk roteren, age 0 =
// nooit verwijderen op leeftijd; conflicteert met defaults).
func EnsureLoggingDefaults(cfg *File) bool {
	changed := false
	if cfg.Agent.LogFilePath == "" {
		cfg.Agent.LogFilePath = DefaultLogFilePath
		changed = true
	}
	if cfg.Agent.LogMaxSizeMB == 0 {
		cfg.Agent.LogMaxSizeMB = 10
		changed = true
	}
	if cfg.Agent.LogMaxBackups == 0 {
		cfg.Agent.LogMaxBackups = 5
		changed = true
	}
	if cfg.Agent.LogMaxAgeDays == 0 {
		cfg.Agent.LogMaxAgeDays = 30
		changed = true
	}
	return changed
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

// configHeader is prepended to every config.yml written by Save(). It serves
// as a quick reference so users can find the keys without reading docs.
// yaml-parsers ignore `#` lines, so it has no effect on Load().
const configHeader = `# proxmoxvue-agent config (generated by --register / --run)
#
# agent.poll_interval_seconds  Tick frequency for the snapshot push loop (default 30).
# agent.log_level              debug | info | warn | error (default info).
# agent.log_file_path          Plain-text log file (default /var/log/proxmoxvue-agent.log).
# agent.log_max_size_mb        Rotate the current file once it reaches this many MB (default 10).
# agent.log_max_backups        Keep this many rotated files; FIFO, oldest deleted first (default 5).
# agent.log_max_age_days       Delete rotated files older than this many days (default 30).
#
# Negative values for the log_max_* keys are an error and stop the agent at
# startup. 0 falls back to the default above.

`

func Save(path string, f File) error {
	data, err := yaml.Marshal(f)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	full := append([]byte(configHeader), data...)
	if err := os.WriteFile(path, full, 0o600); err != nil {
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
