# Commands Subscribe + Dispatch — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** De Go-agent abonneert op Supabase Realtime voor de `commands`-tabel (gefilterd op zijn eigen `host_id`), voert power-acties (start/stop/reboot/shutdown/suspend/resume) uit tegen de lokale Proxmox REST API, en schrijft het resultaat terug naar dezelfde rij — conform decisions #196 (idempotent + 30s TTL) en #198 (RLS via JWT `host_id` claim).

**Architecture:** Twee onafhankelijke event-loops naast elkaar in `runtime.Start`: (1) de bestaande 30s status-push-ticker voor metrics, (2) een nieuwe Realtime-WS-subscriber voor commands. De subscriber pushed `CommandEvent`s door een channel naar een dispatcher, die per event claimt via PATCH (atomic `status=pending → claimed`), Proxmox aanroept, op UPID polt tot klaar, en met resultaat PATCH't (status `done`/`failed`). Bij WS-disconnect vangt een catch-up-scan eventuele gemiste pending commands op. Alle nieuwe code is TDD'd met `httptest.Server` en een in-proces WS-mock.

**Tech Stack:** Go 1.22+, `github.com/coder/websocket` voor WS, standaard `net/http` + `httptest` voor tests, Supabase Realtime v2 (Phoenix-channels protocol), Proxmox VE REST API (`/api2/json`).

---

## Scope

**In scope (iteratie 1):**
- Realtime WS subscribe op `public.commands` (filter `host_id=eq.<mine>`, event INSERT)
- Power-acties: `start`, `stop`, `reboot`, `shutdown`, `suspend`, `resume` (zowel QEMU als LXC) — uniform 30s action-timeout (matcht iOS default)
- Idempotent claim (`status=pending → claimed`, conditional op huidige status)
- Resultaat write-back inclusief UPID + Proxmox exitstatus
- TTL-respect: commands met `expires_at < now()` worden overgeslagen (met write-back status=`expired` indien nog claimable)
- WS reconnect met exp-backoff — **geen catch-up-scan**

**Geen catch-up (bewuste keuze):** de iOS-app gate't het enqueuen van commands op de agent-WS-aanwezigheid via Supabase Realtime **presence** — niet via `last_seen_at`-polling. Presence detecteert het WS-down-REST-up scenario direct (<15s) en exact, dus een reconnect-scan in de agent is overbodig. De 30s TTL is het vangnet voor de zeldzame race. Zie backlog #1365 voor de iOS-kant.

**Out of scope (apart ingepland):**
- Presence-channel voor cadence-switch — #1348
- iOS enqueue-pad naar de `commands`-tabel — nog apart item op te voeren
- iOS: command-enqueue blokkeren wanneer agent offline — #1365
- Snapshot-acties (create/delete/rollback) via commands — #1361
- VM/LXC create via commands — #1362
- VM/LXC delete via commands — #1363
- Guest config-edit via commands — #1364

## Command contract

Omdat er nog geen schrijvend pad is, leggen we het payload-contract hier vast zodat agent en toekomstige iOS-enqueue synchroon blijven:

| Veld | Type | Waarden |
|---|---|---|
| `commands.kind` | text | `"start"` \| `"stop"` \| `"reboot"` \| `"shutdown"` \| `"suspend"` \| `"resume"` |
| `commands.payload.guest_kind` | text | `"qemu"` \| `"lxc"` |
| `commands.payload.node` | text | Proxmox-nodenaam (bv. `"proxmox01"`) |
| `commands.payload.vmid` | int | Numeric VMID |

De agent faalt een command (`status=failed`, `result={"error": "..."}`) bij onbekende `kind` of ontbrekende payload-velden — NOOIT guessen.

## File Structure

```
internal/
├── proxmox/
│   ├── client.go          (bestaand)
│   ├── actions.go         (NEW — PerformAction, TaskStatus, AwaitTaskCompletion)
│   └── actions_test.go    (NEW)
├── supabase/
│   ├── client.go          (bestaand — auth/refresh)
│   ├── rest.go            (bestaand — PushSnapshot)
│   ├── commands.go        (NEW — ClaimCommand, CompleteCommand, ScanPending)
│   ├── commands_test.go   (NEW)
│   ├── realtime.go        (NEW — Phoenix-WS subscriber)
│   └── realtime_test.go   (NEW — met ws-mock)
├── commands/
│   ├── dispatcher.go      (NEW — glue: claim → dispatch → complete)
│   └── dispatcher_test.go (NEW)
└── runtime/
    └── runtime.go         (MODIFY — extra goroutine)
```

Boundaries: `supabase` weet niets van Proxmox, `proxmox` weet niets van Supabase, `commands/dispatcher` is de enige plek waar ze samenkomen. Dat houdt de tests simpel (elk package afzonderlijk testbaar met fakes).

---

## Tasks

### Task 1: Proxmox action endpoints + UPID polling

**Files:**
- Create: `internal/proxmox/actions.go`
- Create: `internal/proxmox/actions_test.go`
- Modify: `internal/proxmox/client.go` (helper `postJSON` toevoegen)

**Rationale:** De agent moet dezelfde action-endpoints kunnen aanroepen als de iOS-app (`/nodes/{node}/{kind}/{vmid}/status/{action}`) en op de UPID kunnen polen tot de task klaar is. Dit is puur Proxmox-client-uitbreiding, onafhankelijk van Supabase.

- [ ] **Step 1.1: Schrijf failing test voor `PerformAction`**

Create `internal/proxmox/actions_test.go`:

