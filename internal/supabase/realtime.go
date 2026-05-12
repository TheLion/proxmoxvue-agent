package supabase

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
)

const (
	heartbeatInterval = 25 * time.Second
	heartbeatTopic    = "phoenix"
	dialTimeout       = 10 * time.Second

	// readDeadline aborts conn.Read if no frame arrives within this
	// window. Supabase Realtime sends a phx_reply on every heartbeat
	// (~25s), so 60s of silence is always abnormal — typically a NAT
	// eviction or upstream edge drop that left the WS half-open.
	readDeadline = 60 * time.Second

	// ackLagThreshold: missed heartbeat-acks before we force-close.
	// 2 missed acks = ~50s without server response — long enough to
	// rule out a single packet-loss, short enough to recover before
	// the user notices their detail-view hanging.
	ackLagThreshold = int64(2)

	// tokenRefreshInterval pushes a fresh JWT over the open channel
	// well before the Supabase default 1h JWT-expiry. Without this the
	// server force-closes the WS at expiry (reason=eof, ~1h), creating
	// a brief reconnect-window in which iOS detail-views can still
	// time out — the symptom that drove this change. 50 min leaves a
	// comfortable 10-min margin even under clock skew.
	tokenRefreshInterval = 50 * time.Minute
)

// OnConnectedFunc runs after a successful (re)join. The agent uses this
// to catch up on rows the Realtime stream missed during a WS gap — the
// only events delivered post-join are NEW inserts, never historical.
type OnConnectedFunc func(ctx context.Context)

