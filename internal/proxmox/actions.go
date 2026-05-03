package proxmox

import (
	"context"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"
)

type GuestKind string

const (
	GuestKindQEMU GuestKind = "qemu"
	GuestKindLXC  GuestKind = "lxc"
)

type Action string

const (
	ActionStart    Action = "start"
	ActionStop     Action = "stop"
	ActionReboot   Action = "reboot"
	ActionShutdown Action = "shutdown"
	ActionSuspend  Action = "suspend"
	ActionResume   Action = "resume"

	ActionSnapshotCreate   Action = "snapshot.create"
	ActionSnapshotDelete   Action = "snapshot.delete"
	ActionSnapshotRollback Action = "snapshot.rollback"

	ActionVMCreate  Action = "vm.create"
	ActionLXCCreate Action = "lxc.create"

	ActionGuestDelete Action = "guest.delete"
)

// IsPowerAction reports whether the action is a guest power-state
// action routed through /status/{action}.
func (a Action) IsPowerAction() bool {
	switch a {
	case ActionStart, ActionStop, ActionReboot, ActionShutdown, ActionSuspend, ActionResume:
		return true
	}
	return false
}

// IsSnapshotAction reports whether the action goes through a /snapshot endpoint.
func (a Action) IsSnapshotAction() bool {
	switch a {
	case ActionSnapshotCreate, ActionSnapshotDelete, ActionSnapshotRollback:
		return true
	}
	return false
}

// IsCreateAction reports whether the action creates a new guest
// (qemu/lxc) via a POST against the node-level guest collection.
func (a Action) IsCreateAction() bool {
	switch a {
	case ActionVMCreate, ActionLXCCreate:
		return true
	}
	return false
}

// IsKnown is true for every action the dispatcher can route.
func (a Action) IsKnown() bool {
	if a.IsPowerAction() || a.IsSnapshotAction() || a.IsCreateAction() {
		return true
	}
	switch a {
	case ActionGuestDelete:
		return true
	}
	return false
}

// PerformAction POSTs /api2/json/nodes/{node}/{kind}/{vmid}/status/{action}
// and returns the UPID assigned by Proxmox.
func (c *Client) PerformAction(ctx context.Context, kind GuestKind, node string, vmid int, action Action) (string, error) {
	path := fmt.Sprintf("/api2/json/nodes/%s/%s/%d/status/%s", node, kind, vmid, action)
	var wrapper struct {
		Data string `json:"data"`
	}
	if err := c.postJSON(ctx, path, nil, &wrapper); err != nil {
		return "", fmt.Errorf("proxmox %s %s/%d: %w", action, kind, vmid, err)
	}
	return wrapper.Data, nil
}

// CreateSnapshot POST /api2/json/nodes/{node}/{kind}/{vmid}/snapshot.
// Form-encoded body with snapname + optional description + vmstate=1.
// Returns the UPID; the caller waits via AwaitTaskCompletion
// (memory snapshots ≥120s). `includeVmState` is only valid for QEMU + running.
func (c *Client) CreateSnapshot(ctx context.Context, kind GuestKind, node string, vmid int, name, description string, includeVmState bool) (string, error) {
	path := fmt.Sprintf("/api2/json/nodes/%s/%s/%d/snapshot", node, kind, vmid)
	form := url.Values{"snapname": {name}}
	if strings.TrimSpace(description) != "" {
		form.Set("description", description)
	}
	if includeVmState {
		form.Set("vmstate", "1")
	}
	var wrapper struct {
		Data string `json:"data"`
	}
	if err := c.postForm(ctx, path, form, &wrapper); err != nil {
		return "", fmt.Errorf("proxmox snapshot.create %s/%d %s: %w", kind, vmid, name, err)
	}
	return wrapper.Data, nil
}