```go
package proxmox

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPerformAction_BuildsCorrectPath(t *testing.T) {
	var gotPath string
	var gotMethod string
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotMethod = r.Method
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":"UPID:proxmox01:00001234:0000ABCD:66000000:qmstart:112:root@pam!claude:"}`))
	}))
	defer srv.Close()

	c := New(Config{APIURL: srv.URL, APITokenID: "root@pam!claude", APITokenSecret: "secret", VerifyTLS: false})
	upid, err := c.PerformAction(context.Background(), GuestKindQEMU, "proxmox01", 112, ActionStart)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !strings.HasPrefix(upid, "UPID:proxmox01:") {
		t.Errorf("unexpected UPID: %q", upid)
	}
	if gotPath != "/api2/json/nodes/proxmox01/qemu/112/status/start" {
		t.Errorf("path = %q", gotPath)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method = %q", gotMethod)
	}
	if gotAuth != "PVEAPIToken=root@pam!claude=secret" {
		t.Errorf("auth = %q", gotAuth)
	}
}
```

- [ ] **Step 1.2: Run test → FAIL**

```bash
cd ~/Documents/Apps/proxmoxvue-agent
go test ./internal/proxmox/ -run TestPerformAction_BuildsCorrectPath -v
```
Expected: FAIL (`PerformAction`, `GuestKindQEMU`, `ActionStart` undefined).

- [ ] **Step 1.3: Implementeer actions.go (minimaal voor test 1)**

Create `internal/proxmox/actions.go`:

```go
package proxmox

import (
	"context"
	"encoding/json"
	"fmt"
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
// Wordt door de dispatcher gebruikt om onbekende kinds te weigeren.
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
```

- [ ] **Step 1.4: Voeg `postJSON` helper toe aan client.go**

In `internal/proxmox/client.go`, onder `getJSON`:

```go
func (c *Client) postJSON(ctx context.Context, path string, body []byte, out any) error {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.APIURL+path, reader)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", c.authHeader)
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("do request: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("status %d: %s", resp.StatusCode, truncate(string(raw), 200))
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("parse response: %w", err)
		}
	}
	return nil
}
```

Voeg bovenaan `import "bytes"` toe indien die nog niet aanwezig is.

- [ ] **Step 1.5: Run test → PASS**

```bash
go test ./internal/proxmox/ -run TestPerformAction_BuildsCorrectPath -v
```
Expected: PASS.

- [ ] **Step 1.6: Schrijf failing test voor `TaskStatus`**

In `internal/proxmox/actions_test.go` toevoegen:

```go
func TestTaskStatus_ParsesRunningAndOK(t *testing.T) {
	cases := []struct {
		name       string
		body       string
		wantDone   bool
		wantExit   string
	}{
		{"running", `{"data":{"status":"running","upid":"UPID:n:1234:ABCD:66:qmstart:112:u:"}}`, false, ""},
		{"done-ok", `{"data":{"status":"stopped","exitstatus":"OK","upid":"UPID:n:1234:ABCD:66:qmstart:112:u:"}}`, true, "OK"},
		{"done-fail", `{"data":{"status":"stopped","exitstatus":"command failed","upid":"UPID:n:1234:ABCD:66:qmstart:112:u:"}}`, true, "command failed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			c := New(Config{APIURL: srv.URL, APITokenID: "x", APITokenSecret: "y"})
			st, err := c.TaskStatus(context.Background(), "proxmox01", "UPID:n:1234:ABCD:66:qmstart:112:u:")
			if err != nil {
				t.Fatalf("err: %v", err)
			}
			if st.Done != tc.wantDone {
				t.Errorf("Done=%v want %v", st.Done, tc.wantDone)
			}
			if st.ExitStatus != tc.wantExit {
				t.Errorf("ExitStatus=%q want %q", st.ExitStatus, tc.wantExit)
			}
		})
	}
}
```

- [ ] **Step 1.7: Run → FAIL, dan implementeer**

Append to `internal/proxmox/actions.go`:

```go
import "net/url"

type TaskStatus struct {
	UPID       string
	Status     string // "running" | "stopped"
	ExitStatus string // alleen gevuld als Status == "stopped"
}

func (t TaskStatus) Done() bool  { return t.Status == "stopped" }
func (t TaskStatus) OK() bool    { return t.ExitStatus == "OK" }

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
	return TaskStatus{UPID: wrapper.Data.UPID, Status: wrapper.Data.Status, ExitStatus: wrapper.Data.ExitStatus}, nil
}
```

Let op: de struct heeft `Done` als field (bool in test) én `Done()` method. Kies één — we houden **field**. Herschrijf:

```go
type TaskStatus struct {
	UPID       string
	Done       bool
	ExitStatus string
}

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
```

Run tests → PASS.

- [ ] **Step 1.8: Schrijf failing test voor `AwaitTaskCompletion`**

```go
func TestAwaitTaskCompletion_PollsUntilDone(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		if calls < 3 {
			_, _ = w.Write([]byte(`{"data":{"status":"running","upid":"UPID:x:1:A:66:qmstart:112:u:"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"data":{"status":"stopped","exitstatus":"OK","upid":"UPID:x:1:A:66:qmstart:112:u:"}}`))
	}))
	defer srv.Close()
	c := New(Config{APIURL: srv.URL, APITokenID: "x", APITokenSecret: "y"})
	st, err := c.AwaitTaskCompletion(context.Background(), "proxmox01", "UPID:x:1:A:66:qmstart:112:u:", 5*time.Second)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if st.ExitStatus != "OK" {
		t.Errorf("exit=%q", st.ExitStatus)
	}
	if calls < 3 {
		t.Errorf("expected ≥3 polls, got %d", calls)
	}
}

func TestAwaitTaskCompletion_TimesOut(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"status":"running","upid":"UPID:x:1:A:66:qmstart:112:u:"}}`))
	}))
	defer srv.Close()
	c := New(Config{APIURL: srv.URL, APITokenID: "x", APITokenSecret: "y"})
	_, err := c.AwaitTaskCompletion(context.Background(), "proxmox01", "UPID:x:1:A:66:qmstart:112:u:", 100*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "timeout") {
		t.Errorf("expected timeout err, got %v", err)
	}
}
```

Voeg `import "time"` toe.

- [ ] **Step 1.9: Implementeer AwaitTaskCompletion**

Append to `actions.go`:

