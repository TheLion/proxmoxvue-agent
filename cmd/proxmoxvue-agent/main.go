package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/TheLion/proxmoxvue-agent/internal/config"
	"github.com/TheLion/proxmoxvue-agent/internal/enroll"
	"github.com/TheLion/proxmoxvue-agent/internal/keysync"
	"github.com/TheLion/proxmoxvue-agent/internal/remoteconfig"
	"github.com/TheLion/proxmoxvue-agent/internal/runtime"
	"github.com/TheLion/proxmoxvue-agent/internal/supabase"
	"golang.org/x/mod/semver"
	"gopkg.in/natefinch/lumberjack.v2"
)

const (
	defaultProjectRef      = "fjesjyoxpkalaudfyebx"
	defaultConfigPath      = "/etc/proxmoxvue-agent/config.yml"
	defaultRemoteCachePath = "/var/lib/proxmoxvue-agent/remote-config.json"
)

// version is injected via ldflags in release builds:
//
//	go build -ldflags="-X main.version=$(git describe --tags --always --dirty)" ...
//
// Defaults to "dev" for `go run` and non-build-script builds.
var version = "dev"

// exitRevoked signals systemd that this failure is permanent for the
// current session — re-enrollment is required. systemd's Restart=
// configuration can exclude this code so we don't loop.
const exitRevoked = 78

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(2)
	}

	switch os.Args[1] {
	case "--register", "register":
		runRegister(os.Args[2:])
	case "--rotate-key", "rotate-key":
		runRotateKey(os.Args[2:])
	case "--run", "run":
		runAgent(os.Args[2:])
	case "--version", "version":
		fmt.Println("proxmoxvue-agent", version)
	case "--help", "-h", "help":
		printUsage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n\n", os.Args[1])
		printUsage()
		os.Exit(2)
	}
}

func printUsage() {
	fmt.Fprintln(os.Stderr, "usage: proxmoxvue-agent <command>")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "commands:")
	fmt.Fprintln(os.Stderr, "  --register CODE   enroll this host against the ProxmoxVue backend")
	fmt.Fprintln(os.Stderr, "  --rotate-key      generate a fresh HPKE keypair and upload the public key")
	fmt.Fprintln(os.Stderr, "  --run             run the long-lived agent loop (used by systemd)")
	fmt.Fprintln(os.Stderr, "  --version         print version")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "flags for --register, --rotate-key, and --run:")
	fmt.Fprintln(os.Stderr, "  --config PATH          config path (default /etc/proxmoxvue-agent/config.yml)")
	fmt.Fprintln(os.Stderr, "  --project-ref REF      Supabase project ref (register only)")
}

