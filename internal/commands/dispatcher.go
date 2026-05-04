// Package commands wires Supabase command events to Proxmox actions.
package commands

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	agentcrypto "github.com/TheLion/proxmoxvue-agent/internal/crypto"
	"github.com/TheLion/proxmoxvue-agent/internal/proxmox"
	"github.com/TheLion/proxmoxvue-agent/internal/supabase"
)

// ProxmoxActor is the subset of proxmox.Client the dispatcher uses.
type ProxmoxActor interface {
	PerformAction(ctx context.Context, kind proxmox.GuestKind, node string, vmid int, action proxmox.Action) (string, error)
	CreateSnapshot(ctx context.Context, kind proxmox.GuestKind, node string, vmid int, name, description string, includeVmState bool) (string, error)
	DeleteSnapshot(ctx context.Context, kind proxmox.GuestKind, node string, vmid int, name string) (string, error)
	RollbackSnapshot(ctx context.Context, kind proxmox.GuestKind, node string, vmid int, name string) (string, error)
	CreateVM(ctx context.Context, spec proxmox.CreateVMSpec) (string, error)
	CreateLXC(ctx context.Context, spec proxmox.CreateLXCSpec) (string, error)
	DeleteGuest(ctx context.Context, kind proxmox.GuestKind, node string, vmid int, destroyDisks, purgeBackups bool) (string, error)
	AwaitTaskCompletion(ctx context.Context, node, upid string, timeout time.Duration) (proxmox.TaskStatus, error)
}

// CommandStore is the subset of supabase.Client the dispatcher uses.
type CommandStore interface {
	ClaimCommand(ctx context.Context, id int64) (bool, error)
	CompleteCommand(ctx context.Context, id int64, status string, result map[string]any) error
}

type Dispatcher struct {
	pve   ProxmoxActor
	store CommandStore

	// ActionTimeout caps how long AwaitTaskCompletion may keep polling.
	// Power actions are typically <5s, but `shutdown` against a
	// slow-responding guest OS can take 1–3 min before Proxmox reports
	// the task as done. 30s was too tight; 5 min also covers slow
	// shutdowns without frustrating the UX (iOS shows its own spinner
	// state) (#1419).
	ActionTimeout time.Duration

	// PrivateKey is the raw X25519 HPKE private key for this cluster.
	// When set, the dispatcher decrypts `password_enc` fields on
	// lxc.create payloads (#1476). Nil → the dispatcher only accepts
	// the plaintext password (legacy path, deprecated). The caller sets
	// this field right after New() — not in the constructor so existing
	// callers don't break.
	PrivateKey []byte
}

func New(pve ProxmoxActor, store CommandStore) *Dispatcher {
	return &Dispatcher{
		pve:           pve,
		store:         store,
		ActionTimeout: 5 * time.Minute,
	}
}

type commandPayload struct {
	GuestKind string `json:"guest_kind"`
	Node      string `json:"node"`
	VMID      int    `json:"vmid"`

	// Snapshot-specific (only present for snapshot.* actions).
	Name           string `json:"name,omitempty"`
	Description    string `json:"description,omitempty"`
	IncludeVmState bool   `json:"include_vmstate,omitempty"`

	// vm.create / lxc.create shared fields.
	Cores         int    `json:"cores,omitempty"`
	MemoryMB      int    `json:"memory_mb,omitempty"`
	DiskStorage   string `json:"disk_storage,omitempty"`
	DiskSizeGB    int    `json:"disk_size_gb,omitempty"`
	NetworkBridge string `json:"network_bridge,omitempty"`

	// lxc.create-specific. PasswordEnc is
	// base64(encapsulated_key||ciphertext) HPKE-sealed with the cluster
	// public_key (#1476). Password (plaintext) is a temporary fallback
	// for pre-#1476 iOS builds — to be removed in a follow-up release.
	Hostname     string `json:"hostname,omitempty"`
	OSTemplate   string `json:"ostemplate,omitempty"`
	Password     string `json:"password,omitempty"`
	PasswordEnc  string `json:"password_enc,omitempty"`
	Unprivileged bool   `json:"unprivileged,omitempty"`

	// guest.delete-specific.
	DestroyDisks bool `json:"destroy_disks,omitempty"`
	PurgeBackups bool `json:"purge_backups,omitempty"`
}

// GuestRef is the parsed payload of a command — everything we need
// post-action to wait for the cluster state update and then push a
// snapshot containing the new state.
type GuestRef struct {
	GuestKind proxmox.GuestKind
	Node      string
	VMID      int
}

// ParseGuestRef extracts the guest coordinates from a Command.
// Returns (_, false) on unknown guest_kind, missing fields or decode
// error; the caller skips the wait gracefully.
func ParseGuestRef(cmd supabase.Command) (GuestRef, bool) {
	var p commandPayload
	if err := json.Unmarshal(cmd.Payload, &p); err != nil {
		return GuestRef{}, false
	}
	kind := proxmox.GuestKind(p.GuestKind)
	if kind != proxmox.GuestKindQEMU && kind != proxmox.GuestKindLXC {
		return GuestRef{}, false
	}
	if p.Node == "" || p.VMID <= 0 {
		return GuestRef{}, false
	}
	return GuestRef{GuestKind: kind, Node: p.Node, VMID: p.VMID}, true
}