```go
// AwaitTaskCompletion polt elke pollInterval tot de task klaar is of
// totdat timeout verstrijkt. Retourneert de laatste TaskStatus.
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
```

Voeg `"time"` toe aan imports van actions.go.

- [ ] **Step 1.10: Run tests → PASS**

```bash
go test ./internal/proxmox/ -v
```

- [ ] **Step 1.11: Commit**

```bash
git add internal/proxmox/actions.go internal/proxmox/actions_test.go internal/proxmox/client.go
git commit -m "feat(proxmox): action endpoints + UPID polling"
```

---

### Task 2: Supabase commands PATCH (claim + complete + scan)

**Files:**
- Create: `internal/supabase/commands.go`
- Create: `internal/supabase/commands_test.go`

**Rationale:** De agent moet commando's atomair claimen (`status=pending → claimed` in één UPDATE met conditie), na afloop resultaat terugschrijven, en bij (re)connect een scan doen van nog-pending rows om niets te missen.

- [ ] **Step 2.1: Schrijf failing test voor ClaimCommand (success + already-claimed)**

Create `internal/supabase/commands_test.go`:

```go
package supabase

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// fakeTokenClient geeft een Client die niet refresht — voor tests die
// alleen de REST-call willen verifiëren.
func fakeTokenClient(t *testing.T, restBase string) *Client {
	t.Helper()
	c := &Client{
		projectRef:  "test",
		httpClient:  &http.Client{Timeout: 5 * time.Second},
		persist:     func(string) error { return nil },
		restBase:    restBase,
		authBase:    "unused",
		accessToken: "fake-jwt",
		expiresAt:   time.Now().Add(time.Hour),
	}
	return c
}

func TestClaimCommand_Success(t *testing.T) {
	var gotMethod, gotQuery, gotPrefer string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotQuery = r.URL.RawQuery
		gotPrefer = r.Header.Get("Prefer")
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"id":42,"status":"claimed"}]`))
	}))
	defer srv.Close()

	c := fakeTokenClient(t, srv.URL)
	claimed, err := c.ClaimCommand(context.Background(), 42)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !claimed {
		t.Errorf("expected claimed=true")
	}
	if gotMethod != http.MethodPatch {
		t.Errorf("method=%q", gotMethod)
	}
	if !strings.Contains(gotQuery, "id=eq.42") || !strings.Contains(gotQuery, "status=eq.pending") {
		t.Errorf("query=%q (want id=eq.42 AND status=eq.pending)", gotQuery)
	}
	if !strings.Contains(gotPrefer, "return=representation") {
		t.Errorf("prefer=%q", gotPrefer)
	}
	if gotBody["status"] != "claimed" {
		t.Errorf("body.status=%v", gotBody["status"])
	}
	if _, ok := gotBody["claimed_at"]; !ok {
		t.Errorf("body missing claimed_at")
	}
}

func TestClaimCommand_AlreadyClaimed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[]`))
	}))
	defer srv.Close()
	c := fakeTokenClient(t, srv.URL)
	claimed, err := c.ClaimCommand(context.Background(), 42)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if claimed {
		t.Errorf("expected claimed=false for already-claimed row")
	}
}
```

- [ ] **Step 2.2: Run → FAIL, dan implementeer commands.go**

Create `internal/supabase/commands.go`:

```go
package supabase

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Command is één rij uit public.commands zoals de agent die relevant heeft.
type Command struct {
	ID        int64           `json:"id"`
	HostID    string          `json:"host_id"`
	Kind      string          `json:"kind"`
	Payload   json.RawMessage `json:"payload"`
	Status    string          `json:"status"`
	ExpiresAt time.Time       `json:"expires_at"`
}

// ClaimCommand probeert de rij met id en status=pending atomair naar
// status=claimed te zetten. Retourneert true als de update een rij raakte
// (wij zijn de claimant), false als de rij al geclaimed/afgerond is.
func (c *Client) ClaimCommand(ctx context.Context, id int64) (bool, error) {
	body := map[string]any{
		"status":     "claimed",
		"claimed_at": time.Now().UTC().Format(time.RFC3339Nano),
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return false, fmt.Errorf("marshal claim: %w", err)
	}
	path := fmt.Sprintf("/commands?id=eq.%d&status=eq.pending", id)
	returned, err := c.patchRowReturning(ctx, path, raw)
	if err != nil {
		return false, err
	}
	return len(returned) > 0, nil
}

// patchRowReturning PATCH't een PostgREST-endpoint met Prefer: return=representation
// en retourneert de resulterende rows (of een lege slice als niets matchte).
// Bij 401 wordt één keer de token gerefreshed en opnieuw geprobeerd.
func (c *Client) patchRowReturning(ctx context.Context, path string, body []byte) ([]json.RawMessage, error) {
	attempt := func(token string) (int, []byte, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPatch, c.restBase+path, bytes.NewReader(body))
		if err != nil {
			return 0, nil, fmt.Errorf("build request: %w", err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("apikey", PublishableKey)
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Prefer", "return=representation")

		resp, err := c.httpClient.Do(req)
		if err != nil {
			return 0, nil, fmt.Errorf("do request: %w", err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, raw, nil
	}

	token, err := c.access(ctx)
	if err != nil {
		return nil, err
	}
	status, raw, err := attempt(token)
	if err != nil {
		return nil, err
	}
	if status == http.StatusUnauthorized {
		token, err = c.refresh(ctx)
		if err != nil {
			return nil, err
		}
		status, raw, err = attempt(token)
		if err != nil {
			return nil, err
		}
	}
	if status < 200 || status >= 300 {
		return nil, fmt.Errorf("PATCH %s: status %d: %s", path, status, string(raw))
	}
	var rows []json.RawMessage
	if err := json.Unmarshal(raw, &rows); err != nil {
		return nil, fmt.Errorf("parse response: %w", err)
	}
	return rows, nil
}
```

Let op: de bestaande `client.go` definieert de `Client` struct. `accessToken` moet public-of-package-local beschikbaar zijn. In de bestaande code is het al lowercase-field in hetzelfde package, dus de test-helper in stap 2.1 werkt. Controleer ook dat `PublishableKey` nog toegankelijk is (is `const` in `client.go`).