func runAgent(args []string) {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	configPath := fs.String("config", defaultConfigPath, "path to config file")
	remoteCachePath := fs.String("remote-config-cache", defaultRemoteCachePath, "path to remote-config cache file")
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}

	// Init the slog handler based on agent.log_level + log_file_path.
	// The config may be missing or corrupt — that error surfaces from
	// runtime.Start; for logging we fall back here to INFO + default
	// file path. A present-but-invalid log_level / negative logging
	// value is fail-fast (otherwise we'd hide user errors).
	level := slog.LevelInfo
	rotation := config.AgentConfig{}.EffectiveLogRotation()
	if cfg, err := config.Load(*configPath); err == nil {
		l, err := config.ParseLogLevel(cfg.Agent.LogLevel)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		if err := cfg.Agent.ValidateLogging(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		level = l
		// Fill missing defaults and rewrite config.yml on every start so
		// all keys + inline comments stay visible, even after an
		// upgrade that introduces new fields/comments. Idempotent when
		// nothing changes (same bytes, only mtime). On write failure
		// (e.g. read-only fs): warn but continue — the agent keeps
		// running with in-memory defaults.
		config.EnsureDefaults(&cfg)
		if saveErr := config.Save(*configPath, cfg); saveErr != nil {
			fmt.Fprintf(os.Stderr, "warn: failed to rewrite config: %v\n", saveErr)
		}
		rotation = cfg.Agent.EffectiveLogRotation()
	}
	logSink := newLogSink(rotation)
	slog.SetDefault(slog.New(slog.NewTextHandler(logSink, &slog.HandlerOptions{Level: level})))

	// Server-driven config: pak Supabase URL/key uit /config/v1.json met
	// fallback naar cache → baked-in. Een wijziging op het endpoint wordt
	// pas op de volgende restart effectief — we hot-swappen niet.
	bootCtx, cancelBoot := context.WithTimeout(context.Background(), 10*time.Second)
	fetcher := remoteconfig.NewFetcher(*remoteCachePath)
	rc, source := fetcher.Load(bootCtx)
	cancelBoot()
	slog.Info("remote-config loaded",
		"source", source.String(),
		"base_url", rc.SupabaseBaseURL,
		"issued_at", rc.IssuedAt)

	if rc.MinAgentVersion != "" {
		if !semver.IsValid(version) || !semver.IsValid(rc.MinAgentVersion) {
			slog.Warn("min_agent_version check skipped — non-semver value(s)",
				"running", version, "required", rc.MinAgentVersion)
		} else if semver.Compare(version, rc.MinAgentVersion) < 0 {
			slog.Error("agent version below min_agent_version, refusing to start",
				"running", version, "required", rc.MinAgentVersion)
			os.Exit(1)
		}
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	go fetcher.RefreshLoop(ctx, remoteconfig.DefaultRefreshInterval)

	err := runtime.Start(ctx, *configPath, version, rc)
	if errors.Is(err, supabase.ErrRefreshRevoked) {
		fmt.Fprintln(os.Stderr, "supabase session revoked — re-enroll with --register")
		os.Exit(exitRevoked)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "agent exited: %v\n", err)
		os.Exit(1)
	}
}

// newLogSink builds a writer that fans every log line out to both
// stderr (so `docker logs` / `journalctl -u proxmoxvue-agent` see
// everything live) and a lumberjack-rotated file at LogFilePath.
// If LogFilePath is not writable (typical during local `go run`
// without /var/log access, or in containers that don't mount a log
// volume) it falls back to stderr-only — that way the agent doesn't
// crash on a logging path; runtime.Start can still report its real
// errors.
func newLogSink(r config.LogRotation) io.Writer {
	probe, err := os.OpenFile(r.FilePath, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		fmt.Fprintf(os.Stderr, "log file %s not writable (%v) — falling back to stderr-only\n", r.FilePath, err)
		return os.Stderr
	}
	_ = probe.Close()
	return io.MultiWriter(os.Stderr, &lumberjack.Logger{
		Filename:   r.FilePath,
		MaxSize:    r.MaxSizeMB,
		MaxBackups: r.MaxBackups,
		MaxAge:     r.MaxAgeDays,
	})
}

