# Agent Central Refresh-Loop Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Vervang de twee per-subscription refresh-goroutines (één per WS-channel) door één centrale refresh-loop op de `Client` die elke 30 minuten een verse JWT haalt en naar alle geregistreerde channels broadcast. Elimineert de hele klasse "stagger-bugs" tussen subscriptions die de gedeelde `c.accessToken`-cache delen.

**Architecture:** `Client` krijgt een `activeSubs` registry (map: topic → `*activeSub`) beschermd door RWMutex. Eén `refreshLoop` goroutine, gestart bij eerste `registerSubscription` via `sync.Once`, vuurt elke 30 min: `c.refresh()` voor verse token + `writeJSON` access_token-event naar elke geregistreerde sub. `subscribeOnce` registreert na succesvolle phx_join, unregistreert in `defer`. Per-subscription refresh-goroutine, `freshAccessToken` helper, `refreshMu` mutex en `tokenRefreshInterval` constant verdwijnen.

**Tech Stack:** Go 1.21+, github.com/coder/websocket, sync.RWMutex, context.Context, log/slog, time.Ticker.

**Refresh-interval keuze:** 30 min (= helft van Supabase JWT-TTL van 60 min). Garandeert dat elke sub die join't met cached token (worst-case 30 min validity) z'n volgende push krijgt vóór JWT-expiry. Geen pre-flight refresh op join nodig.

---

## File structure

| File | Verandering |
|---|---|
| `internal/supabase/client.go` | Voeg toe: `activeSubs`, `activeSubsMu`, `refreshLoopOnce`, `refreshLoopCancel`, `centralRefreshInterval` const, `activeSub` struct, `registerSubscription`, `unregisterSubscription`, `startRefreshLoop`, `pushAccessTokenLocked` methods. Verwijder: `refreshMu` veld, `freshAccessToken` method. |
| `internal/supabase/realtime.go` | Verwijder: per-sub refresh-goroutine in `subscribeOnce`, `tokenRefreshInterval` const. Voeg toe: `c.registerSubscription(...)` na presence-track + `defer c.unregisterSubscription(topic)`. |
| `internal/supabase/client_test.go` | Nieuwe tests voor register/unregister/concurrency van de registry. |

---

## Task 1: voeg `activeSub` struct + registry + lifecycle methods toe (geen wiring nog)

**Files:**
- Modify: `internal/supabase/client.go` (Client struct + methods)
- Test: `internal/supabase/client_test.go` (nieuw bestand)

- [ ] **Step 1: Schrijf failing tests voor register/unregister/lifecycle**

Voeg toe aan `internal/supabase/client_test.go`:

```go
package supabase

import (
	"sync"
	"testing"
)

func TestRegisterSubscription_AddsToRegistry(t *testing.T) {
	c := &Client{activeSubs: map[string]*activeSub{}}
	sub := &activeSub{topic: "realtime:commands:cluster-x"}
	c.registerSubscription(sub)
	c.activeSubsMu.RLock()
	defer c.activeSubsMu.RUnlock()
	if _, ok := c.activeSubs[sub.topic]; !ok {
		t.Fatalf("sub not in registry")
	}
}

func TestUnregisterSubscription_RemovesFromRegistry(t *testing.T) {
	c := &Client{activeSubs: map[string]*activeSub{}}
	sub := &activeSub{topic: "realtime:commands:cluster-x"}
	c.registerSubscription(sub)
	c.unregisterSubscription(sub.topic)
	c.activeSubsMu.RLock()
	defer c.activeSubsMu.RUnlock()
	if _, ok := c.activeSubs[sub.topic]; ok {
		t.Fatalf("sub still in registry after unregister")
	}
}

func TestRegisterSubscription_OverwritesByTopic(t *testing.T) {
	c := &Client{activeSubs: map[string]*activeSub{}}
	sub1 := &activeSub{topic: "realtime:commands:cluster-x"}
	sub2 := &activeSub{topic: "realtime:commands:cluster-x"}
	c.registerSubscription(sub1)
	c.registerSubscription(sub2)
	c.activeSubsMu.RLock()
	defer c.activeSubsMu.RUnlock()
	if got := c.activeSubs[sub1.topic]; got != sub2 {
		t.Fatalf("expected sub2 to overwrite sub1 by topic, got %p (want %p)", got, sub2)
	}
}

func TestRegisterUnregister_ConcurrentSafe(t *testing.T) {
	c := &Client{activeSubs: map[string]*activeSub{}}
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(2)
		go func(i int) { defer wg.Done(); c.registerSubscription(&activeSub{topic: "t"}) }(i)
		go func(i int) { defer wg.Done(); c.unregisterSubscription("t") }(i)
	}
	wg.Wait()
}
```