- [ ] **Step 2.3: Run tests → PASS**

```bash
go test ./internal/supabase/ -run TestClaimCommand -v
```

- [ ] **Step 2.4: Schrijf failing test voor `CompleteCommand`**

Append to `commands_test.go`:

```go
func TestCompleteCommand_Done(t *testing.T) {
	var gotQuery string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := fakeTokenClient(t, srv.URL)
	result := map[string]any{"upid": "UPID:x", "exitstatus": "OK"}
	if err := c.CompleteCommand(context.Background(), 42, "done", result); err != nil {
		t.Fatalf("err: %v", err)
	}
	if !strings.Contains(gotQuery, "id=eq.42") {
		t.Errorf("query=%q", gotQuery)
	}
	if gotBody["status"] != "done" {
		t.Errorf("status=%v", gotBody["status"])
	}
	if _, ok := gotBody["completed_at"]; !ok {
		t.Errorf("missing completed_at")
	}
	inner, _ := gotBody["result"].(map[string]any)
	if inner["upid"] != "UPID:x" {
		t.Errorf("result.upid=%v", inner["upid"])
	}
}
```

- [ ] **Step 2.5: Implementeer CompleteCommand**

Append to `commands.go`:

```go
// CompleteCommand zet status + result + completed_at in één PATCH.
// status moet "done", "failed" of "expired" zijn.
func (c *Client) CompleteCommand(ctx context.Context, id int64, status string, result map[string]any) error {
	body := map[string]any{
		"status":       status,
		"completed_at": time.Now().UTC().Format(time.RFC3339Nano),
		"result":       result,
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal complete: %w", err)
	}
	path := fmt.Sprintf("/commands?id=eq.%d", id)
	return c.patchRow(ctx, path, raw)
}

// patchRow is als patchRowReturning maar zonder return=representation
// (gebruikt Prefer: return=minimal voor minder bandbreedte).
func (c *Client) patchRow(ctx context.Context, path string, body []byte) error {
	attempt := func(token string) (int, string, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPatch, c.restBase+path, bytes.NewReader(body))
		if err != nil {
			return 0, "", fmt.Errorf("build request: %w", err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("apikey", PublishableKey)
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Prefer", "return=minimal")
		resp, err := c.httpClient.Do(req)
		if err != nil {
			return 0, "", fmt.Errorf("do request: %w", err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(raw), nil
	}
	token, err := c.access(ctx)
	if err != nil {
		return err
	}
	status, body2, err := attempt(token)
	if err != nil {
		return err
	}
	if status == http.StatusUnauthorized {
		token, err = c.refresh(ctx)
		if err != nil {
			return err
		}
		status, body2, err = attempt(token)
		if err != nil {
			return err
		}
	}
	if status < 200 || status >= 300 {
		return fmt.Errorf("PATCH %s: status %d: %s", path, status, body2)
	}
	return nil
}
```

- [ ] **Step 2.6: Run → PASS**

```bash
go test ./internal/supabase/ -run TestCompleteCommand -v
```

- [ ] **Step 2.7: Run → PASS**

```bash
go test ./internal/supabase/ -v
```

- [ ] **Step 2.8: Commit**

```bash
git add internal/supabase/commands.go internal/supabase/commands_test.go
git commit -m "feat(supabase): commands PATCH (claim + complete)"
```

---

### Task 3: Supabase Realtime WS subscriber

**Files:**
- Create: `internal/supabase/realtime.go`
- Create: `internal/supabase/realtime_test.go`
- Modify: `go.mod`, `go.sum` (add `github.com/coder/websocket`)

**Rationale:** Subscribe op `public.commands` via Supabase Realtime v2 (Phoenix-channels over WSS). Stuur alleen INSERT-events, gefilterd op `host_id=eq.<ours>`. Heartbeat elke 25s. Reconnect met exp-backoff. De subscriber exposeert `Subscribe(ctx, hostID) <-chan Command` — hogere laag consumeert.

- [ ] **Step 3.1: Voeg dependency toe**

```bash
cd ~/Documents/Apps/proxmoxvue-agent
go get github.com/coder/websocket@latest
go mod tidy
```

- [ ] **Step 3.2: Schrijf failing test voor de WS connect + join**

Create `internal/supabase/realtime_test.go`:

```go
package supabase

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// mockRealtimeServer is een minimale Phoenix-WS server voor tests.
// Hij accepteert één client, accepteert de phx_join, en kan
// INSERT-frames op verzoek pushen.
type mockRealtimeServer struct {
	srv        *httptest.Server
	mu         sync.Mutex
	conn       *websocket.Conn
	joinedTopic string
	joinedPayload map[string]any
}

func newMockRealtime(t *testing.T) *mockRealtimeServer {
	t.Helper()
	m := &mockRealtimeServer{}
	m.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			t.Errorf("accept: %v", err)
			return
		}
		m.mu.Lock()
		m.conn = c
		m.mu.Unlock()
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		for {
			_, raw, err := c.Read(ctx)
			if err != nil {
				return
			}
			var msg map[string]any
			_ = json.Unmarshal(raw, &msg)
			if ev, _ := msg["event"].(string); ev == "phx_join" {
				m.mu.Lock()
				m.joinedTopic, _ = msg["topic"].(string)
				m.joinedPayload, _ = msg["payload"].(map[string]any)
				m.mu.Unlock()
				ref, _ := msg["ref"].(string)
				reply := map[string]any{
					"topic":   m.joinedTopic,
					"event":   "phx_reply",
					"payload": map[string]any{"status": "ok", "response": map[string]any{}},
					"ref":     ref,
				}
				b, _ := json.Marshal(reply)
				_ = c.Write(ctx, websocket.MessageText, b)
			}
		}
	}))
	return m
}

func (m *mockRealtimeServer) wsURL() string {
	return "ws" + strings.TrimPrefix(m.srv.URL, "http")
}

func (m *mockRealtimeServer) Close() { m.srv.Close() }

func (m *mockRealtimeServer) pushInsert(t *testing.T, topic string, record map[string]any) {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.conn == nil {
		t.Fatal("no client connected yet")
	}
	frame := map[string]any{
		"topic": topic,
		"event": "postgres_changes",
		"payload": map[string]any{
			"data": map[string]any{
				"type":   "INSERT",
				"schema": "public",
				"table":  "commands",
				"record": record,
			},
		},
	}
	b, _ := json.Marshal(frame)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := m.conn.Write(ctx, websocket.MessageText, b); err != nil {
		t.Errorf("push: %v", err)
	}
}

func TestSubscribeCommands_JoinsCorrectChannel(t *testing.T) {
	m := newMockRealtime(t)
	defer m.Close()

	c := fakeTokenClient(t, "http://unused")
	// Override WS URL zodat de test op de mock landt:
	c.realtimeURL = m.wsURL() + "/realtime/v1/websocket"

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, err := c.SubscribeCommands(ctx, "host-abc-123")
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	// Geef de goroutine even om te joinen
	time.Sleep(200 * time.Millisecond)
	m.mu.Lock()
	topic := m.joinedTopic
	m.mu.Unlock()
	if !strings.Contains(topic, "commands") {
		t.Errorf("topic=%q", topic)
	}
}
```

