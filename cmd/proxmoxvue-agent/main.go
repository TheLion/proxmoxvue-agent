package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/TheLion/proxmoxvue-agent/internal/config"
	"github.com/TheLion/proxmoxvue-agent/internal/enroll"
)

const defaultProjectRef = "fjesjyoxpkalaudfyebx"

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(2)
	}

	switch os.Args[1] {
	case "--register", "register":
		runRegister(os.Args[2:])
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
	fmt.Fprintln(os.Stderr, "  --version         print version")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "flags for --register:")
	fmt.Fprintln(os.Stderr, "  --config PATH          config destination (default /etc/proxmoxvue-agent/config.yml)")
	fmt.Fprintln(os.Stderr, "  --project-ref REF      Supabase project ref")
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
}
