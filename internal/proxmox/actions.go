package proxmox

import (
	"context"
	"fmt"
	"net/url"
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
)

// IsKnown rapporteert of de action een ondersteunde power-action is.
func (a Action) IsKnown() bool {
	switch a {
	case ActionStart, ActionStop, ActionReboot, ActionShutdown, ActionSuspend, ActionResume:
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