- [ ] **Step 3.3: Run → FAIL, dan realtime.go minimaal implementeren**

Create `internal/supabase/realtime.go`:

```go
package supabase

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
)

const (
	heartbeatInterval = 25 * time.Second
	heartbeatTopic    = "phoenix"
)

// SubscribeCommands opent een Realtime-kanaal voor INSERTs op public.commands
// gefilterd op host_id. Retourneert een channel met Command-events.
// De goroutine blijft draaien tot ctx cancelt; reconnects zijn intern.
func (c *Client) SubscribeCommands(ctx context.Context, hostID string) (<-chan Command, error) {
	if c.realtimeURL == "" {
		c.realtimeURL = fmt.Sprintf("wss://%s.supabase.co/realtime/v1/websocket", c.projectRef)
	}
	out := make(chan Command, 16)
	go c.runSubscription(ctx, hostID, out)
	return out, nil
}

func (c *Client) runSubscription(ctx context.Context, hostID string, out chan<- Command) {
	defer close(out)
	backoff := time.Second
	for {
		if ctx.Err() != nil {
			return
		}
		if err := c.subscribeOnce(ctx, hostID, out); err != nil {
			log.Printf("realtime: subscription loop: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}

func (c *Client) subscribeOnce(ctx context.Context, hostID string, out chan<- Command) error {
	token, err := c.access(ctx)
	if err != nil {
		return fmt.Errorf("get access token: %w", err)
	}

	// TODO (iteratie 2): access_token over de open channel verversen vóór
	// expiry (~1h). Nu laten we de WS droppen bij expiry en reconnecten;
	// dat geeft ~1 reconnect per uur op een stabiele agent.

	// Dial + join + phx_reply moeten binnen 10s rond zijn — anders is
	// Realtime gedegradeerd en reconnecten we liever dan blijven hangen.
	dialCtx, dialCancel := context.WithTimeout(ctx, 10*time.Second)
	defer dialCancel()

	url := fmt.Sprintf("%s?apikey=%s&vsn=1.0.0", c.realtimeURL, PublishableKey)
	conn, _, err := websocket.Dial(dialCtx, url, nil)
	if err != nil {
		return fmt.Errorf("ws dial: %w", err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "bye")

	topic := fmt.Sprintf("realtime:commands:%s", hostID)
	var ref int64
	nextRef := func() string { return strconv.FormatInt(atomic.AddInt64(&ref, 1), 10) }

	joinRef := nextRef()
	join := map[string]any{
		"topic": topic,
		"event": "phx_join",
		"payload": map[string]any{
			"config": map[string]any{
				"postgres_changes": []map[string]any{
					{
						"event":  "INSERT",
						"schema": "public",
						"table":  "commands",
						"filter": "host_id=eq." + hostID,
					},
				},
				// Presence aan zodat iOS-subscribers realtime zien of de agent
				// WS-verbonden is. Zonder actieve presence kan iOS de enqueue-knop
				// niet veilig enablen (last_seen_at is REST-based en mist WS-only
				// disconnects).
				"presence": map[string]any{
					"enabled": true,
					"key":     hostID,
				},
				"private": true,
			},
			"access_token": token,
		},
		"ref":      joinRef,
		"join_ref": joinRef,
	}
	if err := writeJSON(dialCtx, conn, join); err != nil {
		return fmt.Errorf("send join: %w", err)
	}

	// Wacht op phx_reply — als Supabase RLS of config afwijst krijgen we
	// status=error terug. Zonder deze check zou een misconfig zich als
	// "silent success" voordoen (nooit events, geen error).
	_, raw, err := conn.Read(dialCtx)
	if err != nil {
		return fmt.Errorf("wait for phx_reply: %w", err)
	}
	var reply struct {
		Event   string `json:"event"`
		Payload struct {
			Status   string          `json:"status"`
			Response json.RawMessage `json:"response"`
		} `json:"payload"`
	}
	if err := json.Unmarshal(raw, &reply); err != nil {
		return fmt.Errorf("parse phx_reply: %w", err)
	}
	if reply.Event != "phx_reply" {
		return fmt.Errorf("expected phx_reply, got %q", reply.Event)
	}
	if reply.Payload.Status != "ok" {
		return fmt.Errorf("join rejected: %s", string(reply.Payload.Response))
	}
	dialCancel()

	// Heartbeat-loop
	hbCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		t := time.NewTicker(heartbeatInterval)
		defer t.Stop()
		for {
			select {
			case <-hbCtx.Done():
				return
			case <-t.C:
				_ = writeJSON(hbCtx, conn, map[string]any{
					"topic":   heartbeatTopic,
					"event":   "heartbeat",
					"payload": map[string]any{},
					"ref":     nextRef(),
				})
			}
		}
	}()

	// Read-loop
	for {
		_, raw, err := conn.Read(ctx)
		if err != nil {
			return fmt.Errorf("ws read: %w", err)
		}
		var frame struct {
			Topic   string          `json:"topic"`
			Event   string          `json:"event"`
			Payload json.RawMessage `json:"payload"`
		}
		if err := json.Unmarshal(raw, &frame); err != nil {
			log.Printf("realtime: bad frame: %v", err)
			continue
		}
		if frame.Event != "postgres_changes" {
			continue
		}
		var p struct {
			Data struct {
				Type   string          `json:"type"`
				Record json.RawMessage `json:"record"`
			} `json:"data"`
		}
		if err := json.Unmarshal(frame.Payload, &p); err != nil {
			continue
		}
		if p.Data.Type != "INSERT" {
			continue
		}
		var cmd Command
		if err := json.Unmarshal(p.Data.Record, &cmd); err != nil {
			log.Printf("realtime: decode record: %v", err)
			continue
		}
		select {
		case out <- cmd:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func writeJSON(ctx context.Context, conn *websocket.Conn, msg any) error {
	b, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	return conn.Write(ctx, websocket.MessageText, b)
}
```