- [ ] **Step 2: Run tests, verify they FAIL with "undefined" errors**

Run: `cd ~/Documents/Apps/proxmoxvue-agent && go test ./internal/supabase/ -run "Register|Unregister" -v 2>&1 | tail -20`
Expected: build error (`activeSub`, `activeSubs`, `activeSubsMu`, `registerSubscription`, `unregisterSubscription` undefined).

- [ ] **Step 3: Voeg `activeSub` struct toe aan `client.go`**

In `internal/supabase/client.go`, na de `Client` struct definitie (na `refreshToken string` regel — vóór de sluitende `}`), voeg toe binnen de Client struct:

```go
	// activeSubs registry: topic → active Realtime subscription. Used by
	// the central refresh-loop to broadcast access_token-events to all
	// connected channels in one go.
	activeSubsMu sync.RWMutex
	activeSubs   map[string]*activeSub
```

Verwijder de `refreshMu sync.Mutex` regel volledig (was workaround voor de stagger-bug die we nu architecturaal elimineren).

Voeg toe na de `New()` functie (vóór de constants/types die volgen):

```go
// activeSub represents an active Realtime subscription that the central
// refresh-loop pushes access_token-events to. Lifetime: registered after
// successful phx_join, unregistered in subscribeOnce's defer.
type activeSub struct {
	topic   string
	conn    *websocket.Conn
	nextRef func() string
	ctx     context.Context
}

// registerSubscription adds a sub to the registry, overwriting any
// existing entry with the same topic (handles reconnect cleanly: the
// new conn replaces the old).
func (c *Client) registerSubscription(sub *activeSub) {
	c.activeSubsMu.Lock()
	c.activeSubs[sub.topic] = sub
	c.activeSubsMu.Unlock()
}

// unregisterSubscription removes a sub from the registry by topic.
// No-op if not present (e.g. already replaced by reconnect).
func (c *Client) unregisterSubscription(topic string) {
	c.activeSubsMu.Lock()
	delete(c.activeSubs, topic)
	c.activeSubsMu.Unlock()
}
```

In `New()`, initialiseer de map. Vind de `return &Client{...}` regel en voeg toe vóór de `}`:

```go
		activeSubs: map[string]*activeSub{},
```

Imports controleren: `websocket` is `github.com/coder/websocket` — al geïmporteerd? Check; zo niet, voeg toe.

- [ ] **Step 4: Run tests, verify they PASS**

Run: `cd ~/Documents/Apps/proxmoxvue-agent && go test ./internal/supabase/ -run "Register|Unregister" -v 2>&1 | tail -20`
Expected: PASS (4 tests).

- [ ] **Step 5: Verify full package builds + alle bestaande tests slagen**

Run: `cd ~/Documents/Apps/proxmoxvue-agent && go build ./... && go vet ./... && go test ./... 2>&1 | tail -10`
Expected: alle packages compileren, alle bestaande tests groen.

Let op: `freshAccessToken` is nu nog door `realtime.go` gebruikt — die roeping mag nog staan. We verwijderen hem in Task 4. Maar als we `refreshMu` weghaalden moet `freshAccessToken` herschreven worden zodat hij niet meer naar `c.refreshMu.Lock()` verwijst. Voor nu: laat `freshAccessToken` bestaan maar verwijder de `c.refreshMu.Lock/Unlock` regels (vervang door `c.mu.Lock/Unlock` als die voldoende synchronisatie geeft, of laat de calls zonder mutex met een TODO-comment "wordt verwijderd in plan-task 4").

Kortere oplossing: behoud `freshAccessToken` ongewijzigd (incl `refreshMu`) maar voeg `refreshMu` tijdelijk wel terug aan Client zodat de bestaande code blijft compileren tot Task 4. Dan is dit een puur additieve wijziging.

