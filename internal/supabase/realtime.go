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
	dialTimeout       = 10 * time.Second
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
	dialCtx, dialCancel := context.WithTimeout(ctx, dialTimeout)
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
				// WS-verbonden is. Zonder actieve presence kan iOS de
				// enqueue-knop niet veilig enablen (last_seen_at is REST-based
				// en mist WS-only disconnects).
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
