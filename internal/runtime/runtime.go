// Package runtime drives the long-running agent process: auth loop,
// Proxmox poll, Supabase push, graceful shutdown.
package runtime

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/TheLion/proxmoxvue-agent/internal/commands"
	"github.com/TheLion/proxmoxvue-agent/internal/config"
	"github.com/TheLion/proxmoxvue-agent/internal/proxmox"
	"github.com/TheLion/proxmoxvue-agent/internal/supabase"
)

const defaultPollInterval = 30 * time.Second

// Start runs until ctx is cancelled. Returns ErrRefreshRevoked if the
// Supabase session was revoked — caller should exit with a distinct
// code so systemd's restart policy doesn't hammer a dead session.
func Start(ctx context.Context, configPath string) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	if err := validate(cfg); err != nil {
		return fmt.Errorf("config invalid: %w", err)
	}

	pve := proxmox.New(proxmox.Config{
		APIURL:         cfg.Proxmox.APIURL,
		APITokenID:     cfg.Proxmox.APITokenID,
		APITokenSecret: cfg.Proxmox.APITokenSecret,
		VerifyTLS:      cfg.Proxmox.VerifyTLS,
	})

	sb := supabase.New(cfg.Supabase.ProjectRef, cfg.Supabase.RefreshToken, persistRefreshTo(configPath))

	// Fail fast: bad Proxmox creds or bad Supabase session should exit
	// before we start polling.
	if _, err := pve.Version(ctx); err != nil {
		return fmt.Errorf("proxmox version check: %w", err)
	}
	if err := sb.Ping(ctx); err != nil {
		return fmt.Errorf("supabase initial auth: %w", err)
	}
	slog.Info("agent started", "host_id", cfg.Supabase.HostID, "proxmox", cfg.Proxmox.APIURL)

	interval := defaultPollInterval
	if cfg.Agent.PollIntervalSeconds > 0 {
		interval = time.Duration(cfg.Agent.PollIntervalSeconds) * time.Second
	}

	// === Command pipeline (naast de status-push-ticker). ===
	dispatcher := commands.New(pve, sb)
	cmdCh, err := sb.SubscribeCommands(ctx, cfg.Supabase.HostID)
	if err != nil {
		return fmt.Errorf("subscribe commands: %w", err)
	}
	go func() {
		for cmd := range cmdCh {
			go handleCommand(ctx, dispatcher, pve, sb, cfg.Supabase.HostID, cmd)
		}
	}()

	// === Read-RPC pipeline (cluster overview/details on-demand). ===
	readDispatcher := commands.NewReadDispatcher(pve, sb)
	readCh, err := sb.SubscribeReadCommands(ctx, cfg.Supabase.HostID)
	if err != nil {
		return fmt.Errorf("subscribe read_commands: %w", err)
	}
	go func() {
		for cmd := range readCh {
			go handleReadCommand(ctx, readDispatcher, cmd)
		}
	}()

	// First push happens immediately so hosts.last_seen_at becomes
	// non-null at boot without waiting a full tick.
	pushOnce(ctx, pve, sb, cfg.Supabase.HostID)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			slog.Info("agent stopping", "reason", ctx.Err())
			return nil
		case <-ticker.C:
			if err := pushOnce(ctx, pve, sb, cfg.Supabase.HostID); err != nil {
				if errors.Is(err, supabase.ErrRefreshRevoked) {
					return err
				}
			}
		}
	}
}

func handleCommand(ctx context.Context, d *commands.Dispatcher, pve *proxmox.Client, sb *supabase.Client, hostID string, cmd supabase.Command) {
	if err := d.Handle(ctx, cmd); err != nil {
		slog.Error("command handle failed", "id", cmd.ID, "err", err)
		return
	}
	// Direct na een afgehandeld command een verse snapshot pushen, zodat de
	// iOS cloud-read-pad de nieuwe guest-state binnen ~1s ziet i.p.v. te
	// wachten op de volgende 30s-tick. Bij expired/already-claimed cycles is
	// dit redundant, maar de extra Proxmox+Supabase-call is goedkoop genoeg
	// om het ongeconditioneerd te doen.
	if err := pushOnce(ctx, pve, sb, hostID); err != nil {
		slog.Warn("post-action snapshot push failed", "id", cmd.ID, "err", err)
	}
}

func handleReadCommand(ctx context.Context, d *commands.ReadDispatcher, cmd supabase.ReadCommand) {
	if err := d.Handle(ctx, cmd); err != nil {
		slog.Error("read_command handle failed", "id", cmd.ID, "err", err)
	}
}

func pushOnce(ctx context.Context, pve *proxmox.Client, sb *supabase.Client, hostID string) error {
	resources, err := pve.ClusterResources(ctx)
	if err != nil {
		slog.Error("poll proxmox failed", "err", err)
		return err
	}
	if err := sb.PushSnapshot(ctx, hostID, resources); err != nil {
		slog.Error("push snapshot failed", "err", err)
		return err
	}
	slog.Info("snapshot pushed", "bytes", len(resources))
	return nil
}

func validate(cfg config.File) error {
	if cfg.Supabase.ProjectRef == "" || cfg.Supabase.HostID == "" || cfg.Supabase.RefreshToken == "" {
		return fmt.Errorf("supabase section incomplete (run --register first)")
	}
	pvCfg := proxmox.Config{
		APIURL:         cfg.Proxmox.APIURL,
		APITokenID:     cfg.Proxmox.APITokenID,
		APITokenSecret: cfg.Proxmox.APITokenSecret,
	}
	return pvCfg.Valid()
}

// persistRefreshTo returns a PersistRefreshFunc that atomically rewrites
// config.yml with the rotated refresh token. Write-to-temp + rename
// keeps the file valid even if the agent crashes mid-write.
func persistRefreshTo(path string) supabase.PersistRefreshFunc {
	return func(newToken string) error {
		current, err := config.Load(path)
		if err != nil {
			return fmt.Errorf("reload config: %w", err)
		}
		current.Supabase.RefreshToken = newToken

		dir := filepath.Dir(path)
		tmp, err := os.CreateTemp(dir, ".config-*.yml")
		if err != nil {
			return fmt.Errorf("create temp: %w", err)
		}
		tmpPath := tmp.Name()
		tmp.Close()

		if err := config.Save(tmpPath, current); err != nil {
			os.Remove(tmpPath)
			return err
		}
		if err := os.Chmod(tmpPath, 0o600); err != nil {
			os.Remove(tmpPath)
			return fmt.Errorf("chmod temp: %w", err)
		}
		if err := os.Rename(tmpPath, path); err != nil {
			os.Remove(tmpPath)
			return fmt.Errorf("rename temp: %w", err)
		}
		return nil
	}
}
