package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/TheLion/proxmoxvue-agent/internal/config"
	"github.com/TheLion/proxmoxvue-agent/internal/enroll"
	"github.com/TheLion/proxmoxvue-agent/internal/runtime"
	"github.com/TheLion/proxmoxvue-agent/internal/supabase"
)

const (
	defaultProjectRef = "fjesjyoxpkalaudfyebx"
	defaultConfigPath = "/etc/proxmoxvue-agent/config.yml"
)

// version wordt geinjecteerd via ldflags bij release-builds:
//
//	go build -ldflags="-X main.version=$(git describe --tags --always --dirty)" ...
//
// Default "dev" voor `go run` en niet-build-script-builds.
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

	// Init slog handler op basis van agent.log_level. Config kan ontbreken
	// of corrupt zijn — die fout komt straks uit runtime.Start; voor logging
	// vallen we hier terug op INFO. Een wel-aanwezige maar ongeldige waarde
	// is fail-fast (anders verbergen we user-fouten).
	level := slog.LevelInfo
	if cfg, err := config.Load(*configPath); err == nil {
		l, err := config.ParseLogLevel(cfg.Agent.LogLevel)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		level = l
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})))

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

	// Non-destructive merge: laad bestaande config (proxmox + agent
	// blijven zoals ze waren), validate huidige log_level, vervang
	// alleen het Supabase-block. Bij parse-fout op een bestaande
	// config: fail-fast — anders verlies je gebruiker-data door
	// een typo te overschrijven.
	var cfg config.File
	if existing, loadErr := config.Load(*configPath); loadErr == nil {
		cfg = existing
		if cfg.Agent.LogLevel != "" {
			if _, vErr := config.ParseLogLevel(cfg.Agent.LogLevel); vErr != nil {
				fmt.Fprintf(os.Stderr, "config bevat ongeldige %v\n", vErr)
				os.Exit(1)
			}
		}
	} else if !errors.Is(loadErr, os.ErrNotExist) {
		fmt.Fprintf(os.Stderr, "failed to read existing config: %v\n", loadErr)
		os.Exit(1)
	}

	cfg.Supabase = config.SupabaseConfig{
		ProjectRef:   result.ProjectRef,
		HostID:       result.HostID,
		RefreshToken: result.RefreshToken,
	}
	if cfg.Agent.LogLevel == "" {
		cfg.Agent.LogLevel = "info"
	}

	if err := config.Save(*configPath, cfg); err != nil {
		fmt.Fprintf(os.Stderr, "failed to write config: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("registered host %s, config written to %s\n", result.HostID, *configPath)
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
		fmt.Println("  of: proxmoxvue-agent --run  (foreground)")
	}
}
