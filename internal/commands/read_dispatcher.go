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

// ProxmoxReader is de subset van proxmox.Client die de read-dispatcher gebruikt.
type ProxmoxReader interface {
	GetRaw(ctx context.Context, path string) (json.RawMessage, error)
}

// ReadCommandStore is de subset van supabase.Client die de read-dispatcher
// gebruikt.
type ReadCommandStore interface {
	ClaimReadCommand(ctx context.Context, id int64) (bool, error)
	CompleteReadCommand(ctx context.Context, id int64, status string, result json.RawMessage, errMsg string) error
}

// readEndpointWhitelist beperkt welke Proxmox-paden iOS via de read-RPC mag
// aanroepen. Defense in depth — de agent draait met een full-permission
// API-token, dus zonder whitelist kan een gemanipuleerde row de agent forceren
// om bv. /access/users uit te lezen of password-reset te triggeren.
//
// Patterns matchen alleen wat iOS' detail-views nu daadwerkelijk via direct
// gebruiken; uitbreiden mag, mits expliciet en read-only.
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

	// Timeout begrenst hoe lang één Proxmox-call mag duren. Reads zijn typisch
	// <500ms; 10s is ruim genoeg voor trage clusters maar laat de iOS-poller
	// niet hangen.
	Timeout time.Duration
}

func NewReadDispatcher(pve ProxmoxReader, store ReadCommandStore) *ReadDispatcher {
	return &ReadDispatcher{
		pve:     pve,
		store:   store,
		Timeout: 10 * time.Second,
	}
}

// Handle verwerkt één read_command: TTL-check → claim → endpoint-whitelist →
// Proxmox GET → complete. Proxmox-fouten leiden tot een completed row met
// status=failed. De enige error die naar boven bubbelt is wanneer claim of
// complete zelf faalt.
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

// buildPath voegt query-params (uit de jsonb-kolom) toe aan endpoint.
// Params is een platte object {key: stringOrNumber}; geneste structuren
// worden afgewezen — Proxmox GETs gebruiken alleen flat query-strings.
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
			// json.Number zou netter zijn maar map[string]any geeft float64.
			// Voor de Proxmox-paden in onze whitelist (limit=N, content=type)
			// is dit voldoende.
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