// DeleteSnapshot DELETE /api2/json/nodes/{node}/{kind}/{vmid}/snapshot/{name}.
// Returns the UPID. Proxmox does not URL-encode snapname itself — by
// the time we get here the caller has already validated the name
// (SnapshotNamePattern), so there is no URL-injection risk.
func (c *Client) DeleteSnapshot(ctx context.Context, kind GuestKind, node string, vmid int, name string) (string, error) {
	path := fmt.Sprintf("/api2/json/nodes/%s/%s/%d/snapshot/%s", node, kind, vmid, url.PathEscape(name))
	var wrapper struct {
		Data string `json:"data"`
	}
	if err := c.deleteJSON(ctx, path, &wrapper); err != nil {
		return "", fmt.Errorf("proxmox snapshot.delete %s/%d %s: %w", kind, vmid, name, err)
	}
	return wrapper.Data, nil
}

// RollbackSnapshot POST /api2/json/nodes/{node}/{kind}/{vmid}/snapshot/{name}/rollback.
// Returns the UPID. During rollback Proxmox removes every snapshot
// taken after this one in the chain — the caller (UI) communicates
// that to the user.
func (c *Client) RollbackSnapshot(ctx context.Context, kind GuestKind, node string, vmid int, name string) (string, error) {
	path := fmt.Sprintf("/api2/json/nodes/%s/%s/%d/snapshot/%s/rollback", node, kind, vmid, url.PathEscape(name))
	var wrapper struct {
		Data string `json:"data"`
	}
	if err := c.postJSON(ctx, path, nil, &wrapper); err != nil {
		return "", fmt.Errorf("proxmox snapshot.rollback %s/%d %s: %w", kind, vmid, name, err)
	}
	return wrapper.Data, nil
}

// CreateVMSpec carries the minimum fields for a QEMU VM create.
// Subset of Proxmox's /api2/json/nodes/{node}/qemu params; expand when
// iOS exposes more options. ostype=l26 is injected server-side so iOS
// doesn't need to send it.
type CreateVMSpec struct {
	Node          string
	VMID          int
	Name          string
	Cores         int
	MemoryMB      int
	DiskStorage   string
	DiskSizeGB    int
	NetworkBridge string
}

// CreateVM POST /api2/json/nodes/{node}/qemu. Returns the UPID.
func (c *Client) CreateVM(ctx context.Context, spec CreateVMSpec) (string, error) {
	path := fmt.Sprintf("/api2/json/nodes/%s/qemu", spec.Node)
	form := url.Values{
		"vmid":   {fmt.Sprintf("%d", spec.VMID)},
		"name":   {spec.Name},
		"cores":  {fmt.Sprintf("%d", spec.Cores)},
		"memory": {fmt.Sprintf("%d", spec.MemoryMB)},
		"scsi0":  {fmt.Sprintf("%s:%d", spec.DiskStorage, spec.DiskSizeGB)},
		"net0":   {fmt.Sprintf("virtio,bridge=%s", spec.NetworkBridge)},
		"ostype": {"l26"},
	}
	var wrapper struct {
		Data string `json:"data"`
	}
	if err := c.postForm(ctx, path, form, &wrapper); err != nil {
		return "", fmt.Errorf("proxmox vm.create node=%s vmid=%d: %w", spec.Node, spec.VMID, err)
	}
	return wrapper.Data, nil
}

// CreateLXCSpec carries the minimum fields for an LXC container
// create. The Password field holds plaintext credentials at the
// moment CreateLXC is called — the upstream dispatcher has already
// HPKE-decrypted it (#1476). Do not log.
type CreateLXCSpec struct {
	Node          string
	VMID          int
	Hostname      string
	OSTemplate    string
	Password      string
	Cores         int
	MemoryMB      int
	DiskStorage   string
	DiskSizeGB    int
	NetworkBridge string
	Unprivileged  bool
}