**Voorkeur**: behoud `refreshMu` + `freshAccessToken` ongewijzigd in Task 1; verwijder beide expliciet pas in Task 4. Geen tussenstaten.

Pas Step 3 hierboven aan: **NIET** `refreshMu` verwijderen in Task 1.

- [ ] **Step 6: Commit**

```bash
cd ~/Documents/Apps/proxmoxvue-agent
git add internal/supabase/client.go internal/supabase/client_test.go
git commit -m "refactor(supabase): add activeSubs registry + register/unregister methods (unwired)"
git push
```

---

## Task 2: voeg centrale refresh-loop toe (singleton via sync.Once)

**Files:**
- Modify: `internal/supabase/client.go`
- Test: `internal/supabase/client_test.go`

- [ ] **Step 1: Schrijf test voor refresh-loop trigger via sync.Once**

Voeg toe aan `internal/supabase/client_test.go`:

```go
func TestStartRefreshLoop_OnlyOnce(t *testing.T) {
	c := &Client{activeSubs: map[string]*activeSub{}}
	var counter int32
	test := func() { atomic.AddInt32(&counter, 1) }
	// startRefreshLoop should fire its actual loop exactly once
	// regardless of repeat invocations.
	c.startRefreshLoopFor(t.Context(), test)
	c.startRefreshLoopFor(t.Context(), test)
	c.startRefreshLoopFor(t.Context(), test)
	if got := atomic.LoadInt32(&counter); got != 1 {
		t.Fatalf("startRefreshLoop fired %d times; want 1", got)
	}
}
```

Voeg `"sync/atomic"` aan de imports toe als die er nog niet staat.

(`startRefreshLoopFor` is een test-only helper die we toevoegen om de Once-gating te testen zonder echt een 30-min ticker te starten.)

- [ ] **Step 2: Run test, verify FAIL**

Run: `cd ~/Documents/Apps/proxmoxvue-agent && go test ./internal/supabase/ -run "StartRefreshLoop" -v 2>&1 | tail -10`
Expected: undefined errors voor `startRefreshLoopFor`, `refreshLoopOnce`.

- [ ] **Step 3: Voeg refresh-loop velden + methods toe**

In `client.go`, in de Client struct, voeg toe na `activeSubs`:

```go
	refreshLoopOnce sync.Once
```

Voeg toe na `unregisterSubscription`:

```go
// centralRefreshInterval is half of the Supabase JWT-TTL (60 min). At
// 30 min, a sub that joined right after a tick still has ≥30 min token
// validity at the next tick — guarantees no JWT-driven EOF without a
// pre-flight refresh on join.
const centralRefreshInterval = 30 * time.Minute

// startRefreshLoop starts the central token-refresh loop. Idempotent
// via sync.Once: subsequent calls are no-ops. Called from
// registerSubscription so the loop activates on the first sub and
// keeps running for the lifetime of the agent.
func (c *Client) startRefreshLoop(ctx context.Context) {
	c.startRefreshLoopFor(ctx, c.refreshAndPushAll)
}

// startRefreshLoopFor is the testable form. fn is the per-tick action.
func (c *Client) startRefreshLoopFor(ctx context.Context, fn func()) {
	c.refreshLoopOnce.Do(func() {
		go func() {
			t := time.NewTicker(centralRefreshInterval)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					fn()
				}
			}
		}()
	})
}

// refreshAndPushAll is called per central-loop tick: refresh the JWT,
// then broadcast an access_token-event to every active sub.
func (c *Client) refreshAndPushAll() {
	ctx := context.Background()
	freshToken, err := c.refresh(ctx)
	if err != nil {
		slog.Warn("central refresh: get fresh token failed", "err", err)
		return
	}
	c.activeSubsMu.RLock()
	subs := make([]*activeSub, 0, len(c.activeSubs))
	for _, s := range c.activeSubs {
		subs = append(subs, s)
	}
	c.activeSubsMu.RUnlock()
	pushed := 0
	for _, sub := range subs {
		if err := c.pushAccessToken(sub, freshToken); err != nil {
			slog.Warn("central refresh: push failed",
				"topic", sub.topic, "err", err)
			continue
		}
		pushed++
	}
	slog.Info("realtime access_token refreshed (central)",
		"subs_pushed", pushed,
		"subs_total", len(subs),
		"token_expires_in", time.Until(c.expiresAt).Round(time.Second).String())
}

// pushAccessToken sends an access_token-event to one sub.
func (c *Client) pushAccessToken(sub *activeSub, token string) error {
	return writeJSON(sub.ctx, sub.conn, map[string]any{
		"topic": sub.topic,
		"event": "access_token",
		"payload": map[string]any{
			"access_token": token,
		},
		"ref": sub.nextRef(),
	})
}
```