func runRegister(args []string) {
	fs := flag.NewFlagSet("register", flag.ExitOnError)
	configPath := fs.String("config", "/etc/proxmoxvue-agent/config.yml", "path to write the config file")
	projectRef := fs.String("project-ref", defaultProjectRef, "Supabase project ref")
	remoteCachePath := fs.String("remote-config-cache", defaultRemoteCachePath, "path to remote-config cache file")
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}

	if fs.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "missing CODE argument")
		os.Exit(2)
	}
	code := fs.Arg(0)

	result, err := enroll.Run(enroll.Options{
		Code:       code,
		ProjectRef: *projectRef,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "enrollment failed: %v\n", err)
		os.Exit(1)
	}

	// Non-destructive merge: load the existing config (proxmox + agent
	// stay as they were), validate the current log_level, replace only
	// the Supabase block. On parse error against an existing config:
	// fail-fast — otherwise we'd silently overwrite user data on a typo.
	var cfg config.File
	if existing, loadErr := config.Load(*configPath); loadErr == nil {
		cfg = existing
		if cfg.Agent.LogLevel != "" {
			if _, vErr := config.ParseLogLevel(cfg.Agent.LogLevel); vErr != nil {
				fmt.Fprintf(os.Stderr, "config has invalid %v\n", vErr)
				os.Exit(1)
			}
		}
	} else if !errors.Is(loadErr, os.ErrNotExist) {
		fmt.Fprintf(os.Stderr, "failed to read existing config: %v\n", loadErr)
		os.Exit(1)
	}

	// Replace the Supabase block — keep proxmox + agent settings as they
	// were. Existing private_key (if any) is preserved by EnsurePrivateKey
	// below; re-register must not rotate the keypair, otherwise already-
	// encrypted payloads can no longer be decrypted.
	cfg.Supabase = config.SupabaseConfig{
		ProjectRef:   result.ProjectRef,
		ClusterID:    result.ClusterID,
		RefreshToken: result.RefreshToken,
		PrivateKey:   cfg.Supabase.PrivateKey, // preserved across re-register
	}
	config.EnsureDefaults(&cfg)

	if err := config.Save(*configPath, cfg); err != nil {
		fmt.Fprintf(os.Stderr, "failed to write config: %v\n", err)
		os.Exit(1)
	}

	// Generate keypair if missing (first-time enrollment) and persist it.
	privateKeyB64, _, err := keysync.EnsurePrivateKey(&cfg, *configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to ensure keypair: %v\n", err)
		os.Exit(1)
	}

	// Upload the public key to clusters.public_key so iOS can send
	// LXC passwords E2E-encrypted (#1476). Failure here is not fatal —
	// the iOS app then shows "agent update needed" on LXC create and
	// the user can manually re-run --register or --rotate-key.
	config.EnsureSupabaseDefaults(&cfg)
	rc, _ := remoteconfig.NewFetcher(*remoteCachePath).Load(context.Background())
	sb, err := supabase.New(rc.SupabaseBaseURL, rc.SupabasePublishableKey, rc.SupabaseRealtimeURL, cfg.Supabase.RefreshToken, nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "build supabase client: %v\n", err)
		os.Exit(1)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := keysync.UploadPublicKey(ctx, sb, cfg.Supabase.ClusterID, privateKeyB64); err != nil {
		fmt.Fprintf(os.Stderr, "warn: failed to upload public key: %v\n", err)
		fmt.Fprintln(os.Stderr, "      cloud-path LXC creates will fail until this succeeds;")
		fmt.Fprintln(os.Stderr, "      run --register or --rotate-key once Supabase is reachable.")
	}

	fmt.Printf("registered cluster %s (host %s), config written to %s\n", result.ClusterID, result.HostID, *configPath)
	fmt.Println()
	proxmoxIncomplete := cfg.Proxmox.APIURL == "" || cfg.Proxmox.APITokenSecret == ""
	if proxmoxIncomplete {
		// Interactive prompt only on a TTY — scripted/automation
		// installs (CI, ansible stdin redirected) keep working with
		// the printed manual-edit instructions instead.
		if isStdinTTY() {
			if changed := promptProxmoxConfig(&cfg); changed {
				if err := config.Save(*configPath, cfg); err != nil {
					fmt.Fprintf(os.Stderr, "failed to write Proxmox credentials to config: %v\n", err)
					os.Exit(1)
				}
				fmt.Printf("\nProxmox credentials written to %s\n", *configPath)
				fmt.Println("restart the agent to start dispatching commands:")
				fmt.Println("  systemctl restart proxmoxvue-agent  (systemd)")
				fmt.Println("  or: proxmoxvue-agent --run  (foreground)")
				return
			}
		}
		fmt.Println("next: add your Proxmox API token to the config file:")
		fmt.Println("  proxmox:")
		fmt.Println("    api_url: https://<host>:8006")
		fmt.Println("    api_token_id: user@realm!tokenid")
		fmt.Println("    api_token_secret: <uuid>")
		fmt.Println("    verify_tls: false")
		fmt.Println("then: systemctl restart proxmoxvue-agent")
		return
	}
	fmt.Println("Proxmox config is ready — restart the agent:")
	fmt.Println("  systemctl restart proxmoxvue-agent  (systemd)")
	fmt.Println("  or: proxmoxvue-agent --run  (foreground)")
}

// isStdinTTY reports whether stdin is connected to a terminal. Used to
// gate interactive prompts: pipes, redirected stdin, and headless CI
// runs all return false so we never block on a read that never gets
// input.
func isStdinTTY() bool {
	stat, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return (stat.Mode() & os.ModeCharDevice) != 0
}