// stateExpectation describes what the agent must see in
// /cluster/resources for a given command kind after CompleteCommand,
// plus how long to wait for it before pushing the current (possibly
// still stale) snapshot anyway.
type stateExpectation struct {
	expected string
	timeout  time.Duration
}

// expectedStates maps cmd.Kind to (expected status, max wait). Status
// actions get 10s of slack — in our measurements
// /cluster/resources lagged 1–7s behind task completion, regardless of
// VM size. Future cloud-write actions (vm-create, delete,
// snapshot-rollback) should add their own entry with a fitting
// timeout.
var expectedStates = map[string]stateExpectation{
	"start":    {expected: "running", timeout: 10 * time.Second},
	"resume":   {expected: "running", timeout: 10 * time.Second},
	"reboot":   {expected: "running", timeout: 10 * time.Second},
	"stop":     {expected: "stopped", timeout: 10 * time.Second},
	"shutdown": {expected: "stopped", timeout: 10 * time.Second},
	"suspend":  {expected: "paused", timeout: 10 * time.Second},
}

// ExpectedStateFor returns what the agent must see in
// /cluster/resources after a successful command, plus how long to wait.
// Returns (_, false) for unknown kinds — the caller then skips the wait.
func ExpectedStateFor(kind string) (expected string, timeout time.Duration, ok bool) {
	e, found := expectedStates[kind]
	if !found {
		return "", 0, false
	}
	return e.expected, e.timeout, true
}

