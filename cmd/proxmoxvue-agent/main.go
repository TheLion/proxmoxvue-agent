package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
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
		fmt.Println("proxmoxvue-agent dev")
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

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	err := runtime.Start(ctx, *configPath)
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

	cfg := config.File{
		Supabase: config.SupabaseConfig{
			ProjectRef:   result.ProjectRef,
			HostID:       result.HostID,
			RefreshToken: result.RefreshToken,
		},
	}
	if err := config.Save(*configPath, cfg); err != nil {
		fmt.Fprintf(os.Stderr, "failed to write config: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("registered host %s, config written to %s\n", result.HostID, *configPath)
	fmt.Println()
	fmt.Println("next: add your Proxmox API token to the config file:")
	fmt.Println("  proxmox:")
	fmt.Println("    api_url: https://<host>:8006")
	fmt.Println("    api_token_id: user@realm!tokenid")
	fmt.Println("    api_token_secret: <uuid>")
	fmt.Println("    verify_tls: false")
	fmt.Println("then: systemctl restart proxmoxvue-agent")
}