// CreateLXC POST /api2/json/nodes/{node}/lxc. Returns the UPID.
func (c *Client) CreateLXC(ctx context.Context, spec CreateLXCSpec) (string, error) {
	path := fmt.Sprintf("/api2/json/nodes/%s/lxc", spec.Node)
	form := url.Values{
		"vmid":         {fmt.Sprintf("%d", spec.VMID)},
		"hostname":     {spec.Hostname},
		"ostemplate":   {spec.OSTemplate},
		"password":     {spec.Password},
		"cores":        {fmt.Sprintf("%d", spec.Cores)},
		"memory":       {fmt.Sprintf("%d", spec.MemoryMB)},
		"rootfs":       {fmt.Sprintf("%s:%d", spec.DiskStorage, spec.DiskSizeGB)},
		"net0":         {fmt.Sprintf("name=eth0,bridge=%s,ip=dhcp", spec.NetworkBridge)},
		"unprivileged": {boolToFlag(spec.Unprivileged)},
		"start":        {"0"},
	}
	var wrapper struct {
		Data string `json:"data"`
	}
	if err := c.postForm(ctx, path, form, &wrapper); err != nil {
		return "", fmt.Errorf("proxmox lxc.create node=%s vmid=%d: %w", spec.Node, spec.VMID, err)
	}
	return wrapper.Data, nil
}

// DeleteGuest DELETE /api2/json/nodes/{node}/{kind}/{vmid}. With
// `destroyDisks`, unreferenced disks are removed too
// (`destroy-unreferenced-disks=1`); with `purgeBackups` all backups
// are wiped (`purge=1`). Proxmox requires the guest to be stopped —
// otherwise the caller gets a 400 propagated.
func (c *Client) DeleteGuest(ctx context.Context, kind GuestKind, node string, vmid int, destroyDisks, purgeBackups bool) (string, error) {
	q := url.Values{}
	if destroyDisks {
		q.Set("destroy-unreferenced-disks", "1")
	}
	if purgeBackups {
		q.Set("purge", "1")
	}
	path := fmt.Sprintf("/api2/json/nodes/%s/%s/%d", node, kind, vmid)
	if encoded := q.Encode(); encoded != "" {
		path += "?" + encoded
	}
	var wrapper struct {
		Data string `json:"data"`
	}
	if err := c.deleteJSON(ctx, path, &wrapper); err != nil {
		return "", fmt.Errorf("proxmox guest.delete %s/%d: %w", kind, vmid, err)
	}
	return wrapper.Data, nil
}

func boolToFlag(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

// SnapshotNamePattern is the same validation Proxmox applies to
// snapname (`[a-z][a-z0-9_-]+/i` in PVE::JSONSchema; min 2 chars total).
// Length capped at 40 as a pragmatic cap — Proxmox itself sets no
// max. Mismatch = 400 from Proxmox; checking before the roundtrip
// gives a clearer error in iOS.
var SnapshotNamePattern = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_-]{1,39}$`)

type TaskStatus struct {
	UPID       string
	Done       bool
	ExitStatus string
}

// TaskStatus GET /api2/json/nodes/{node}/tasks/{upid}/status.
func (c *Client) TaskStatus(ctx context.Context, node, upid string) (TaskStatus, error) {
	encoded := url.PathEscape(upid)
	var wrapper struct {
		Data struct {
			UPID       string `json:"upid"`
			Status     string `json:"status"`
			ExitStatus string `json:"exitstatus"`
		} `json:"data"`
	}
	if err := c.getJSON(ctx, "/api2/json/nodes/"+node+"/tasks/"+encoded+"/status", &wrapper); err != nil {
		return TaskStatus{}, fmt.Errorf("task status: %w", err)
	}
	return TaskStatus{
		UPID:       wrapper.Data.UPID,
		Done:       wrapper.Data.Status == "stopped",
		ExitStatus: wrapper.Data.ExitStatus,
	}, nil
}

// AwaitTaskCompletion polls until the task is done or the timeout elapses.
func (c *Client) AwaitTaskCompletion(ctx context.Context, node, upid string, timeout time.Duration) (TaskStatus, error) {
	const pollInterval = 500 * time.Millisecond
	deadline := time.Now().Add(timeout)
	for {
		st, err := c.TaskStatus(ctx, node, upid)
		if err != nil {
			return st, err
		}
		if st.Done {
			return st, nil
		}
		if time.Now().After(deadline) {
			return st, fmt.Errorf("await task %s: timeout after %s", upid, timeout)
		}
		select {
		case <-ctx.Done():
			return st, ctx.Err()
		case <-time.After(pollInterval):
		}
	}
}
