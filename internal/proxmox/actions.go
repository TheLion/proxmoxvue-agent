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

// IsPowerAction rapporteert of de action een guest power-state-actie is
// die via /status/{action} loopt.
func (a Action) IsPowerAction() bool {
	switch a {
	case ActionStart, ActionStop, ActionReboot, ActionShutdown, ActionSuspend, ActionResume:
		return true
	}
	return false
}

// IsSnapshotAction rapporteert of de action via een /snapshot-endpoint loopt.
func (a Action) IsSnapshotAction() bool {
	switch a {
	case ActionSnapshotCreate, ActionSnapshotDelete, ActionSnapshotRollback:
		return true
	}
	return false
}

// IsCreateAction rapporteert of de action een nieuwe guest aanmaakt
// (qemu/lxc) via een POST op de node-level guest-collection.
func (a Action) IsCreateAction() bool {
	switch a {
	case ActionVMCreate, ActionLXCCreate:
		return true
	}
	return false
}

// IsKnown is true voor elke action die de dispatcher kan routeren.
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

// PerformAction POST't /api2/json/nodes/{node}/{kind}/{vmid}/status/{action}
// en retourneert de door Proxmox toegewezen UPID.
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

// CreateSnapshot POST /api2/json/nodes/{node}/{kind}/{vmid}/snapshot. Form-
// encoded body met snapname + optionele description + vmstate=1. Returnt de
// UPID; caller wacht via AwaitTaskCompletion (memory-snapshots ≥120s).
// `includeVmState` is alleen geldig voor QEMU + running.
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
// Returnt de UPID. Proxmox URL-encodet snapname zelf niet — caller heeft hier
// al een gevalideerde naam (SnapshotNamePattern), dus geen URL-injectie-risk.
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
// Returnt de UPID. Proxmox verwijdert tijdens rollback alle snapshots ná deze
// in de chain — caller (UI) communiceert dat aan de gebruiker.
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

// CreateVMSpec bevat de minimale velden voor een QEMU VM-create. Subset van
// Proxmox' /api2/json/nodes/{node}/qemu params; uitgebreid wanneer iOS meer
// opties exposeert. ostype=l26 wordt server-side geïnjecteerd zodat iOS dat
// niet mee hoeft te sturen.
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

// CreateVM POST /api2/json/nodes/{node}/qemu. Returnt de UPID.
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

// CreateLXCSpec bevat de minimale velden voor een LXC-container-create. Het
// password-veld bevat plaintext credentials — caller moet ervoor zorgen dat
// dit veld niet wordt gelogd. Future: zie row 1476 (E2E-encryptie via cluster-key).
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

// CreateLXC POST /api2/json/nodes/{node}/lxc. Returnt de UPID.
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

// DeleteGuest DELETE /api2/json/nodes/{node}/{kind}/{vmid}. Met
// `destroyDisks` worden niet-gelinkte disks mee verwijderd
// (`destroy-unreferenced-disks=1`); met `purgeBackups` ook alle
// back-ups (`purge=1`). Proxmox eist dat de guest gestopt is — anders
// krijgt de caller een 400 doorgegeven.
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

// SnapshotNamePattern is dezelfde validatie die Proxmox toepast op snapname
// (`[a-z][a-z0-9_-]+/i` in PVE::JSONSchema; min 2 chars totaal). Lengte
// gemaximeerd op 40 als pragmatische cap — Proxmox stelt zelf geen max.
// Mismatch = 400 van Proxmox; vóór de roundtrip checken geeft een
// duidelijker fout in iOS.
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

// AwaitTaskCompletion polt tot de task klaar is of de timeout verstrijkt.
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