// SubscribeCommands opens a Realtime channel for INSERTs on
// public.commands filtered by cluster_id. Returns a channel with
// Command events. The goroutine keeps running until ctx is cancelled;
// reconnects are internal. onConnected runs after every successful
// (re)join — agents use this to catch up on missed events.
func (c *Client) SubscribeCommands(ctx context.Context, clusterID string, onConnected OnConnectedFunc) (<-chan Command, error) {
	out := make(chan Command, 16)
	raw := c.subscribeTable(ctx, clusterID, "commands", onConnected)
	go func() {
		defer close(out)
		for r := range raw {
			var cmd Command
			if err := json.Unmarshal(r, &cmd); err != nil {
				slog.Warn("realtime decode command failed", "err", err)
				continue
			}
			select {
			case out <- cmd:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out, nil
}

// SubscribeReadCommands is the read-RPC equivalent of SubscribeCommands.
// Separate channel so the read-dispatcher can run independently from
// the write-dispatcher — failures on one side don't affect the other.
func (c *Client) SubscribeReadCommands(ctx context.Context, clusterID string, onConnected OnConnectedFunc) (<-chan ReadCommand, error) {
	out := make(chan ReadCommand, 16)
	raw := c.subscribeTable(ctx, clusterID, "read_commands", onConnected)
	go func() {
		defer close(out)
		for r := range raw {
			var cmd ReadCommand
			if err := json.Unmarshal(r, &cmd); err != nil {
				slog.Warn("realtime decode read_command failed", "err", err)
				continue
			}
			select {
			case out <- cmd:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out, nil
}

// subscribeTable opens a Realtime WS for INSERTs on the given table,
// filtered by cluster_id, and pushes raw record bytes onto the
// returned channel. Reconnects are internal; the channel closes when
// ctx is cancelled.
func (c *Client) subscribeTable(ctx context.Context, clusterID, table string, onConnected OnConnectedFunc) <-chan json.RawMessage {
	out := make(chan json.RawMessage, 16)
	go c.runSubscription(ctx, clusterID, table, onConnected, out)
	return out
}

func (c *Client) runSubscription(ctx context.Context, clusterID, table string, onConnected OnConnectedFunc, out chan<- json.RawMessage) {
	defer close(out)
	backoff := time.Second
	for {
		if ctx.Err() != nil {
			return
		}
		if err := c.subscribeOnce(ctx, clusterID, table, onConnected, out); err != nil {
			// Pre-join rejections (auth, RLS, bad realtime URL) stay WARN.
			// Post-join disconnects are logged inside subscribeOnce with
			// a structured reason ("realtime ws closed"), so we don't
			// double-log here.
			if errors.Is(err, errPostJoinClosed) {
				// Already logged with reason+duration inside subscribeOnce.
			} else {
				slog.Warn("realtime subscription rejected", "table", table, "err", err)
			}
		}
		// Jitter (±25%) so agents from different users don't
		// synchronously reconnect after a Supabase outage.
		jitter := time.Duration(rand.Int64N(int64(backoff/2))) - backoff/4
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff + jitter):
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}

// errPostJoinClosed is a sentinel: subscribeOnce already logged a
// structured "realtime ws closed" line with reason+duration. The
// reconnect loop in runSubscription must NOT log again.
var errPostJoinClosed = errors.New("realtime ws closed (post-join)")

func (c *Client) subscribeOnce(ctx context.Context, clusterID, table string, onConnected OnConnectedFunc, out chan<- json.RawMessage) error {
	token, err := c.access(ctx)
	if err != nil {
		return fmt.Errorf("get access token: %w", err)
	}

	// TODO (iteration 2): refresh access_token over the open channel
	// before expiry (~1h). For now we let the WS drop on expiry and
	// reconnect; that's about 1 reconnect per hour on a stable agent.

	// Dial + join + phx_reply must complete within 10s — otherwise
	// Realtime is degraded and we'd rather reconnect than hang.
	dialCtx, dialCancel := context.WithTimeout(ctx, dialTimeout)
	defer dialCancel()

	url := fmt.Sprintf("%s?apikey=%s&vsn=1.0.0", c.realtimeURL, c.publishableKey)
	slog.Debug("realtime ws dial", "table", table, "url", c.realtimeURL)
	conn, _, err := websocket.Dial(dialCtx, url, nil)
	if err != nil {
		return fmt.Errorf("ws dial: %w", err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "bye")
	slog.Debug("realtime ws connected", "table", table)

	topic := fmt.Sprintf("realtime:%s:%s", table, clusterID)
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
						"table":  table,
						"filter": "cluster_id=eq." + clusterID,
					},
				},
				// Presence on so iOS subscribers see in real time
				// whether the agent is WS-connected. Without active
				// presence iOS can't safely enable the enqueue button
				// (last_seen_at is REST-based and misses WS-only
				// disconnects).
				"presence": map[string]any{
					"enabled": true,
					"key":     clusterID,
				},
				"private": true,
			},
			"access_token": token,
		},
		"ref":      joinRef,
		"join_ref": joinRef,
	}
	slog.Debug("realtime channel join request", "topic", topic, "table", table, "cluster_id", clusterID)
	if err := writeJSON(dialCtx, conn, join); err != nil {
		return fmt.Errorf("send join: %w", err)
	}

	// Wait for phx_reply — if Supabase RLS or config rejects we get
	// status=error back. Without this check a misconfig would behave
	// as "silent success" (never any events, no error).
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
	slog.Info("realtime subscription connected", "table", table, "topic", topic)
	dialCancel()
	connectedAt := time.Now()

	// Catch-up: events that arrived between WS death and reconnect are
	// not replayed by Realtime — only NEW inserts after join are
	// delivered. Run the catch-up after a successful join so the
	// dispatcher can pick up anything still in `pending`. Runs in its
	// own goroutine so it can't stall the read-loop; the dispatcher's
	// atomic claim handles the race with concurrent Realtime delivery.
	if onConnected != nil {
		go onConnected(ctx)
	}

	// Track presence: without an explicit track frame iOS doesn't see
	// any presence_join, not even when the join config has presence
	// enabled. Best-effort — if the write fails the read loop will
	// pick up the dead connection.
	trackMsg := map[string]any{
		"topic": topic,
		"event": "presence",
		"payload": map[string]any{
			"type":  "presence",
			"event": "track",
			"payload": map[string]any{
				"cluster_id": clusterID,
				"online_at":  time.Now().UTC().Format(time.RFC3339),
			},
		},
		"ref":      nextRef(),
		"join_ref": joinRef,
	}
	slog.Debug("realtime presence track sent", "topic", topic, "cluster_id", clusterID)
	if err := writeJSON(ctx, conn, trackMsg); err != nil {
		slog.Warn("realtime presence track failed", "err", err)
	}

	// Heartbeat loop with ack tracking + force-close on lag. hbSent
	// counts heartbeats sent, hbAcked counts phx_reply on
	// topic="phoenix". Pre-fix this only logged; now lag >= 2 (~50s of
	// silence) actively closes the WS so the read-loop errors out and
	// the reconnect-loop fires. Without this, a half-open TCP (NAT
	// eviction, Cloudflare edge drop, etc.) would let writes succeed
	// into the OS buffer forever while Read blocks indefinitely.
	var hbSent, hbAcked atomic.Int64
	// closeReason is set by whoever decides to kill the conn so the
	// read-loop can log a structured reason. atomic.Pointer because
	// it's read on a different goroutine than the heartbeat that sets it.
	var closeReason atomic.Pointer[string]
	setReason := func(r string) {
		closeReason.CompareAndSwap(nil, &r)
	}

	hbCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	// In-channel access_token refresh — Phoenix protocol per Supabase
	// Realtime docs: push event="access_token" with the new JWT to the
	// channel topic; server validates, re-authorizes, updates Postgres
	// subscriptions in-place. Avoids the otherwise-hourly WS reconnect
	// at JWT-expiry that briefly stalls iOS detail-views (#1632).
	go func() {
		t := time.NewTicker(tokenRefreshInterval)
		defer t.Stop()
		for {
			select {
			case <-hbCtx.Done():
				return
			case <-t.C:
				freshToken, err := c.access(hbCtx)
				if err != nil {
					slog.Warn("realtime token refresh: get fresh token failed",
						"table", table, "err", err)
					continue
				}
				if err := writeJSON(hbCtx, conn, map[string]any{
					"topic": topic,
					"event": "access_token",
					"payload": map[string]any{
						"access_token": freshToken,
					},
					"ref": nextRef(),
				}); err != nil {
					// Don't trigger our own close here — the read-loop
					// will catch any actual error. Refresh-write failure
					// most likely means the conn is already going down
					// for another reason.
					slog.Warn("realtime token refresh: write failed",
						"table", table, "err", err)
					continue
				}
				// token_expires_in shows whether c.access actually returned
				// a fresh token or just the cached one (still valid above
				// refreshSkew=30s). If we see ~22m here while JWT-TTL is
				// ~72m, the in-channel push delivers a near-stale JWT and
				// the server-side WS-life isn't extended → cause of the
				// observed reason=eof at ~1h12m. Diagnostic only — fix
				// follows after one cycle of confirmed evidence.
				c.mu.Lock()
				expiresIn := time.Until(c.expiresAt).Round(time.Second).String()
				c.mu.Unlock()
				slog.Info("realtime access_token refreshed",
					"table", table, "topic", topic, "token_expires_in", expiresIn)
			}
		}
	}()

	go func() {
		t := time.NewTicker(heartbeatInterval)
		defer t.Stop()
		for {
			select {
			case <-hbCtx.Done():
				return
			case <-t.C:
				if err := writeJSON(hbCtx, conn, map[string]any{
					"topic":   heartbeatTopic,
					"event":   "heartbeat",
					"payload": map[string]any{},
					"ref":     nextRef(),
				}); err != nil {
					return
				}
				sent := hbSent.Add(1)
				acked := hbAcked.Load()
				lag := sent - acked
				slog.Debug("realtime heartbeat",
					"table", table, "sent", sent, "acked", acked, "lag", lag)
				if lag >= ackLagThreshold {
					slog.Warn("realtime heartbeat-ack lag — force-closing ws",
						"table", table, "sent", sent, "acked", acked, "lag", lag)
					setReason("ack-lag")
					// CloseNow skips the polite close handshake — the
					// peer is presumed dead, no point waiting on a close
					// reply that won't arrive.
					_ = conn.CloseNow()
					return
				}
			}
		}
	}()

	// Read-loop with per-read deadline. Each conn.Read is wrapped in a
	// context-with-timeout so a silent half-open TCP can't hang it
	// forever. Supabase Realtime sends a phx_reply on every heartbeat
	// (~25s), so readDeadline (60s) of pure silence is always abnormal.
	for {
		readCtx, readCancel := context.WithTimeout(ctx, readDeadline)
		_, raw, err := conn.Read(readCtx)
		readCancel()
		if err != nil {
			// Determine close-reason for the structured log:
			// 1. ack-lag (heartbeat goroutine set it)
			// 2. read-timeout (our own deadline fired)
			// 3. peer-close / eof (peer or network)
			reason := "peer-close"
			if r := closeReason.Load(); r != nil {
				reason = *r
			} else if errors.Is(err, context.DeadlineExceeded) {
				reason = "read-timeout"
				setReason(reason)
			} else if strings.Contains(err.Error(), "EOF") {
				reason = "eof"
			}
			// token_expires_in at close-time discriminates JWT-driven from
			// other-reason closes. ~0 ⇒ JWT-exp drove the close (fix
			// = push genuinely-fresh tokens). >>0 ⇒ NAT/edge/peer
			// dropped the conn for other reasons.
			c.mu.Lock()
			tokenExpiresIn := time.Until(c.expiresAt).Round(time.Second).String()
			c.mu.Unlock()
			slog.Warn("realtime ws closed",
				"table", table,
				"reason", reason,
				"duration", time.Since(connectedAt).Round(time.Second).String(),
				"token_expires_in", tokenExpiresIn,
				"err", err)
			return errPostJoinClosed
		}
		var frame struct {
			Topic   string          `json:"topic"`
			Event   string          `json:"event"`
			Payload json.RawMessage `json:"payload"`
		}
		if err := json.Unmarshal(raw, &frame); err != nil {
			slog.Debug("realtime bad frame", "err", err)
			continue
		}
		// Heartbeat ack: phx_reply on topic="phoenix" matches our heartbeats.
		if frame.Event == "phx_reply" && frame.Topic == heartbeatTopic {
			hbAcked.Add(1)
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
		// Pre-filter visibility: lets DEBUG sessions confirm Realtime
		// is delivering events even when they're being dropped because
		// type != INSERT.
		slog.Debug("realtime postgres_changes received", "table", table, "type", p.Data.Type)
		if p.Data.Type != "INSERT" {
			continue
		}
		select {
		case out <- p.Data.Record:
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