Voeg `realtimeURL string` veld toe aan de `Client` struct in `client.go`:

```go
type Client struct {
	projectRef   string
	httpClient   *http.Client
	persist      PersistRefreshFunc
	authBase     string
	restBase     string
	realtimeURL  string // override voor tests; leeg = defaults naar wss://<ref>.supabase.co

	mu           sync.Mutex
	accessToken  string
	expiresAt    time.Time
	refreshToken string
}
```

- [ ] **Step 3.4: Run → PASS op join-test**

```bash
go test ./internal/supabase/ -run TestSubscribeCommands -v
```

- [ ] **Step 3.5: Schrijf test voor INSERT-event forward**

Append to `realtime_test.go`:

```go
func TestSubscribeCommands_ForwardsInsert(t *testing.T) {
	m := newMockRealtime(t)
	defer m.Close()

	c := fakeTokenClient(t, "http://unused")
	c.realtimeURL = m.wsURL() + "/realtime/v1/websocket"

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	ch, err := c.SubscribeCommands(ctx, "host-abc-123")
	if err != nil {
		t.Fatal(err)
	}

	time.Sleep(200 * time.Millisecond) // wait for join

	m.pushInsert(t, "realtime:commands:host-abc-123", map[string]any{
		"id":         float64(7),
		"host_id":    "host-abc-123",
		"kind":       "start",
		"payload":    map[string]any{"guest_kind": "qemu", "node": "n1", "vmid": 112},
		"status":     "pending",
		"expires_at": "2099-01-01T00:00:00Z",
	})

	select {
	case cmd := <-ch:
		if cmd.ID != 7 || cmd.Kind != "start" {
			t.Errorf("unexpected cmd: %+v", cmd)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for forwarded command")
	}
}
```

- [ ] **Step 3.6: Run → PASS**

```bash
go test ./internal/supabase/ -run TestSubscribeCommands -v
```

Als het faalt omdat `Command.Payload` een `json.RawMessage` is en de mock een geneste map stuurt: de json-unmarshal converteert dat automatisch (raw blijft raw). OK.

- [ ] **Step 3.7: Commit**

```bash
git add internal/supabase/realtime.go internal/supabase/realtime_test.go internal/supabase/client.go go.mod go.sum
git commit -m "feat(supabase): Realtime WS subscriber for commands table"
```

---

### Task 4: Command dispatcher

**Files:**
- Create: `internal/commands/dispatcher.go`
- Create: `internal/commands/dispatcher_test.go`

**Rationale:** Glue-laag. Eén goroutine consumeert de `<-chan Command`, claimt de rij, voert de Proxmox-actie uit, polt UPID, en schrijft het resultaat terug. Expired of onbekende commands worden als `failed`/`expired` geschreven.

- [ ] **Step 4.1: Definieer interfaces (voor testbaarheid)**

Create `internal/commands/dispatcher.go`:

```go
// Package commands wires Supabase command events to Proxmox actions.
package commands

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
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

	// Hoe lang we maximaal op een task wachten voordat we 'm als
	// "timeout" markeren. Power-acties zijn doorgaans <5s, maar bij
	// shutdown kan het langer duren (guest OS stopt netjes).
	ActionTimeout time.Duration
}

func New(pve ProxmoxActor, store CommandStore) *Dispatcher {
	return &Dispatcher{
		pve:           pve,
		store:         store,
		ActionTimeout: 60 * time.Second,
	}
}
```

- [ ] **Step 4.2: Schrijf failing test voor happy path**

Create `internal/commands/dispatcher_test.go`:

```go
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
	mu       sync.Mutex
	perform  func(ctx context.Context, kind proxmox.GuestKind, node string, vmid int, action proxmox.Action) (string, error)
	await    func(ctx context.Context, node, upid string, timeout time.Duration) (proxmox.TaskStatus, error)
}

func (f *fakeActor) PerformAction(ctx context.Context, kind proxmox.GuestKind, node string, vmid int, action proxmox.Action) (string, error) {
	return f.perform(ctx, kind, node, vmid, action)
}
func (f *fakeActor) AwaitTaskCompletion(ctx context.Context, node, upid string, timeout time.Duration) (proxmox.TaskStatus, error) {
	return f.await(ctx, node, upid, timeout)
}

type fakeStore struct {
	mu         sync.Mutex
	claimed    map[int64]bool
	completed  map[int64]completion
	claimRet   func(id int64) (bool, error)
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
```

- [ ] **Step 4.3: Implementeer Handle (minimaal — happy path)**

Append to `dispatcher.go`:

