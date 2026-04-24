package commands

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/TheLion/proxmoxvue-agent/internal/proxmox"
	"github.com/TheLion/proxmoxvue-agent/internal/supabase"
)

type fakeActor struct {
	perform func(ctx context.Context, kind proxmox.GuestKind, node string, vmid int, action proxmox.Action) (string, error)
	await   func(ctx context.Context, node, upid string, timeout time.Duration) (proxmox.TaskStatus, error)
}

func (f *fakeActor) PerformAction(ctx context.Context, kind proxmox.GuestKind, node string, vmid int, action proxmox.Action) (string, error) {
	return f.perform(ctx, kind, node, vmid, action)
}
func (f *fakeActor) AwaitTaskCompletion(ctx context.Context, node, upid string, timeout time.Duration) (proxmox.TaskStatus, error) {
	return f.await(ctx, node, upid, timeout)
}

type fakeStore struct {
	mu        sync.Mutex
	claimed   map[int64]bool
	completed map[int64]completion
	claimRet  func(id int64) (bool, error)
}

type completion struct {
	status string
	result map[string]any
}

func (f *fakeStore) ClaimCommand(ctx context.Context, id int64) (bool, error) {
	ok, err := f.claimRet(id)
	if ok {
		f.mu.Lock()
		if f.claimed == nil {
			f.claimed = map[int64]bool{}
		}
		f.claimed[id] = true
		f.mu.Unlock()
	}
	return ok, err
}
func (f *fakeStore) CompleteCommand(ctx context.Context, id int64, status string, result map[string]any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.completed == nil {
		f.completed = map[int64]completion{}
	}
	f.completed[id] = completion{status: status, result: result}
	return nil
}

func newCmd(id int64, kind, guestKind, node string, vmid int) supabase.Command {
	payload, _ := json.Marshal(map[string]any{"guest_kind": guestKind, "node": node, "vmid": vmid})
	return supabase.Command{
		ID:        id,
		HostID:    "host-abc",
		Kind:      kind,
		Payload:   payload,
		Status:    "pending",
		ExpiresAt: time.Now().Add(30 * time.Second),
	}
}

