package supabase

import (
	"context"
	"encoding/json"
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
)

// SubscribeCommands opens a Realtime channel for INSERTs on
// public.commands filtered by cluster_id. Returns a channel with
// Command events. The goroutine keeps running until ctx is cancelled;
// reconnects are internal.
func (c *Client) SubscribeCommands(ctx context.Context, clusterID string) (<-chan Command, error) {
	out := make(chan Command, 16)
	raw := c.subscribeTable(ctx, clusterID, "commands")
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
func (c *Client) SubscribeReadCommands(ctx context.Context, clusterID string) (<-chan ReadCommand, error) {
	out := make(chan ReadCommand, 16)
	raw := c.subscribeTable(ctx, clusterID, "read_commands")
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
func (c *Client) subscribeTable(ctx context.Context, clusterID, table string) <-chan json.RawMessage {
	if c.realtimeURL == "" {
		c.realtimeURL = fmt.Sprintf("wss://%s.supabase.co/realtime/v1/websocket", c.projectRef)
	}
	out := make(chan json.RawMessage, 16)
	go c.runSubscription(ctx, clusterID, table, out)
	return out
}

func (c *Client) runSubscription(ctx context.Context, clusterID, table string, out chan<- json.RawMessage) {
	defer close(out)
	backoff := time.Second
	for {
		if ctx.Err() != nil {
			return
		}
		if err := c.subscribeOnce(ctx, clusterID, table, out); err != nil {
			// Idle disconnect / EOF is normal on long-lived WS —
			// server-side keep-alive timeout or network transition.
			// Reconnect via backoff is enough; no reason to WARN-log
			// every disconnect. Real rejections (Unauthorized,
			// channel-policy) stay WARN.
			msg := err.Error()
			if strings.Contains(msg, "EOF") || strings.Contains(msg, "ws read") {
				slog.Debug("realtime subscription disconnected", "table", table, "err", err)
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

func (c *Client) subscribeOnce(ctx context.Context, clusterID, table string, out chan<- json.RawMessage) error {
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

	url := fmt.Sprintf("%s?apikey=%s&vsn=1.0.0", c.realtimeURL, PublishableKey)
	conn, _, err := websocket.Dial(dialCtx, url, nil)
	if err != nil {
		return fmt.Errorf("ws dial: %w", err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "bye")

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
	dialCancel()

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
	if err := writeJSON(ctx, conn, trackMsg); err != nil {
		slog.Warn("realtime presence track failed", "err", err)
	}

	// Heartbeat loop with ack tracking. hbSent counts hb's sent,
	// hbAcked counts phx_reply on topic="phoenix". If sent runs ever
	// further ahead of acked the WS is dead but not (yet) dropped —
	// silent death we'd otherwise only notice on the next read error.
	var hbSent, hbAcked atomic.Int64
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
				if lag := sent - acked; lag >= 2 {
					slog.Warn("realtime heartbeat-ack lag",
						"table", table, "sent", sent, "acked", acked, "lag", lag)
				} else {
					slog.Debug("realtime heartbeat",
						"table", table, "sent", sent, "acked", acked)
				}
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