// Handle processes a single command: claim → dispatch → await → complete.
// Proxmox errors result in a completed command with status=failed.
// The only error that bubbles up is when claim/complete itself fails
// (network, RLS) — the caller then logs and moves on.
func (d *Dispatcher) Handle(ctx context.Context, cmd supabase.Command) error {
	// 1. TTL check (decision #196). If the row is already expired,
	//    claim it anyway so we can mark it as expired — that way the
	//    iOS UI knows something failed instead of leaving the row on
	//    pending forever.
	if !cmd.ExpiresAt.IsZero() && time.Now().After(cmd.ExpiresAt) {
		if ok, err := d.store.ClaimCommand(ctx, cmd.ID); err == nil && ok {
			slog.Info("command expired", "id", cmd.ID, "kind", cmd.Kind)
			return d.store.CompleteCommand(ctx, cmd.ID, "expired", map[string]any{"reason": "ttl"})
		}
		return nil
	}

	// 2. Claim atomically. If the PATCH didn't hit a row, another
	//    instance already picked it up or the status is no longer
	//    pending.
	ok, err := d.store.ClaimCommand(ctx, cmd.ID)
	if err != nil {
		return fmt.Errorf("claim %d: %w", cmd.ID, err)
	}
	slog.Debug("command claim attempt", "id", cmd.ID, "kind", cmd.Kind, "claimed", ok)
	if !ok {
		return nil
	}
	slog.Info("command claimed", "id", cmd.ID, "kind", cmd.Kind)

	// 3. Validate action + payload. Unknown kinds or broken payloads
	//    are explicitly marked as failed — that way no row ends up
	//    stuck on "claimed" forever without a result.
	action := proxmox.Action(cmd.Kind)
	if !action.IsKnown() {
		return d.store.CompleteCommand(ctx, cmd.ID, "failed", map[string]any{"error": "unknown kind: " + cmd.Kind})
	}
	var p commandPayload
	if err := json.Unmarshal(cmd.Payload, &p); err != nil {
		return d.store.CompleteCommand(ctx, cmd.ID, "failed", map[string]any{"error": "bad payload: " + err.Error()})
	}
	guestKind := proxmox.GuestKind(p.GuestKind)
	if guestKind != proxmox.GuestKindQEMU && guestKind != proxmox.GuestKindLXC {
		return d.store.CompleteCommand(ctx, cmd.ID, "failed", map[string]any{"error": "unknown guest_kind: " + p.GuestKind})
	}
	if p.Node == "" || p.VMID <= 0 {
		return d.store.CompleteCommand(ctx, cmd.ID, "failed", map[string]any{"error": "missing node or vmid"})
	}
	slog.Debug("command payload validated", "id", cmd.ID, "kind", cmd.Kind, "guest_kind", p.GuestKind, "node", p.Node, "vmid", p.VMID)

	// 4. Dispatch to Proxmox. Per-category routing: power actions go to
	//    /status/{action}, snapshot actions to /snapshot. Wait-timeout
	//    is picked per category (vmstate snapshots can take 120s+ on
	//    large VMs, too tight under the 5min default ActionTimeout
	//    alone).
	var upid string
	waitTimeout := d.ActionTimeout
	switch {
	case action.IsPowerAction():
		upid, err = d.pve.PerformAction(ctx, guestKind, p.Node, p.VMID, action)
	case action.IsSnapshotAction():
		if !proxmox.SnapshotNamePattern.MatchString(p.Name) {
			return d.store.CompleteCommand(ctx, cmd.ID, "failed", map[string]any{"error": "invalid snapshot name: " + p.Name})
		}
		switch action {
		case proxmox.ActionSnapshotCreate:
			upid, err = d.pve.CreateSnapshot(ctx, guestKind, p.Node, p.VMID, p.Name, p.Description, p.IncludeVmState)
		case proxmox.ActionSnapshotDelete:
			upid, err = d.pve.DeleteSnapshot(ctx, guestKind, p.Node, p.VMID, p.Name)
		case proxmox.ActionSnapshotRollback:
			upid, err = d.pve.RollbackSnapshot(ctx, guestKind, p.Node, p.VMID, p.Name)
		default:
			return d.store.CompleteCommand(ctx, cmd.ID, "failed", map[string]any{"error": "unrouted snapshot action: " + cmd.Kind})
		}
	case action == proxmox.ActionGuestDelete:
		upid, err = d.pve.DeleteGuest(ctx, guestKind, p.Node, p.VMID, p.DestroyDisks, p.PurgeBackups)
	case action.IsCreateAction():
		switch action {
		case proxmox.ActionVMCreate:
			upid, err = d.pve.CreateVM(ctx, proxmox.CreateVMSpec{
				Node: p.Node, VMID: p.VMID, Name: p.Name,
				Cores: p.Cores, MemoryMB: p.MemoryMB,
				DiskStorage: p.DiskStorage, DiskSizeGB: p.DiskSizeGB,
				NetworkBridge: p.NetworkBridge,
			})
		case proxmox.ActionLXCCreate:
			plaintextPw, pwErr := d.resolveLXCPassword(p)
			if pwErr != nil {
				return d.store.CompleteCommand(ctx, cmd.ID, "failed", map[string]any{"error": pwErr.Error()})
			}
			upid, err = d.pve.CreateLXC(ctx, proxmox.CreateLXCSpec{
				Node: p.Node, VMID: p.VMID, Hostname: p.Hostname,
				OSTemplate: p.OSTemplate, Password: plaintextPw,
				Cores: p.Cores, MemoryMB: p.MemoryMB,
				DiskStorage: p.DiskStorage, DiskSizeGB: p.DiskSizeGB,
				NetworkBridge: p.NetworkBridge, Unprivileged: p.Unprivileged,
			})
		default:
			return d.store.CompleteCommand(ctx, cmd.ID, "failed", map[string]any{"error": "unrouted create action: " + cmd.Kind})
		}
	default:
		// IsKnown filtered this out earlier, so this branch is
		// unreachable — explicit failed-completion to keep us out of a
		// silent-skip when we extend things later.
		return d.store.CompleteCommand(ctx, cmd.ID, "failed", map[string]any{"error": "unrouted kind: " + cmd.Kind})
	}
	if err != nil {
		return d.store.CompleteCommand(ctx, cmd.ID, "failed", map[string]any{"error": err.Error()})
	}
	slog.Info("command dispatched", "id", cmd.ID, "upid", upid, "node", p.Node, "vmid", p.VMID)

	// 5. Wait until the task is done (or timeout).
	st, err := d.pve.AwaitTaskCompletion(ctx, p.Node, upid, waitTimeout)
	if err != nil {
		return d.store.CompleteCommand(ctx, cmd.ID, "failed", map[string]any{"upid": upid, "error": err.Error()})
	}

	// 6. Done.
	result := map[string]any{"upid": upid, "exitstatus": st.ExitStatus}
	status := "done"
	if st.ExitStatus != "OK" {
		status = "failed"
	}
	if err := d.store.CompleteCommand(ctx, cmd.ID, status, result); err != nil {
		slog.Error("complete command failed", "id", cmd.ID, "err", err)
		return err
	}
	slog.Info("command done", "id", cmd.ID, "status", status, "exitstatus", st.ExitStatus)
	return nil
}

// resolveLXCPassword resolves the plaintext LXC password from the
// payload. Preferred: encrypted (`password_enc`) → HPKE-decrypt with
// the agent's private key. Fallback: plaintext (`password`) — only for
// backwards-compat with pre-#1476 iOS builds, logged at WARN so we
// can monitor when that fallback disappears and we can drop the
// plaintext path.
func (d *Dispatcher) resolveLXCPassword(p commandPayload) (string, error) {
	if p.PasswordEnc != "" {
		if d.PrivateKey == nil {
			return "", errors.New("password_enc received but agent has no private key configured (re-run --register)")
		}
		raw, err := base64.StdEncoding.DecodeString(p.PasswordEnc)
		if err != nil {
			return "", fmt.Errorf("decode password_enc: %w", err)
		}
		plaintext, err := agentcrypto.Decrypt(d.PrivateKey, raw)
		if err != nil {
			return "", fmt.Errorf("decrypt password_enc: %w", err)
		}
		return string(plaintext), nil
	}
	if p.Password != "" {
		slog.Warn("lxc.create using plaintext password (deprecated; iOS pre-#1476)")
		return p.Password, nil
	}
	return "", errors.New("missing password (neither password_enc nor password set)")
}