Imports needed: `sync` (al), `time` (al), `context` (al), `log/slog` (al). `writeJSON` zit in `realtime.go` — same package, dus accessible.

- [ ] **Step 4: Run test, verify PASS**

Run: `cd ~/Documents/Apps/proxmoxvue-agent && go test ./internal/supabase/ -run "StartRefreshLoop" -v 2>&1 | tail -10`
Expected: PASS.

- [ ] **Step 5: Verify volledige build + alle tests groen**

Run: `cd ~/Documents/Apps/proxmoxvue-agent && go build ./... && go vet ./... && go test ./... 2>&1 | tail -10`
Expected: alles groen.

- [ ] **Step 6: Commit**

```bash
cd ~/Documents/Apps/proxmoxvue-agent
git add internal/supabase/client.go internal/supabase/client_test.go
git commit -m "refactor(supabase): add central refresh-loop + pushAccessToken (unwired)"
git push
```

---

## Task 3: wire register/unregister + startRefreshLoop in subscribeOnce

**Files:**
- Modify: `internal/supabase/realtime.go`

- [ ] **Step 1: Voeg register/unregister toe in subscribeOnce**

In `internal/supabase/realtime.go`, in functie `subscribeOnce`, na de `slog.Info("realtime subscription connected", ...)` regel maar **vóór** de `connectedAt := time.Now()` regel (of direct daarna), voeg toe:

```go
	// Register with the central refresh-loop: this sub will receive
	// access_token-event pushes every centralRefreshInterval. The loop
	// activates on first registration via sync.Once.
	sub := &activeSub{
		topic:   topic,
		conn:    conn,
		nextRef: nextRef,
		ctx:     ctx,
	}
	c.registerSubscription(sub)
	c.startRefreshLoop(ctx)
	defer c.unregisterSubscription(topic)
```

- [ ] **Step 2: Verify build + alle tests groen**

Run: `cd ~/Documents/Apps/proxmoxvue-agent && go build ./... && go vet ./... && go test ./... 2>&1 | tail -10`
Expected: alles groen. Per-sub refresh-goroutine staat NOG, samen met de centrale loop — dubbele refresh tijdelijk. Wordt opgeruimd in Task 4.

- [ ] **Step 3: Commit**

```bash
cd ~/Documents/Apps/proxmoxvue-agent
git add internal/supabase/realtime.go
git commit -m "refactor(realtime): wire subscribeOnce naar centrale refresh-loop (per-sub goroutine nog actief)"
git push
```

---

## Task 4: verwijder per-subscription refresh-goroutine, freshAccessToken, refreshMu, tokenRefreshInterval

**Files:**
- Modify: `internal/supabase/realtime.go`
- Modify: `internal/supabase/client.go`

- [ ] **Step 1: Verwijder per-sub refresh-goroutine in `subscribeOnce`**

In `internal/supabase/realtime.go`, vind het commentaar `// In-channel access_token refresh — Phoenix protocol per Supabase` en het complete `go func() { ... }()` blok dat erop volgt — vanaf de `go func() {` t/m de matching `}()` (de hele anonieme goroutine die de per-sub refresh-tick deed). Verwijder het volledig, inclusief het comment-blok ervoor.

- [ ] **Step 2: Verwijder de `tokenRefreshInterval` const**

In `internal/supabase/realtime.go`, vind in de const-block:

```go
	// tokenRefreshInterval pushes a fresh JWT over the open channel
	// well before the Supabase default 1h JWT-expiry. Without this the
	// server force-closes the WS at expiry (reason=eof, ~1h), creating
	// a brief reconnect-window in which iOS detail-views can still
	// time out — the symptom that drove this change. 50 min leaves a
	// comfortable 10-min margin even under clock skew.
	tokenRefreshInterval = 50 * time.Minute
```

