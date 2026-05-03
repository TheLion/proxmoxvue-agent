package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"encoding/base64"

	"github.com/TheLion/proxmoxvue-agent/internal/config"
	agentcrypto "github.com/TheLion/proxmoxvue-agent/internal/crypto"
	"github.com/TheLion/proxmoxvue-agent/internal/enroll"
	"github.com/TheLion/proxmoxvue-agent/internal/runtime"
	"github.com/TheLion/proxmoxvue-agent/internal/supabase"
	"gopkg.in/natefinch/lumberjack.v2"
)

const (
	defaultProjectRef = "fjesjyoxpkalaudfyebx"
	defaultConfigPath = "/etc/proxmoxvue-agent/config.yml"
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
	fmt.Fprintln(os.Stderr, "  --run             run the long-lived agent loop (used by systemd)")
	fmt.Fprintln(os.Stderr, "  --version         print version")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "flags for --register and --run:")
	fmt.Fprintln(os.Stderr, "  --config PATH          config path (default /etc/proxmoxvue-agent/config.yml)")
	fmt.Fprintln(os.Stderr, "  --project-ref REF      Supabase project ref (register only)")
}

func runAgent(args []string) {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	configPath := fs.String("config", defaultConfigPath, "path to config file")
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

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	err := runtime.Start(ctx, *configPath, version)
	if errors.Is(err, supabase.ErrRefreshRevoked) {
		fmt.Fprintln(os.Stderr, "supabase session revoked — re-enroll with --register")
		os.Exit(exitRevoked)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "agent exited: %v\n", err)
		os.Exit(1)
	}
}

// newLogSink builds a lumberjack writer from the given rotation
// config. If LogFilePath is not writable (typical during local
// `go run` without /var/log access) it falls back to stderr — that
// way the agent doesn't crash on a logging path; runtime.Start can
// still report its real errors.
func newLogSink(r config.LogRotation) io.Writer {
	probe, err := os.OpenFile(r.FilePath, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		fmt.Fprintf(os.Stderr, "log file %s not writable (%v) — falling back to stderr\n", r.FilePath, err)
		return os.Stderr
	}
	_ = probe.Close()
	return &lumberjack.Logger{
		Filename:   r.FilePath,
		MaxSize:    r.MaxSizeMB,
		MaxBackups: r.MaxBackups,
		MaxAge:     r.MaxAgeDays,
	}
}

func runRegister(args []string) {
	fs := flag.NewFlagSet("register", flag.ExitOnError)
	configPath := fs.String("config", "/etc/proxmoxvue-agent/config.yml", "path to write the config file")
	projectRef := fs.String("project-ref", defaultProjectRef, "Supabase project ref")
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

	// Reuse the existing PrivateKey if present — re-register must not
	// rotate the keypair, otherwise already-encrypted payloads can no
	// longer be decrypted. Only generate when absent.
	privateKeyB64 := cfg.Supabase.PrivateKey
	if privateKeyB64 == "" {
		privBytes, _, err := agentcrypto.GenerateKeypair()
		if err != nil {
			fmt.Fprintf(os.Stderr, "failed to generate keypair: %v\n", err)
			os.Exit(1)
		}
		privateKeyB64 = base64.StdEncoding.EncodeToString(privBytes)
	}

	cfg.Supabase = config.SupabaseConfig{
		ProjectRef:   result.ProjectRef,
		ClusterID:    result.ClusterID,
		RefreshToken: result.RefreshToken,
		PrivateKey:   privateKeyB64,
	}
	config.EnsureDefaults(&cfg)

	if err := config.Save(*configPath, cfg); err != nil {
		fmt.Fprintf(os.Stderr, "failed to write config: %v\n", err)
		os.Exit(1)
	}

	// Upload the public key to clusters.public_key so iOS can send
	// LXC passwords E2E-encrypted (#1476). Failure here is not fatal —
	// the iOS app then shows "agent update needed" on LXC create and
	// the user can manually re-run --register.
	if err := uploadPublicKey(cfg, privateKeyB64); err != nil {
		fmt.Fprintf(os.Stderr, "warn: failed to upload public key: %v\n", err)
		fmt.Fprintln(os.Stderr, "      cloud-path LXC creates will fail until this succeeds;")
		fmt.Fprintln(os.Stderr, "      run --register again once Supabase is reachable.")
	}

	fmt.Printf("registered cluster %s (host %s), config written to %s\n", result.ClusterID, result.HostID, *configPath)
	fmt.Println()
	if cfg.Proxmox.APIURL == "" || cfg.Proxmox.APITokenSecret == "" {
		fmt.Println("next: add your Proxmox API token to the config file:")
		fmt.Println("  proxmox:")
		fmt.Println("    api_url: https://<host>:8006")
		fmt.Println("    api_token_id: user@realm!tokenid")
		fmt.Println("    api_token_secret: <uuid>")
		fmt.Println("    verify_tls: false")
		fmt.Println("then: systemctl restart proxmoxvue-agent")
	} else {
		fmt.Println("Proxmox-config staat al klaar — herstart de agent:")
		fmt.Println("  systemctl restart proxmoxvue-agent  (systemd)")
		fmt.Println("  or: proxmoxvue-agent --run  (foreground)")
	}
}

// uploadPublicKey derives the public key from the persisted private
// key and writes it to clusters.public_key. Idempotent — safe to
// re-run after a network failure.
func uploadPublicKey(cfg config.File, privateKeyB64 string) error {
	privBytes, err := base64.StdEncoding.DecodeString(privateKeyB64)
	if err != nil {
		return fmt.Errorf("decode private key: %w", err)
	}
	pubBytes, err := agentcrypto.PublicKeyFromPrivate(privBytes)
	if err != nil {
		return fmt.Errorf("derive public key: %w", err)
	}
	client := supabase.New(cfg.Supabase.ProjectRef, cfg.Supabase.RefreshToken, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return client.UploadClusterPublicKey(ctx, cfg.Supabase.ClusterID, pubBytes)
}