// promptProxmoxConfig fills empty Proxmox fields in cfg from interactive
// stdin input. Already-populated fields are left untouched
// (non-destructive merge — re-running --register against a partially
// configured agent must not wipe an api_token_secret). Returns true
// when at least one field was filled in, signalling the caller to
// persist the config.
//
// VerifyTLS is only prompted on a fresh install (all three string
// fields empty); on partial configs the existing bool is preserved
// because we cannot tell explicit-false from the zero value.
func promptProxmoxConfig(cfg *config.File) bool {
	reader := bufio.NewReader(os.Stdin)
	freshInstall := cfg.Proxmox.APIURL == "" && cfg.Proxmox.APITokenID == "" && cfg.Proxmox.APITokenSecret == ""
	changed := false

	fmt.Println()
	fmt.Println("set up Proxmox API credentials (press Enter to skip a field):")

	if cfg.Proxmox.APIURL == "" {
		if v := readLine(reader, "  api_url (e.g. https://pve.example.com:8006): "); v != "" {
			cfg.Proxmox.APIURL = v
			changed = true
		}
	}
	if cfg.Proxmox.APITokenID == "" {
		if v := readLine(reader, "  api_token_id (e.g. root@pam!claude): "); v != "" {
			cfg.Proxmox.APITokenID = v
			changed = true
		}
	}
	if cfg.Proxmox.APITokenSecret == "" {
		if v := readLine(reader, "  api_token_secret (uuid): "); v != "" {
			cfg.Proxmox.APITokenSecret = v
			changed = true
		}
	}
	if freshInstall && changed {
		v := readLine(reader, "  verify_tls (y/N) [N for self-signed homelab certs]: ")
		v = strings.ToLower(v)
		cfg.Proxmox.VerifyTLS = v == "y" || v == "yes"
	}
	return changed
}

// readLine prints the prompt and returns the trimmed line read from
// the reader. EOF and read errors collapse to an empty string so the
// caller can treat "no input" identically to "user pressed Enter".
func readLine(r *bufio.Reader, prompt string) string {
	fmt.Print(prompt)
	line, err := r.ReadString('\n')
	if err != nil && line == "" {
		return ""
	}
	return strings.TrimSpace(line)
}

// runRotateKey generates a fresh HPKE keypair, persists the private key
// to config.yml, and uploads the matching public key to
// clusters.public_key. Existing private key is overwritten — only useful
// when the operator suspects key compromise or when migrating from a
// pre-#1476 install where no keypair was ever generated.
//
// Note: rotating the key invalidates all previously-issued ciphertexts.
// LXC create-passwords are always freshly encrypted by the iOS app, so
// this has no effect on already-completed operations.
func runRotateKey(args []string) {
	fs := flag.NewFlagSet("rotate-key", flag.ExitOnError)
	configPath := fs.String("config", defaultConfigPath, "path to config file")
	remoteCachePath := fs.String("remote-config-cache", defaultRemoteCachePath, "path to remote-config cache file")
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to load config: %v\n", err)
		os.Exit(1)
	}
	config.EnsureSupabaseDefaults(&cfg)
	if cfg.Supabase.BaseURL == "" || cfg.Supabase.ClusterID == "" || cfg.Supabase.RefreshToken == "" {
		fmt.Fprintln(os.Stderr, "config is missing Supabase enrollment fields — run --register first")
		os.Exit(1)
	}

	rc, _ := remoteconfig.NewFetcher(*remoteCachePath).Load(context.Background())
	sb, err := supabase.New(rc.SupabaseBaseURL, rc.SupabasePublishableKey, rc.SupabaseRealtimeURL, cfg.Supabase.RefreshToken, nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "build supabase client: %v\n", err)
		os.Exit(1)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if _, err := keysync.RotateKey(ctx, &cfg, *configPath, sb); err != nil {
		fmt.Fprintf(os.Stderr, "rotate-key failed: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("rotated HPKE keypair, public key uploaded to cluster")
	fmt.Println("restart the agent to pick up the new private key:")
	fmt.Println("  systemctl restart proxmoxvue-agent")
}