Verwijder volledig (commentaar + regel).

- [ ] **Step 3: Verwijder `freshAccessToken` method en `refreshMu` veld**

In `internal/supabase/client.go`:

a) Verwijder uit Client struct:
```go
	refreshMu sync.Mutex
```
en het bijbehorende commentaar erboven.

b) Verwijder de hele `freshAccessToken` functie (inclusief comment-blok).

- [ ] **Step 4: Verify build**

Run: `cd ~/Documents/Apps/proxmoxvue-agent && go build ./... 2>&1`
Expected: compileert. Als `freshAccessToken` nog aangeroepen wordt elders → de compiler vertelt het. Onverwacht.

- [ ] **Step 5: Verify alle tests groen**

Run: `cd ~/Documents/Apps/proxmoxvue-agent && go test ./... 2>&1 | tail -10`
Expected: alles groen. Bestaande tests in `realtime_test.go` (4 tests met `nil` voor onConnected) blijven werken — niets veranderd aan de subscribe-API.

- [ ] **Step 6: Commit**

```bash
cd ~/Documents/Apps/proxmoxvue-agent
git add internal/supabase/realtime.go internal/supabase/client.go
git commit -m "$(cat <<'EOF'
refactor(supabase): remove per-sub refresh goroutine + freshAccessToken/refreshMu/tokenRefreshInterval

Vervangen door de centrale refresh-loop op Client (commit van Task 2).
Een refresh per 30 min per agent in plaats van twee onafhankelijke
refreshes per channel; broadcast naar alle geregistreerde subs in één
sweep. Elimineert de stagger-bug-klasse waarbij staggered subscriptions
een gedeelde cached-near-stale token konden pushen.
EOF
)"
git push
```

---

## Task 5: end-to-end verificatie via deployment + observatie

**Files:** geen wijzigingen, alleen observatie.

- [ ] **Step 1: Deploy nieuwe agent-image**

Notitie aan user: "Plan-execution compleet. Pull het laatste image en restart de container."

(Dit is handmatig door de user — geen autonome shell-actie.)

- [ ] **Step 2: Observatie-criteria documenteren**

Verwacht in de log binnen 30-90 min na deploy:

```
INFO realtime subscription connected table=read_commands ...
INFO realtime subscription connected table=commands ...
(geen access_token refreshed regels per sub meer)
INFO realtime access_token refreshed (central) subs_pushed=2 subs_total=2 token_expires_in=...
```

Falen-signalen (regressie):
- `level=ERROR` of `panic` in log direct na deploy
- Refresh-event meldt `subs_pushed=0` terwijl `subs_total>0` (push-loop kapot)
- WS closures met `token_expires_in≈0` op een sub die langer dan 1h up is (refresh werkt niet)

- [ ] **Step 3: Bij regressie — rollback**

```bash
cd ~/Documents/Apps/proxmoxvue-agent
git revert HEAD~3..HEAD --no-edit  # rollback Task 4 + Task 3 + Task 2 commits
git push
```

(Task 1's "additive only" commit kan blijven — geen wired changes.)

---

## Self-review

**Spec coverage:** 4 implementatie-tasks dekken: (1) registry, (2) centrale loop, (3) wiring, (4) opruimen. Deployment is Task 5. Volledig.

**Placeholder scan:** alle code-stappen bevatten echte code. Geen "TODO"/"adjust accordingly".

**Type consistency:** `activeSub` struct gedefinieerd in Task 1, gebruikt in Task 2 (`pushAccessToken`) en Task 3 (`subscribeOnce`). Methodnamen consistent: `registerSubscription` / `unregisterSubscription` / `startRefreshLoop` / `refreshAndPushAll` / `pushAccessToken`.

**Sequencing:** elke task is op zichzelf compileerbaar/testbaar. Task 1+2 zijn additief (geen gedrag-wijziging). Task 3 wired in (dubbele refresh tijdelijk — niet schadelijk, alleen extra verkeer). Task 4 verwijdert oude code. Single-commit-rollbacks per task mogelijk.

**Risico's:** zie Task 5 step 3 voor falen-signalen + rollback-procedure.
