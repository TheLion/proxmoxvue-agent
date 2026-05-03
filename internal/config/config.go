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
	Proxmox  ProxmoxConfig  `yaml:"proxmox"`
	Agent    AgentConfig    `yaml:"agent"`
}

type SupabaseConfig struct {
	ProjectRef   string `yaml:"project_ref"`
	ClusterID    string `yaml:"cluster_id"`
	RefreshToken string `yaml:"refresh_token"`
	// PrivateKey is een base64-encoded raw X25519 private key (32 bytes)
	// voor HPKE-decryptie van LXC create-passwords (#1476). Wordt
	// gegenereerd bij --register als hij ontbreekt; de bijbehorende
	// public key gaat naar clusters.public_key in Supabase. Lekkage
	// van deze key compromitteert lopend-versleutelde passwords; mode
	// 0600 op config.yml beschermt 'm. Re-keying = nieuwe --register.
	PrivateKey string `yaml:"private_key,omitempty"`
}

func (c SupabaseConfig) String() string {
	maskedToken := "<unset>"
	if c.RefreshToken != "" {
		maskedToken = "[REDACTED]"
	}
	maskedKey := "<unset>"
	if c.PrivateKey != "" {
		maskedKey = "[REDACTED]"
	}
	return fmt.Sprintf("{ProjectRef:%s ClusterID:%s RefreshToken:%s PrivateKey:%s}",
		c.ProjectRef, c.ClusterID, maskedToken, maskedKey)
}

