// Package commands wires Supabase command events to Proxmox actions.
package commands

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/TheLion/proxmoxvue-agent/internal/proxmox"
	"github.com/TheLion/proxmoxvue-agent/internal/supabase"
)

// ProxmoxActor is de subset van proxmox.Client die de dispatcher gebruikt.
type ProxmoxActor interface {
	PerformAction(ctx context.Context, kind proxmox.GuestKind, node string, vmid int, action proxmox.Action) (string, error)
	AwaitTaskCompletion(ctx context.Context, node, upid string, timeout time.Duration) (proxmox.TaskStatus, error)
}

// CommandStore is de subset van supabase.Client die de dispatcher gebruikt.
type CommandStore interface {
	ClaimCommand(ctx context.Context, id int64) (bool, error)
	CompleteCommand(ctx context.Context, id int64, status string, result map[string]any) error
}

type Dispatcher struct {
	pve   ProxmoxActor
	store CommandStore

	// ActionTimeout begrenst hoe lang AwaitTaskCompletion mag doorpolen.
	// Power-acties zijn typisch <5s; 30s komt overeen met de iOS default.
	ActionTimeout time.Duration
}

func New(pve ProxmoxActor, store CommandStore) *Dispatcher {
	return &Dispatcher{
		pve:           pve,
		store:         store,
		ActionTimeout: 30 * time.Second,
	}
}

type commandPayload struct {
	GuestKind string `json:"guest_kind"`
	Node      string `json:"node"`
	VMID      int    `json:"vmid"`
}

// Handle verwerkt één command: claim → dispatch → await → complete.
// Proxmox-fouten leiden tot een completed command met status=failed.
// De enige error die naar boven bubbelt is wanneer claim/complete zelf
// faalt (netwerk, RLS) — dan logt de caller het en gaat door.
func (d *Dispatcher) Handle(ctx context.Context, cmd supabase.Command) error {
	// 1. TTL-check (decision #196). Als de rij al expired is, claim'm
	//    alsnog om 'm als expired te markeren — dan weet de iOS-UI dat
	//    er iets niet is gelukt i.p.v. hem eeuwig op pending te laten.
	if !cmd.ExpiresAt.IsZero() && time.Now().After(cmd.ExpiresAt) {
		if ok, err := d.store.ClaimCommand(ctx, cmd.ID); err == nil && ok {
			slog.Info("command expired", "id", cmd.ID, "kind", cmd.Kind)
			return d.store.CompleteCommand(ctx, cmd.ID, "expired", map[string]any{"reason": "ttl"})
		}
		return nil
	}

	// 2. Claim atomair. Als de PATCH geen row raakte, heeft een andere
	//    instance 'm al opgepakt of staat de status niet meer op pending.
	ok, err := d.store.ClaimCommand(ctx, cmd.ID)
	if err != nil {
		return fmt.Errorf("claim %d: %w", cmd.ID, err)
	}
	if !ok {
		return nil
	}
	slog.Info("command claimed", "id", cmd.ID, "kind", cmd.Kind)

	// 3. Valideer action + payload. Onbekende kind of kapotte payload
	//    markeren we expliciet als failed — zo eindigt geen row eeuwig
	//    op "claimed" zonder result.
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

	// 4. Dispatch naar Proxmox.
	upid, err := d.pve.PerformAction(ctx, guestKind, p.Node, p.VMID, action)
	if err != nil {
		return d.store.CompleteCommand(ctx, cmd.ID, "failed", map[string]any{"error": err.Error()})
	}
	slog.Info("command dispatched", "id", cmd.ID, "upid", upid, "node", p.Node, "vmid", p.VMID)

	// 5. Wacht tot de task klaar is (of timeout).
	st, err := d.pve.AwaitTaskCompletion(ctx, p.Node, upid, d.ActionTimeout)
	if err != nil {
		return d.store.CompleteCommand(ctx, cmd.ID, "failed", map[string]any{"upid": upid, "error": err.Error()})
	}

	// 6. Klaar.
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
