package commands

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/TheLion/proxmoxvue-agent/internal/supabase"
)

// ProxmoxReader is the subset of proxmox.Client the read-dispatcher uses.
type ProxmoxReader interface {
	GetRaw(ctx context.Context, path string) (json.RawMessage, error)
}

// ReadCommandStore is the subset of supabase.Client the read-dispatcher uses.
type ReadCommandStore interface {
	ClaimReadCommand(ctx context.Context, id int64) (bool, error)
	CompleteReadCommand(ctx context.Context, id int64, status string, result json.RawMessage, errMsg string) error
}

// readEndpointWhitelist limits which Proxmox paths iOS may call via
// the read-RPC. Defense in depth — the agent runs with a
// full-permission API token, so without a whitelist a tampered row
// could force the agent to read `/access/users` or trigger a password
// reset.
//
// The patterns only match what iOS' detail views currently use over
// the direct path; expanding is fine, provided it stays explicit and
// read-only.
var readEndpointWhitelist = []*regexp.Regexp{
	regexp.MustCompile(`^/api2/json/cluster/resources$`),
	regexp.MustCompile(`^/api2/json/cluster/status$`),
	regexp.MustCompile(`^/api2/json/cluster/tasks$`),
	regexp.MustCompile(`^/api2/json/nodes/[^/]+/status$`),
	regexp.MustCompile(`^/api2/json/nodes/[^/]+/network$`),
	regexp.MustCompile(`^/api2/json/nodes/[^/]+/firewall/rules$`),
	regexp.MustCompile(`^/api2/json/nodes/[^/]+/tasks$`),
	regexp.MustCompile(`^/api2/json/nodes/[^/]+/tasks/[^/]+/status$`),
	regexp.MustCompile(`^/api2/json/nodes/[^/]+/tasks/[^/]+/log$`),
	regexp.MustCompile(`^/api2/json/nodes/[^/]+/(qemu|lxc)/\d+/config$`),
	regexp.MustCompile(`^/api2/json/nodes/[^/]+/(qemu|lxc)/\d+/snapshot$`),
	regexp.MustCompile(`^/api2/json/nodes/[^/]+/(qemu|lxc)/\d+/feature$`),
	regexp.MustCompile(`^/api2/json/nodes/[^/]+/storage/[^/]+/content$`),
}

func endpointAllowed(path string) bool {
	for _, re := range readEndpointWhitelist {
		if re.MatchString(path) {
			return true
		}
	}
	return false
}

type ReadDispatcher struct {
	pve   ProxmoxReader
	store ReadCommandStore

	// Timeout caps how long a single Proxmox call may take. Reads are
	// typically <500ms; 10s is generous enough for slow clusters
	// without leaving the iOS poller hanging.
	Timeout time.Duration
}

func NewReadDispatcher(pve ProxmoxReader, store ReadCommandStore) *ReadDispatcher {
	return &ReadDispatcher{
		pve:     pve,
		store:   store,
		Timeout: 10 * time.Second,
	}
}

// Handle processes a single read_command: TTL check → claim →
// endpoint whitelist → Proxmox GET → complete. Proxmox errors result
// in a completed row with status=failed. The only error that bubbles
// up is when claim or complete itself fails.
func (d *ReadDispatcher) Handle(ctx context.Context, cmd supabase.ReadCommand) error {
	if !cmd.ExpiresAt.IsZero() && time.Now().After(cmd.ExpiresAt) {
		if ok, err := d.store.ClaimReadCommand(ctx, cmd.ID); err == nil && ok {
			slog.Info("read_command expired", "id", cmd.ID, "endpoint", cmd.Endpoint)
			return d.store.CompleteReadCommand(ctx, cmd.ID, "expired", nil, "ttl")
		}
		return nil
	}

	ok, err := d.store.ClaimReadCommand(ctx, cmd.ID)
	if err != nil {
		return fmt.Errorf("claim read_command %d: %w", cmd.ID, err)
	}
	if !ok {
		return nil
	}
	slog.Info("read_command claimed", "id", cmd.ID, "endpoint", cmd.Endpoint)

	if !endpointAllowed(cmd.Endpoint) {
		return d.store.CompleteReadCommand(ctx, cmd.ID, "failed", nil, "endpoint not allowed: "+cmd.Endpoint)
	}

	path, err := buildPath(cmd.Endpoint, cmd.Params)
	if err != nil {
		return d.store.CompleteReadCommand(ctx, cmd.ID, "failed", nil, "bad params: "+err.Error())
	}

	callCtx, cancel := context.WithTimeout(ctx, d.Timeout)
	defer cancel()

	body, err := d.pve.GetRaw(callCtx, path)
	if err != nil {
		return d.store.CompleteReadCommand(ctx, cmd.ID, "failed", nil, err.Error())
	}

	if completeErr := d.store.CompleteReadCommand(ctx, cmd.ID, "done", body, ""); completeErr != nil {
		slog.Error("complete read_command failed", "id", cmd.ID, "err", completeErr)
		return completeErr
	}
	slog.Info("read_command done", "id", cmd.ID, "bytes", len(body))
	return nil
}

// buildPath appends query params (from the jsonb column) to endpoint.
// Params is a flat object {key: stringOrNumber}; nested structures
// are rejected — Proxmox GETs only use flat query strings.
func buildPath(endpoint string, params json.RawMessage) (string, error) {
	if len(params) == 0 || string(params) == "{}" || string(params) == "null" {
		return endpoint, nil
	}
	var raw map[string]any
	if err := json.Unmarshal(params, &raw); err != nil {
		return "", fmt.Errorf("params not an object: %w", err)
	}
	if len(raw) == 0 {
		return endpoint, nil
	}
	q := url.Values{}
	for k, v := range raw {
		switch x := v.(type) {
		case string:
			q.Set(k, x)
		case float64:
			// json.Number would be cleaner but map[string]any returns
			// float64. For the Proxmox paths in our whitelist
			// (limit=N, content=type) this is sufficient.
			q.Set(k, strings.TrimRight(strings.TrimRight(fmt.Sprintf("%f", x), "0"), "."))
		case bool:
			if x {
				q.Set(k, "1")
			} else {
				q.Set(k, "0")
			}
		default:
			return "", fmt.Errorf("unsupported param type for %q", k)
		}
	}
	return endpoint + "?" + q.Encode(), nil
}