type ProxmoxConfig struct {
	APIURL         string `yaml:"api_url"`
	APITokenID     string `yaml:"api_token_id"`
	APITokenSecret string `yaml:"api_token_secret"`
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
	PollIntervalSeconds int    `yaml:"poll_interval_seconds"`
	LogLevel            string `yaml:"log_level"`

	// File-logging via lumberjack. Lege LogFilePath valt terug op de default
	// `/var/log/proxmoxvue-agent.log`; de andere lege/0-waarden vallen terug
	// op de defaults uit DefaultLogRotation. Gebruiker kan elk veld los
	// overrullen via config.yml. EnsureDefaults persisteert deze waarden bij
	// elke agent-start zodat de keys altijd zichtbaar in config.yml staan.
	LogFilePath   string `yaml:"log_file_path"`
	LogMaxSizeMB  int    `yaml:"log_max_size_mb"`
	LogMaxBackups int    `yaml:"log_max_backups"`
	LogMaxAgeDays int    `yaml:"log_max_age_days"`
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

// Defaults voor de overige agent-velden, zo gecentraliseerd dat
// EnsureDefaults en EffectiveLogRotation hetzelfde getal pakken.
const (
	DefaultPollIntervalSeconds = 30
	DefaultLogLevel            = "info"
	DefaultLogMaxSizeMB        = 10
	DefaultLogMaxBackups       = 5
	DefaultLogMaxAgeDays       = 30
)

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
		r.MaxSizeMB = DefaultLogMaxSizeMB
	}
	if r.MaxBackups == 0 {
		r.MaxBackups = DefaultLogMaxBackups
	}
	if r.MaxAgeDays == 0 {
		r.MaxAgeDays = DefaultLogMaxAgeDays
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

// EnsureDefaults vult ontbrekende agent-velden in de config met hun
// defaults. Returnt true als er iets is gewijzigd zodat de caller weet
// of een Save nodig is. Het idee: na de eerste agent-start staan álle
// keys expliciet in config.yml, zodat een gebruiker ze kan vinden +
// aanpassen zonder docs te lezen.
//
// Note: 0 / "" worden hier als 'unset' behandeld, gelijk aan
// EffectiveLogRotation. Een gebruiker die expliciet 0 wil persisten kan dat
// niet — dat is geen zinvolle waarde voor deze velden (size 0 = onmiddellijk
// roteren, age 0 = nooit verwijderen op leeftijd, poll 0 = drukke loop;
// conflicteert met defaults).
func EnsureDefaults(cfg *File) bool {
	changed := false
	if cfg.Agent.PollIntervalSeconds == 0 {
		cfg.Agent.PollIntervalSeconds = DefaultPollIntervalSeconds
		changed = true
	}
	if cfg.Agent.LogLevel == "" {
		cfg.Agent.LogLevel = DefaultLogLevel
		changed = true
	}
	if cfg.Agent.LogFilePath == "" {
		cfg.Agent.LogFilePath = DefaultLogFilePath
		changed = true
	}
	if cfg.Agent.LogMaxSizeMB == 0 {
		cfg.Agent.LogMaxSizeMB = DefaultLogMaxSizeMB
		changed = true
	}
	if cfg.Agent.LogMaxBackups == 0 {
		cfg.Agent.LogMaxBackups = DefaultLogMaxBackups
		changed = true
	}
	if cfg.Agent.LogMaxAgeDays == 0 {
		cfg.Agent.LogMaxAgeDays = DefaultLogMaxAgeDays
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

// configHeader is prepended to every config.yml written by Save(). Detail
// per key is rendered inline above each key via fieldHeadComments — the
// header itself is short on purpose. yaml-parsers ignore `#` lines, so the
// header has no effect on Load().
const configHeader = `# proxmoxvue-agent config (generated by --register / --run)
# Per-key documentation is rendered inline below.

`

// fieldHeadComments maps dotted yaml paths to a single-line description that
// is rendered as `# comment` directly above the key. Only keys that actually
// appear in the marshaled output get a comment — `omitempty` fields without
// a value won't show, and that's fine.
var fieldHeadComments = map[string]string{
	"supabase":                    "Cluster identity issued at registration. Don't edit manually.",
	"supabase.project_ref":        "Supabase project ref. Set by --register.",
	"supabase.cluster_id":         "Cluster UUID issued by the backend during enrollment.",
	"supabase.refresh_token":      "Long-lived refresh token. Re-run --register if revoked.",
	"supabase.private_key":        "X25519 private key (base64) for HPKE decrypt of LXC passwords. Generated at --register; treat as a secret.",
	"proxmox":                     "Direct connection to the local Proxmox VE API.",
	"proxmox.api_url":             "Proxmox API endpoint, e.g. https://<host>:8006.",
	"proxmox.api_token_id":        "Token ID in the form user@realm!tokenid.",
	"proxmox.api_token_secret":    "Token secret (UUID). Treat like a password.",
	"proxmox.verify_tls":          "Verify TLS cert; false for self-signed PVE certs.",
	"agent":                       "Agent loop + logging settings.",
	"agent.poll_interval_seconds": "Tick frequency for the snapshot push loop (default 30).",
	"agent.log_level":             "debug | info | warn | error (default info).",
	"agent.log_file_path":         "Plain-text log file (default /var/log/proxmoxvue-agent.log).",
	"agent.log_max_size_mb":       "Rotate the active file once it reaches this many MB (default 10).",
	"agent.log_max_backups":       "Keep this many rotated files; FIFO, oldest deleted first (default 5).",
	"agent.log_max_age_days":      "Delete rotated files older than this many days (default 30).",
}

// annotateComments walks a yaml mapping tree and attaches HeadComments from
// fieldHeadComments based on the dotted key path. Document and non-mapping
// nodes are passed through. Called recursively for nested mappings.
func annotateComments(n *yaml.Node, prefix string) {
	if n == nil {
		return
	}
	if n.Kind == yaml.DocumentNode {
		for _, c := range n.Content {
			annotateComments(c, prefix)
		}
		return
	}
	if n.Kind != yaml.MappingNode {
		return
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		keyNode := n.Content[i]
		valNode := n.Content[i+1]
		path := keyNode.Value
		if prefix != "" {
			path = prefix + "." + keyNode.Value
		}
		if c, ok := fieldHeadComments[path]; ok {
			keyNode.HeadComment = c
		}
		annotateComments(valNode, path)
	}
}

func Save(path string, f File) error {
	var root yaml.Node
	if err := root.Encode(f); err != nil {
		return fmt.Errorf("encode: %w", err)
	}
	annotateComments(&root, "")
	data, err := yaml.Marshal(&root)
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