```go
type commandPayload struct {
	GuestKind string `json:"guest_kind"`
	Node      string `json:"node"`
	VMID      int    `json:"vmid"`
}

// Handle verwerkt één command: claim → dispatch → await → complete.
// Retourneert alleen een error als de claim-call of complete-call zelf faalt;
// Proxmox-fouten leiden tot een completed command met status=failed.
func (d *Dispatcher) Handle(ctx context.Context, cmd supabase.Command) error {
	// 1. TTL-check (decision #196)
	if !cmd.ExpiresAt.IsZero() && time.Now().After(cmd.ExpiresAt) {
		// Stiekem: rij kan al expired zijn op server. We markeren zelf als we 'm nog kunnen claimen.
		if ok, err := d.store.ClaimCommand(ctx, cmd.ID); err == nil && ok {
			return d.store.CompleteCommand(ctx, cmd.ID, "expired", map[string]any{"reason": "ttl"})
		}
		return nil
	}

	// 2. Claim atomair
	ok, err := d.store.ClaimCommand(ctx, cmd.ID)
	if err != nil {
		return fmt.Errorf("claim %d: %w", cmd.ID, err)
	}
	if !ok {
		// Andere instance pakte 'm al, of rij staat niet meer op pending.
		return nil
	}

	// 3. Valideer action + payload
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

	// 4. Dispatch naar Proxmox
	upid, err := d.pve.PerformAction(ctx, guestKind, p.Node, p.VMID, action)
	if err != nil {
		return d.store.CompleteCommand(ctx, cmd.ID, "failed", map[string]any{"error": err.Error()})
	}

	// 5. Await task
	st, err := d.pve.AwaitTaskCompletion(ctx, p.Node, upid, d.ActionTimeout)
	if err != nil {
		return d.store.CompleteCommand(ctx, cmd.ID, "failed", map[string]any{"upid": upid, "error": err.Error()})
	}

	// 6. Klaar
	result := map[string]any{"upid": upid, "exitstatus": st.ExitStatus}
	status := "done"
	if st.ExitStatus != "OK" {
		status = "failed"
	}
	if err := d.store.CompleteCommand(ctx, cmd.ID, status, result); err != nil {
		log.Printf("dispatcher: complete %d: %v", cmd.ID, err)
		return err
	}
	return nil
}
```

- [ ] **Step 4.4: Run → PASS**

```bash
go test ./internal/commands/ -v
```

- [ ] **Step 4.5: Test voor already-claimed (idempotency)**

Append to `dispatcher_test.go`:

```go
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
```

- [ ] **Step 4.6: Test voor onbekende kind**

```go
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
```

- [ ] **Step 4.7: Test voor Proxmox-fout**

```go
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
```

- [ ] **Step 4.8: Run alle tests → PASS**

```bash
go test ./internal/commands/ -v
```

- [ ] **Step 4.9: Commit**

```bash
git add internal/commands/
git commit -m "feat(commands): dispatcher with claim + Proxmox dispatch + result write-back"
```

---

### Task 5: Runtime integration

**Files:**
- Modify: `internal/runtime/runtime.go`

**Rationale:** Start de subscriber en dispatcher naast de bestaande status-ticker. Geen catch-up-scan — gemiste rows zijn de iOS-kant z'n verantwoordelijkheid (backlog #1365) en de 30s TTL is het vangnet.

- [ ] **Step 5.1: Breid runtime.Start uit**

Replace the body of `Start` in `internal/runtime/runtime.go`:

```go
func Start(ctx context.Context, configPath string) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	if err := validate(cfg); err != nil {
		return fmt.Errorf("config invalid: %w", err)
	}

	pve := proxmox.New(proxmox.Config{
		APIURL:         cfg.Proxmox.APIURL,
		APITokenID:     cfg.Proxmox.APITokenID,
		APITokenSecret: cfg.Proxmox.APITokenSecret,
		VerifyTLS:      cfg.Proxmox.VerifyTLS,
	})
	sb := supabase.New(cfg.Supabase.ProjectRef, cfg.Supabase.RefreshToken, persistRefreshTo(configPath))

	if _, err := pve.Version(ctx); err != nil {
		return fmt.Errorf("proxmox version check: %w", err)
	}
	if err := sb.Ping(ctx); err != nil {
		return fmt.Errorf("supabase initial auth: %w", err)
	}
	log.Printf("agent started: host_id=%s proxmox=%s", cfg.Supabase.HostID, cfg.Proxmox.APIURL)

	interval := defaultPollInterval
	if cfg.Agent.PollIntervalSeconds > 0 {
		interval = time.Duration(cfg.Agent.PollIntervalSeconds) * time.Second
	}

	// === Command pipeline ===
	dispatcher := commands.New(pve, sb)
	cmdCh, err := sb.SubscribeCommands(ctx, cfg.Supabase.HostID)
	if err != nil {
		return fmt.Errorf("subscribe commands: %w", err)
	}

	// Consumer goroutine: één-op-één dispatch per event.
	go func() {
		for cmd := range cmdCh {
			go handleCommand(ctx, dispatcher, cmd)
		}
	}()

	// === Metrics push (bestaand gedrag) ===
	pushOnce(ctx, pve, sb, cfg.Supabase.HostID)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Printf("agent stopping: %v", ctx.Err())
			return nil
		case <-ticker.C:
			if err := pushOnce(ctx, pve, sb, cfg.Supabase.HostID); err != nil {
				if errors.Is(err, supabase.ErrRefreshRevoked) {
					return err
				}
			}
		}
	}
}

func handleCommand(ctx context.Context, d *commands.Dispatcher, cmd supabase.Command) {
	if err := d.Handle(ctx, cmd); err != nil {
		log.Printf("command %d: %v", cmd.ID, err)
	}
}
```

Voeg de nieuwe import toe:

```go
import (
	// ...
	"github.com/TheLion/proxmoxvue-agent/internal/commands"
)
```

- [ ] **Step 5.2: Compile-check**

```bash
go build ./...
```
Expected: geen errors.

- [ ] **Step 5.3: Run alle tests**