func TestHandle_HappyPath(t *testing.T) {
	actor := &fakeActor{
		perform: func(ctx context.Context, kind proxmox.GuestKind, node string, vmid int, action proxmox.Action) (string, error) {
			if kind != proxmox.GuestKindQEMU || node != "n1" || vmid != 112 || action != proxmox.ActionStart {
				t.Errorf("unexpected args: kind=%s node=%s vmid=%d action=%s", kind, node, vmid, action)
			}
			return "UPID:x", nil
		},
		await: func(ctx context.Context, node, upid string, timeout time.Duration) (proxmox.TaskStatus, error) {
			return proxmox.TaskStatus{UPID: upid, Done: true, ExitStatus: "OK"}, nil
		},
	}
	store := &fakeStore{claimRet: func(id int64) (bool, error) { return true, nil }}
	d := New(actor, store)

	if err := d.Handle(context.Background(), newCmd(7, "start", "qemu", "n1", 112)); err != nil {
		t.Fatalf("err: %v", err)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.completed[7].status != "done" {
		t.Errorf("status=%q", store.completed[7].status)
	}
	if store.completed[7].result["exitstatus"] != "OK" {
		t.Errorf("exit=%v", store.completed[7].result["exitstatus"])
	}
}

func TestHandle_AlreadyClaimed_NoOp(t *testing.T) {
	performed := false
	actor := &fakeActor{
		perform: func(context.Context, proxmox.GuestKind, string, int, proxmox.Action) (string, error) {
			performed = true
			return "", nil
		},
		await: func(context.Context, string, string, time.Duration) (proxmox.TaskStatus, error) {
			return proxmox.TaskStatus{}, nil
		},
	}
	store := &fakeStore{claimRet: func(int64) (bool, error) { return false, nil }}
	d := New(actor, store)
	if err := d.Handle(context.Background(), newCmd(7, "start", "qemu", "n1", 112)); err != nil {
		t.Fatal(err)
	}
	if performed {
		t.Error("PerformAction was called despite failed claim")
	}
	if _, ok := store.completed[7]; ok {
		t.Error("CompleteCommand was called despite failed claim")
	}
}

func TestHandle_UnknownKind_Fails(t *testing.T) {
	actor := &fakeActor{
		perform: func(context.Context, proxmox.GuestKind, string, int, proxmox.Action) (string, error) {
			t.Error("should not call Proxmox")
			return "", nil
		},
		await: func(context.Context, string, string, time.Duration) (proxmox.TaskStatus, error) {
			return proxmox.TaskStatus{}, nil
		},
	}
	store := &fakeStore{claimRet: func(int64) (bool, error) { return true, nil }}
	d := New(actor, store)
	cmd := newCmd(7, "teleport", "qemu", "n1", 112)
	if err := d.Handle(context.Background(), cmd); err != nil {
		t.Fatal(err)
	}
	if store.completed[7].status != "failed" {
		t.Errorf("status=%q", store.completed[7].status)
	}
}

func TestHandle_ProxmoxError_MarksFailed(t *testing.T) {
	actor := &fakeActor{
		perform: func(context.Context, proxmox.GuestKind, string, int, proxmox.Action) (string, error) {
			return "", fmt.Errorf("boom")
		},
		await: func(context.Context, string, string, time.Duration) (proxmox.TaskStatus, error) {
			return proxmox.TaskStatus{}, nil
		},
	}
	store := &fakeStore{claimRet: func(int64) (bool, error) { return true, nil }}
	d := New(actor, store)
	if err := d.Handle(context.Background(), newCmd(7, "start", "qemu", "n1", 112)); err != nil {
		t.Fatal(err)
	}
	if store.completed[7].status != "failed" {
		t.Errorf("status=%q", store.completed[7].status)
	}
	if got, _ := store.completed[7].result["error"].(string); got != "boom" {
		t.Errorf("err=%q", got)
	}
}

func TestHandle_TTLExpired_MarksExpired(t *testing.T) {
	actor := &fakeActor{
		perform: func(context.Context, proxmox.GuestKind, string, int, proxmox.Action) (string, error) {
			t.Error("should not call Proxmox for expired command")
			return "", nil
		},
		await: func(context.Context, string, string, time.Duration) (proxmox.TaskStatus, error) {
			return proxmox.TaskStatus{}, nil
		},
	}
	store := &fakeStore{claimRet: func(int64) (bool, error) { return true, nil }}
	d := New(actor, store)
	cmd := newCmd(7, "start", "qemu", "n1", 112)
	cmd.ExpiresAt = time.Now().Add(-time.Second) // verleden
	if err := d.Handle(context.Background(), cmd); err != nil {
		t.Fatal(err)
	}
	if store.completed[7].status != "expired" {
		t.Errorf("status=%q want expired", store.completed[7].status)
	}
}

func TestHandle_BadGuestKind_Fails(t *testing.T) {
	actor := &fakeActor{
		perform: func(context.Context, proxmox.GuestKind, string, int, proxmox.Action) (string, error) {
			t.Error("should not call Proxmox")
			return "", nil
		},
		await: func(context.Context, string, string, time.Duration) (proxmox.TaskStatus, error) {
			return proxmox.TaskStatus{}, nil
		},
	}
	store := &fakeStore{claimRet: func(int64) (bool, error) { return true, nil }}
	d := New(actor, store)
	cmd := newCmd(7, "start", "vmware", "n1", 112)
	if err := d.Handle(context.Background(), cmd); err != nil {
		t.Fatal(err)
	}
	if store.completed[7].status != "failed" {
		t.Errorf("status=%q", store.completed[7].status)
	}
}
