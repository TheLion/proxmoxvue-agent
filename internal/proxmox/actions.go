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

	ActionSnapshotCreate Action = "snapshot.create"
	ActionSnapshotDelete Action = "snapshot.delete"
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
	case ActionSnapshotCreate, ActionSnapshotDelete:
		return true
	}
	return false
}

// IsKnown is true voor elke action die de dispatcher kan routeren.
func (a Action) IsKnown() bool {
	return a.IsPowerAction() || a.IsSnapshotAction()
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

// SnapshotNamePattern is dezelfde validatie die Proxmox toepast op snapname.
// Eerste teken letter, daarna [a-zA-Z0-9_-]{0,39}. Mismatch = 400 van Proxmox;
// vóór de roundtrip checken geeft een duidelijker fout in iOS.
var SnapshotNamePattern = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_-]{0,39}$`)

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