```bash
go test ./...
```
Expected: alles groen.

- [ ] **Step 5.4: Commit**

```bash
git add internal/runtime/runtime.go
git commit -m "feat(runtime): wire command subscription alongside metrics push"
```

---

### Task 6: Docs update

**Files:**
- Modify: `docs/ARCHITECTURE.md`
- Modify: `CHANGELOG.md`

**Rationale:** De bestaande ARCHITECTURE.md noemt Realtime al in de data-flow — maar niet het command-contract, TTL-gedrag, of catch-up-scan. Leg dat expliciet vast zodat er geen ambiguïteit is als iOS straks de enqueue-kant bouwt.

- [ ] **Step 6.1: Voeg sectie "Command flow" toe aan ARCHITECTURE.md**

Invoegen ná de "Data flow" sectie, vóór "Outbound connections":

```markdown
## Command flow

Commands worden door de iOS-app (owner) in `public.commands` INSERT'ed.
De agent subscribet via Supabase Realtime op INSERT-events met filter
`host_id=eq.<mine>`, claimed de rij atomair via PATCH met conditie
`status=eq.pending`, voert de bijhorende Proxmox-actie uit, en schrijft
het resultaat terug in dezelfde rij.

### Command contract

| Veld | Waarde |
|---|---|
| `kind` | `start` \| `stop` \| `reboot` \| `shutdown` \| `suspend` \| `resume` |
| `payload.guest_kind` | `qemu` \| `lxc` |
| `payload.node` | Proxmox-nodenaam |
| `payload.vmid` | Numeric VMID |

### TTL (decision #196)

`expires_at` staat default op `now() + 30s`. Een command die bij het
claim-moment voorbij de expiry is, wordt als `status=expired`
afgeschreven zonder uitvoering. Dat voorkomt dat een agent die na een
lange netwerkhickup weer online komt, oude (inmiddels door de gebruiker
in de UI herhaalde) commando's alsnog uitvoert.

### Idempotency

De `ClaimCommand`-PATCH zet `status=claimed` met conditie
`status=eq.pending`. PostgREST retourneert een lege array als de
conditie niet matchte — de agent interpreteert dat als "al geclaimed" en
doet niets. Twee agents die tegelijk claimen krijgen dus automatisch een
winner/loser.

### Geen catch-up — presence-based gating

De agent doet géén scan bij reconnect. In plaats daarvan enabled hij
**Supabase Realtime presence** op z'n command-channel, en de iOS-app
subscribet daarop. Zolang de agent's WS verbonden is, zien iOS-clients
`presence_join` en kunnen ze enqueue-acties enablen; valt de WS weg, dan
krijgen ze binnen ~15s een `presence_leave` en disabelt de UI de acties.

Dit is strikter dan `hosts.last_seen_at` (die is REST-based en mist WS-only
disconnects) en vangt het scenario op waarin een agent nog status pushed
maar z'n command-subscribe dood is. De 30s TTL blijft als vangnet voor de
zeldzame race (agent-WS valt precies tussen enqueue en verwerking weg).
```

- [ ] **Step 6.2: Werk CHANGELOG.md bij**

Append:

```markdown
## Unreleased

### Added
- Agent abonneert op Supabase Realtime voor `public.commands` (INSERT) en
  voert power-acties (start/stop/reboot/shutdown/suspend/resume) uit tegen
  de lokale Proxmox REST API.
- Result write-back met UPID + Proxmox exitstatus.
```

- [ ] **Step 6.3: Commit**

```bash
git add docs/ARCHITECTURE.md CHANGELOG.md
git commit -m "docs: command flow + TTL + catch-up"
```

---

## Post-implementation checks (Martijn to run)

Deze stappen vereisen een echte Proxmox + geënrolde agent — niet automatisch uitvoerbaar.

1. Build binary:
   ```bash
   cd ~/Documents/Apps/proxmoxvue-agent
   GOOS=linux GOARCH=amd64 go build -o proxmoxvue-agent ./cmd/proxmoxvue-agent
   ```
2. Scp naar dev-Proxmox, vervang `/usr/local/bin/proxmoxvue-agent`, restart service.
3. Check journal: `journalctl -u proxmoxvue-agent -f` — verwacht log-lines voor "subscribe commands" en bestaande "snapshot pushed".
4. In Supabase SQL editor één test-row inserten:
   ```sql
   insert into commands (host_id, kind, payload)
   values ('<jouw host_id>', 'start', '{"guest_kind":"qemu","node":"proxmox01","vmid":<test-vmid>}'::jsonb);
   ```
5. Binnen enkele seconden moet het `status`-veld `done` worden en `result` de UPID+exitstatus bevatten.
6. Verifieer in Proxmox UI dat de VM daadwerkelijk gestart is.

Als alles groen is → Baserow #126 → status `done`, en we kunnen verder met #1348 (presence-aware cadence).

---

## Self-review

**Spec coverage:**
- Subscribe op commands-tabel → Task 3 ✓
- Power-acties uitvoeren → Task 1 (client) + Task 4 (dispatcher) ✓
- Result terugschrijven → Task 2 (CompleteCommand) + Task 4 (glue) ✓
- Idempotent (#196) → Task 2 (ClaimCommand conditional) + Task 4 (check ok-flag) ✓
- 30s TTL (#196) → Task 4.3 (TTL-check vóór claim) ✓
- RLS (#198) → impliciet: agent authenticeert als agent-user met host_id JWT-claim; geen extra code nodig, RLS doet de scoping server-side
- WS reconnect → Task 3 (subscribeOnce in loop met backoff) ✓
- Geen catch-up → bewuste keuze, afgedekt door #1365 + 30s TTL ✓

**Placeholder scan:** geen TBD / TODO / "handle edge cases" in het plan.

**Type consistency:** `Command`, `GuestKind`, `Action`, `TaskStatus` zijn éénmaal gedefinieerd en consistent gebruikt in alle tasks. `ClaimCommand` signature matcht in interface, implementatie en tests.
