// Package runtime drives the long-running agent process: auth loop,
// Proxmox poll, Supabase push, graceful shutdown.
package runtime

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/TheLion/proxmoxvue-agent/internal/commands"
	"github.com/TheLion/proxmoxvue-agent/internal/config"
	"github.com/TheLion/proxmoxvue-agent/internal/keysync"
	"github.com/TheLion/proxmoxvue-agent/internal/proxmox"
	"github.com/TheLion/proxmoxvue-agent/internal/supabase"
)

const defaultPollInterval = 30 * time.Second

// Start runs until ctx is cancelled. Returns ErrRefreshRevoked if the
// Supabase session was revoked — caller should exit with a distinct
// code so systemd's restart policy doesn't hammer a dead session.
//
// `version` is logged at startup so a journalctl grep immediately
// reveals which build is running (injected via ldflags in release
// builds).
func Start(ctx context.Context, configPath, version string) error {
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
	slog.Info("agent started", "version", version, "cluster_id", cfg.Supabase.ClusterID, "proxmox", cfg.Proxmox.APIURL)

	interval := defaultPollInterval
	if cfg.Agent.PollIntervalSeconds > 0 {
		interval = time.Duration(cfg.Agent.PollIntervalSeconds) * time.Second
	}

	// === HPKE keypair auto-heal ===
	// Pre-#1476 installs (and any config that lost its private_key) need
	// a keypair before the dispatcher can decrypt LXC create-passwords.
	// Auto-heal: generate locally, persist, upload the matching public
	// key — same effect as `--rotate-key`, just driven by an empty
	// private_key field instead of explicit user request.
	privateKeyB64, generated, ksErr := keysync.EnsurePrivateKey(&cfg, configPath)
	if ksErr != nil {
		return fmt.Errorf("ensure private key: %w", ksErr)
	}
	if generated {
		slog.Info("private_key was missing — generated new keypair, uploading public key")
		if upErr := keysync.UploadPublicKey(ctx, sb, cfg.Supabase.ClusterID, privateKeyB64); upErr != nil {
			// Best-effort: agent keeps running, but iOS LXC creates will
			// fail until the public key is uploaded. Retry happens at
			// next restart since the upload is idempotent.
			slog.Warn("failed to upload public key after auto-heal", "error", upErr.Error())
		} else {
			slog.Info("public key uploaded — LXC create-passwords can now be decrypted")
		}
	}

	// === Command pipeline (alongside the status-push ticker). ===
	dispatcher := commands.New(pve, sb)
	privBytes, decodeErr := base64.StdEncoding.DecodeString(privateKeyB64)
	if decodeErr != nil {
		return fmt.Errorf("decode supabase.private_key: %w", decodeErr)
	}
	dispatcher.PrivateKey = privBytes
	cmdCh, err := sb.SubscribeCommands(ctx, cfg.Supabase.ClusterID)
	if err != nil {
		return fmt.Errorf("subscribe commands: %w", err)
	}
	go func() {
		for cmd := range cmdCh {
			go handleCommand(ctx, dispatcher, pve, sb, cfg.Supabase.ClusterID, cmd)
		}
	}()

	// === Read-RPC pipeline (cluster overview/details on-demand). ===
	readDispatcher := commands.NewReadDispatcher(pve, sb)
	readCh, err := sb.SubscribeReadCommands(ctx, cfg.Supabase.ClusterID)
	if err != nil {
		return fmt.Errorf("subscribe read_commands: %w", err)
	}
	go func() {
		for cmd := range readCh {
			go handleReadCommand(ctx, readDispatcher, cmd)
		}
	}()

	// First push happens immediately so the cluster has a snapshot
	// right after boot without waiting a full tick.
	pushOnce(ctx, pve, sb, cfg.Supabase.ClusterID)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			slog.Info("agent stopping", "reason", ctx.Err())
			return nil
		case <-ticker.C:
			if err := pushOnce(ctx, pve, sb, cfg.Supabase.ClusterID); err != nil {
				if errors.Is(err, supabase.ErrRefreshRevoked) {
					return err
				}
			}
		}
	}
}

func handleCommand(ctx context.Context, d *commands.Dispatcher, pve *proxmox.Client, sb *supabase.Client, clusterID string, cmd supabase.Command) {
	if err := d.Handle(ctx, cmd); err != nil {
		slog.Error("command handle failed", "id", cmd.ID, "err", err)
		return
	}
	// /cluster/resources is eventually consistent — empirically 1–7s
	// behind task completion. A direct push after CompleteCommand
	// therefore typically still carries the old state. Actively wait
	// until Proxmox' aggregate cache reflects the new state, then push.
	// On timeout we push anyway (UX degradation to the routine 30s
	// tick, no correctness issue).
	waitForClusterStateMatch(ctx, pve, cmd)
	if err := pushOnce(ctx, pve, sb, clusterID); err != nil {
		slog.Warn("post-action snapshot push failed", "id", cmd.ID, "err", err)
	}
}

// waitForClusterStateMatch polls /cluster/resources until the target
// guest is in the expected status or the per-kind timeout elapses.
// No-op for commands with an unknown kind or an unparseable payload.
func waitForClusterStateMatch(ctx context.Context, pve *proxmox.Client, cmd supabase.Command) {
	expected, timeout, ok := commands.ExpectedStateFor(cmd.Kind)
	if !ok {
		return
	}
	ref, ok := commands.ParseGuestRef(cmd)
	if !ok {
		return
	}

	deadline := time.Now().Add(timeout)
	pollInterval := 500 * time.Millisecond
	for time.Now().Before(deadline) {
		resources, err := pve.ClusterResources(ctx)
		if err == nil && hasGuestState(resources, ref.VMID, expected) {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(pollInterval):
		}
	}
	slog.Info("post-action state-match timeout — pushing current state", "id", cmd.ID, "vmid", ref.VMID, "expected", expected)
}

// hasGuestState parses the raw /cluster/resources payload and checks
// whether the guest with `vmid` is in the `expected` status. The
// payload array contains nodes/qemu/lxc/storage/network entries with
// variable fields — a minimal `[{vmid, status}]` decode is enough.
func hasGuestState(resources json.RawMessage, vmid int, expected string) bool {
	var entries []struct {
		VMID   int    `json:"vmid"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal(resources, &entries); err != nil {
		return false
	}
	for _, e := range entries {
		if e.VMID == vmid && e.Status == expected {
			return true
		}
	}
	return false
}

func handleReadCommand(ctx context.Context, d *commands.ReadDispatcher, cmd supabase.ReadCommand) {
	if err := d.Handle(ctx, cmd); err != nil {
		slog.Error("read_command handle failed", "id", cmd.ID, "err", err)
	}
}

func pushOnce(ctx context.Context, pve *proxmox.Client, sb *supabase.Client, clusterID string) error {
	resources, err := pve.ClusterResources(ctx)
	if err != nil {
		slog.Error("poll proxmox failed", "err", err)
		return err
	}
	if err := sb.PushSnapshot(ctx, clusterID, resources); err != nil {
		slog.Error("push snapshot failed", "err", err)
		return err
	}
	// DEBUG: routine 30s pushes would otherwise drown out the
	// read_command lines you actually want to see while debugging.
	// Push failures stay at ERROR.
	slog.Debug("snapshot pushed", "bytes", len(resources))
	return nil
}

func validate(cfg config.File) error {
	if cfg.Supabase.ProjectRef == "" || cfg.Supabase.ClusterID == "" || cfg.Supabase.RefreshToken == "" {
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
