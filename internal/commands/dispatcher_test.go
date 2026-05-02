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
	perform          func(ctx context.Context, kind proxmox.GuestKind, node string, vmid int, action proxmox.Action) (string, error)
	createSnapshot   func(ctx context.Context, kind proxmox.GuestKind, node string, vmid int, name, description string, includeVmState bool) (string, error)
	deleteSnapshot   func(ctx context.Context, kind proxmox.GuestKind, node string, vmid int, name string) (string, error)
	rollbackSnapshot func(ctx context.Context, kind proxmox.GuestKind, node string, vmid int, name string) (string, error)
	createVM         func(ctx context.Context, spec proxmox.CreateVMSpec) (string, error)
	createLXC        func(ctx context.Context, spec proxmox.CreateLXCSpec) (string, error)
	deleteGuest      func(ctx context.Context, kind proxmox.GuestKind, node string, vmid int, destroyDisks, purgeBackups bool) (string, error)
	await            func(ctx context.Context, node, upid string, timeout time.Duration) (proxmox.TaskStatus, error)
}

func (f *fakeActor) PerformAction(ctx context.Context, kind proxmox.GuestKind, node string, vmid int, action proxmox.Action) (string, error) {
	return f.perform(ctx, kind, node, vmid, action)
}
func (f *fakeActor) CreateSnapshot(ctx context.Context, kind proxmox.GuestKind, node string, vmid int, name, description string, includeVmState bool) (string, error) {
	if f.createSnapshot == nil {
		return "", fmt.Errorf("CreateSnapshot not configured for this test")
	}
	return f.createSnapshot(ctx, kind, node, vmid, name, description, includeVmState)
}
func (f *fakeActor) DeleteSnapshot(ctx context.Context, kind proxmox.GuestKind, node string, vmid int, name string) (string, error) {
	if f.deleteSnapshot == nil {
		return "", fmt.Errorf("DeleteSnapshot not configured for this test")
	}
	return f.deleteSnapshot(ctx, kind, node, vmid, name)
}
func (f *fakeActor) RollbackSnapshot(ctx context.Context, kind proxmox.GuestKind, node string, vmid int, name string) (string, error) {
	if f.rollbackSnapshot == nil {
		return "", fmt.Errorf("RollbackSnapshot not configured for this test")
	}
	return f.rollbackSnapshot(ctx, kind, node, vmid, name)
}
func (f *fakeActor) CreateVM(ctx context.Context, spec proxmox.CreateVMSpec) (string, error) {
	if f.createVM == nil {
		return "", fmt.Errorf("CreateVM not configured for this test")
	}
	return f.createVM(ctx, spec)
}
func (f *fakeActor) CreateLXC(ctx context.Context, spec proxmox.CreateLXCSpec) (string, error) {
	if f.createLXC == nil {
		return "", fmt.Errorf("CreateLXC not configured for this test")
	}
	return f.createLXC(ctx, spec)
}
func (f *fakeActor) DeleteGuest(ctx context.Context, kind proxmox.GuestKind, node string, vmid int, destroyDisks, purgeBackups bool) (string, error) {
	if f.deleteGuest == nil {
		return "", fmt.Errorf("DeleteGuest not configured for this test")
	}
	return f.deleteGuest(ctx, kind, node, vmid, destroyDisks, purgeBackups)
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

func newSnapshotCreateCmd(id int64, guestKind, node string, vmid int, name, description string, includeVmState bool) supabase.Command {
	payload, _ := json.Marshal(map[string]any{
		"guest_kind":      guestKind,
		"node":            node,
		"vmid":            vmid,
		"name":            name,
		"description":     description,
		"include_vmstate": includeVmState,
	})
	return supabase.Command{
		ID:        id,
		HostID:    "host-abc",
		Kind:      string(proxmox.ActionSnapshotCreate),
		Payload:   payload,
		Status:    "pending",
		ExpiresAt: time.Now().Add(30 * time.Second),
	}
}

func TestHandle_SnapshotCreate_HappyPath(t *testing.T) {
	var captured struct {
		kind                                  proxmox.GuestKind
		node, name, description               string
		vmid                                  int
		includeVmState                        bool
	}
	actor := &fakeActor{
		perform: func(context.Context, proxmox.GuestKind, string, int, proxmox.Action) (string, error) {
			t.Error("PerformAction should not be called for snapshot.create")
			return "", nil
		},
		createSnapshot: func(ctx context.Context, kind proxmox.GuestKind, node string, vmid int, name, description string, includeVmState bool) (string, error) {
			captured.kind = kind
			captured.node = node
			captured.vmid = vmid
			captured.name = name
			captured.description = description
			captured.includeVmState = includeVmState
			return "UPID:snap", nil
		},
		await: func(ctx context.Context, node, upid string, timeout time.Duration) (proxmox.TaskStatus, error) {
			return proxmox.TaskStatus{UPID: upid, Done: true, ExitStatus: "OK"}, nil
		},
	}
	store := &fakeStore{claimRet: func(int64) (bool, error) { return true, nil }}
	d := New(actor, store)

	cmd := newSnapshotCreateCmd(11, "qemu", "n1", 112, "snap_alpha", "before upgrade", true)
	if err := d.Handle(context.Background(), cmd); err != nil {
		t.Fatalf("err: %v", err)
	}
	if captured.kind != proxmox.GuestKindQEMU || captured.node != "n1" || captured.vmid != 112 ||
		captured.name != "snap_alpha" || captured.description != "before upgrade" || !captured.includeVmState {
		t.Errorf("unexpected snapshot args: %+v", captured)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.completed[11].status != "done" {
		t.Errorf("status=%q", store.completed[11].status)
	}
	if store.completed[11].result["upid"] != "UPID:snap" {
		t.Errorf("upid=%v", store.completed[11].result["upid"])
	}
}

func TestHandle_SnapshotCreate_InvalidName_Fails(t *testing.T) {
	actor := &fakeActor{
		perform: func(context.Context, proxmox.GuestKind, string, int, proxmox.Action) (string, error) {
			return "", nil
		},
		createSnapshot: func(context.Context, proxmox.GuestKind, string, int, string, string, bool) (string, error) {
			t.Error("CreateSnapshot should not be called for invalid name")
			return "", nil
		},
		await: func(context.Context, string, string, time.Duration) (proxmox.TaskStatus, error) {
			return proxmox.TaskStatus{}, nil
		},
	}
	store := &fakeStore{claimRet: func(int64) (bool, error) { return true, nil }}
	d := New(actor, store)

	// Beginnen met cijfer is volgens Proxmox-regels niet geldig.
	cmd := newSnapshotCreateCmd(12, "qemu", "n1", 112, "1bad", "", false)
	if err := d.Handle(context.Background(), cmd); err != nil {
		t.Fatal(err)
	}
	if store.completed[12].status != "failed" {
		t.Errorf("status=%q", store.completed[12].status)
	}

	// Eén-char naam — Proxmox eist minstens 2 chars (regex `[a-z][a-z0-9_-]+`).
	cmd = newSnapshotCreateCmd(15, "qemu", "n1", 112, "a", "", false)
	if err := d.Handle(context.Background(), cmd); err != nil {
		t.Fatal(err)
	}
	if store.completed[15].status != "failed" {
		t.Errorf("one-char name: status=%q", store.completed[15].status)
	}
}

func newSnapshotDeleteCmd(id int64, guestKind, node string, vmid int, name string) supabase.Command {
	payload, _ := json.Marshal(map[string]any{
		"guest_kind": guestKind,
		"node":       node,
		"vmid":       vmid,
		"name":       name,
	})
	return supabase.Command{
		ID:        id,
		HostID:    "host-abc",
		Kind:      string(proxmox.ActionSnapshotDelete),
		Payload:   payload,
		Status:    "pending",
		ExpiresAt: time.Now().Add(30 * time.Second),
	}
}

func TestHandle_SnapshotDelete_HappyPath(t *testing.T) {
	var capturedName string
	actor := &fakeActor{
		perform: func(context.Context, proxmox.GuestKind, string, int, proxmox.Action) (string, error) {
			t.Error("PerformAction should not be called for snapshot.delete")
			return "", nil
		},
		deleteSnapshot: func(ctx context.Context, kind proxmox.GuestKind, node string, vmid int, name string) (string, error) {
			capturedName = name
			return "UPID:del", nil
		},
		await: func(ctx context.Context, node, upid string, timeout time.Duration) (proxmox.TaskStatus, error) {
			return proxmox.TaskStatus{UPID: upid, Done: true, ExitStatus: "OK"}, nil
		},
	}
	store := &fakeStore{claimRet: func(int64) (bool, error) { return true, nil }}
	d := New(actor, store)

	cmd := newSnapshotDeleteCmd(13, "qemu", "n1", 112, "snap_alpha")
	if err := d.Handle(context.Background(), cmd); err != nil {
		t.Fatalf("err: %v", err)
	}
	if capturedName != "snap_alpha" {
		t.Errorf("captured name=%q want snap_alpha", capturedName)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.completed[13].status != "done" {
		t.Errorf("status=%q", store.completed[13].status)
	}
	if store.completed[13].result["upid"] != "UPID:del" {
		t.Errorf("upid=%v", store.completed[13].result["upid"])
	}
}

func newSnapshotRollbackCmd(id int64, guestKind, node string, vmid int, name string, includeVmState bool) supabase.Command {
	payload, _ := json.Marshal(map[string]any{
		"guest_kind":      guestKind,
		"node":            node,
		"vmid":            vmid,
		"name":            name,
		"include_vmstate": includeVmState,
	})
	return supabase.Command{
		ID:        id,
		HostID:    "host-abc",
		Kind:      string(proxmox.ActionSnapshotRollback),
		Payload:   payload,
		Status:    "pending",
		ExpiresAt: time.Now().Add(30 * time.Second),
	}
}

func TestHandle_SnapshotRollback_HappyPath(t *testing.T) {
	var capturedName string
	actor := &fakeActor{
		perform: func(context.Context, proxmox.GuestKind, string, int, proxmox.Action) (string, error) {
			t.Error("PerformAction should not be called for snapshot.rollback")
			return "", nil
		},
		rollbackSnapshot: func(ctx context.Context, kind proxmox.GuestKind, node string, vmid int, name string) (string, error) {
			capturedName = name
			return "UPID:rb", nil
		},
		await: func(ctx context.Context, node, upid string, timeout time.Duration) (proxmox.TaskStatus, error) {
			return proxmox.TaskStatus{UPID: upid, Done: true, ExitStatus: "OK"}, nil
		},
	}
	store := &fakeStore{claimRet: func(int64) (bool, error) { return true, nil }}
	d := New(actor, store)

	cmd := newSnapshotRollbackCmd(14, "qemu", "n1", 112, "snap_alpha", true)
	if err := d.Handle(context.Background(), cmd); err != nil {
		t.Fatalf("err: %v", err)
	}
	if capturedName != "snap_alpha" {
		t.Errorf("captured name=%q want snap_alpha", capturedName)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.completed[14].status != "done" {
		t.Errorf("status=%q", store.completed[14].status)
	}
	if store.completed[14].result["upid"] != "UPID:rb" {
		t.Errorf("upid=%v", store.completed[14].result["upid"])
	}
}

func newVMCreateCmd(id int64, node string, vmid int, name string, cores, memMB int, store string, diskGB int, bridge string) supabase.Command {
	payload, _ := json.Marshal(map[string]any{
		"guest_kind":     "qemu",
		"node":           node,
		"vmid":           vmid,
		"name":           name,
		"cores":          cores,
		"memory_mb":      memMB,
		"disk_storage":   store,
		"disk_size_gb":   diskGB,
		"network_bridge": bridge,
	})
	return supabase.Command{
		ID:        id,
		HostID:    "host-abc",
		Kind:      string(proxmox.ActionVMCreate),
		Payload:   payload,
		Status:    "pending",
		ExpiresAt: time.Now().Add(30 * time.Second),
	}
}

func TestHandle_VMCreate_HappyPath(t *testing.T) {
	var captured proxmox.CreateVMSpec
	actor := &fakeActor{
		createVM: func(ctx context.Context, spec proxmox.CreateVMSpec) (string, error) {
			captured = spec
			return "UPID:vmnew", nil
		},
		await: func(ctx context.Context, node, upid string, timeout time.Duration) (proxmox.TaskStatus, error) {
			return proxmox.TaskStatus{UPID: upid, Done: true, ExitStatus: "OK"}, nil
		},
	}
	store := &fakeStore{claimRet: func(int64) (bool, error) { return true, nil }}
	d := New(actor, store)

	cmd := newVMCreateCmd(30, "n1", 200, "alpha", 2, 2048, "local-lvm", 20, "vmbr0")
	if err := d.Handle(context.Background(), cmd); err != nil {
		t.Fatalf("err: %v", err)
	}
	want := proxmox.CreateVMSpec{Node: "n1", VMID: 200, Name: "alpha", Cores: 2, MemoryMB: 2048, DiskStorage: "local-lvm", DiskSizeGB: 20, NetworkBridge: "vmbr0"}
	if captured != want {
		t.Errorf("spec mismatch: got %+v, want %+v", captured, want)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.completed[30].status != "done" {
		t.Errorf("status=%q", store.completed[30].status)
	}
}

func newLXCCreateCmd(id int64, node string, vmid int, hostname, ostemplate, password string, cores, memMB int, store string, diskGB int, bridge string, unprivileged bool) supabase.Command {
	payload, _ := json.Marshal(map[string]any{
		"guest_kind":     "lxc",
		"node":           node,
		"vmid":           vmid,
		"hostname":       hostname,
		"ostemplate":     ostemplate,
		"password":       password,
		"cores":          cores,
		"memory_mb":      memMB,
		"disk_storage":   store,
		"disk_size_gb":   diskGB,
		"network_bridge": bridge,
		"unprivileged":   unprivileged,
	})
	return supabase.Command{
		ID:        id,
		HostID:    "host-abc",
		Kind:      string(proxmox.ActionLXCCreate),
		Payload:   payload,
		Status:    "pending",
		ExpiresAt: time.Now().Add(30 * time.Second),
	}
}

func TestHandle_LXCCreate_HappyPath_PasswordPropagated(t *testing.T) {
	var captured proxmox.CreateLXCSpec
	actor := &fakeActor{
		createLXC: func(ctx context.Context, spec proxmox.CreateLXCSpec) (string, error) {
			captured = spec
			return "UPID:lxcnew", nil
		},
		await: func(ctx context.Context, node, upid string, timeout time.Duration) (proxmox.TaskStatus, error) {
			return proxmox.TaskStatus{UPID: upid, Done: true, ExitStatus: "OK"}, nil
		},
	}
	store := &fakeStore{claimRet: func(int64) (bool, error) { return true, nil }}
	d := New(actor, store)

	cmd := newLXCCreateCmd(31, "n1", 201, "alpha-ct", "local:vztmpl/debian.tar.zst", "s3cret!", 1, 512, "local-lvm", 8, "vmbr0", true)
	if err := d.Handle(context.Background(), cmd); err != nil {
		t.Fatalf("err: %v", err)
	}
	want := proxmox.CreateLXCSpec{
		Node: "n1", VMID: 201, Hostname: "alpha-ct",
		OSTemplate: "local:vztmpl/debian.tar.zst", Password: "s3cret!",
		Cores: 1, MemoryMB: 512, DiskStorage: "local-lvm", DiskSizeGB: 8,
		NetworkBridge: "vmbr0", Unprivileged: true,
	}
	if captured != want {
		t.Errorf("spec mismatch: got %+v, want %+v", captured, want)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.completed[31].status != "done" {
		t.Errorf("status=%q", store.completed[31].status)
	}
}

func newGuestDeleteCmd(id int64, guestKind, node string, vmid int, destroyDisks, purgeBackups bool) supabase.Command {
	payload, _ := json.Marshal(map[string]any{
		"guest_kind":     guestKind,
		"node":           node,
		"vmid":           vmid,
		"destroy_disks":  destroyDisks,
		"purge_backups":  purgeBackups,
	})
	return supabase.Command{
		ID:        id,
		HostID:    "host-abc",
		Kind:      string(proxmox.ActionGuestDelete),
		Payload:   payload,
		Status:    "pending",
		ExpiresAt: time.Now().Add(30 * time.Second),
	}
}

func TestHandle_GuestDelete_HappyPath_FlagsPropagated(t *testing.T) {
	var captured struct {
		kind                       proxmox.GuestKind
		node                       string
		vmid                       int
		destroyDisks, purgeBackups bool
	}
	actor := &fakeActor{
		deleteGuest: func(ctx context.Context, kind proxmox.GuestKind, node string, vmid int, destroyDisks, purgeBackups bool) (string, error) {
			captured.kind = kind
			captured.node = node
			captured.vmid = vmid
			captured.destroyDisks = destroyDisks
			captured.purgeBackups = purgeBackups
			return "UPID:del", nil
		},
		await: func(ctx context.Context, node, upid string, timeout time.Duration) (proxmox.TaskStatus, error) {
			return proxmox.TaskStatus{UPID: upid, Done: true, ExitStatus: "OK"}, nil
		},
	}
	store := &fakeStore{claimRet: func(int64) (bool, error) { return true, nil }}
	d := New(actor, store)

	cmd := newGuestDeleteCmd(40, "lxc", "n1", 300, true, false)
	if err := d.Handle(context.Background(), cmd); err != nil {
		t.Fatalf("err: %v", err)
	}
	if captured.kind != proxmox.GuestKindLXC || captured.node != "n1" || captured.vmid != 300 ||
		!captured.destroyDisks || captured.purgeBackups {
		t.Errorf("unexpected: %+v", captured)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.completed[40].status != "done" {
		t.Errorf("status=%q", store.completed[40].status)
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
