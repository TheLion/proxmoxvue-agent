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

// mockRealtimeServer is a minimal Phoenix-WS server for tests.
type mockRealtimeServer struct {
	srv             *httptest.Server
	mu              sync.Mutex
	conn            *websocket.Conn
	joinedTopic     string
	joinedPayload   map[string]any
	joinReply       string // "ok" (default) of "error"
	ready           chan struct{}
	presencePayload map[string]any // laatste 'presence'-frame payload (na phx_reply)
}

func newMockRealtime(t *testing.T) *mockRealtimeServer {
	t.Helper()
	m := &mockRealtimeServer{joinReply: "ok", ready: make(chan struct{})}
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
			ev, _ := msg["event"].(string)
			switch ev {
			case "phx_join":
				m.mu.Lock()
				m.joinedTopic, _ = msg["topic"].(string)
				m.joinedPayload, _ = msg["payload"].(map[string]any)
				status := m.joinReply
				m.mu.Unlock()
				ref, _ := msg["ref"].(string)
				reply := map[string]any{
					"topic":   m.joinedTopic,
					"event":   "phx_reply",
					"payload": map[string]any{"status": status, "response": map[string]any{}},
					"ref":     ref,
				}
				b, _ := json.Marshal(reply)
				_ = c.Write(ctx, websocket.MessageText, b)
				select {
				case <-m.ready:
				default:
					close(m.ready)
				}
			case "presence":
				m.mu.Lock()
				m.presencePayload, _ = msg["payload"].(map[string]any)
				m.mu.Unlock()
			}
		}
	}))
	return m
}

func (m *mockRealtimeServer) wsURL() string {
	return "ws" + strings.TrimPrefix(m.srv.URL, "http") + "/realtime/v1/websocket"
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
	c.realtimeURL = m.wsURL()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, err := c.SubscribeCommands(ctx, "cluster-abc-123")
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	select {
	case <-m.ready:
	case <-time.After(2 * time.Second):
		t.Fatal("join never arrived")
	}

	m.mu.Lock()
	topic := m.joinedTopic
	payload := m.joinedPayload
	m.mu.Unlock()
	if !strings.Contains(topic, "commands") || !strings.Contains(topic, "cluster-abc-123") {
		t.Errorf("topic=%q (want contains commands + cluster_id)", topic)
	}
	// Verify presence is requested
	cfg, _ := payload["config"].(map[string]any)
	presence, _ := cfg["presence"].(map[string]any)
	if enabled, _ := presence["enabled"].(bool); !enabled {
		t.Errorf("presence.enabled not set: %+v", presence)
	}
	// Verify filter is set on postgres_changes
	pc, _ := cfg["postgres_changes"].([]any)
	if len(pc) != 1 {
		t.Fatalf("postgres_changes=%v", pc)
	}
	first, _ := pc[0].(map[string]any)
	if filter, _ := first["filter"].(string); !strings.Contains(filter, "cluster-abc-123") {
		t.Errorf("filter=%q", filter)
	}
}

func TestSubscribeCommands_TracksPresenceAfterJoin(t *testing.T) {
	m := newMockRealtime(t)
	defer m.Close()

	c := fakeTokenClient(t, "http://unused")
	c.realtimeURL = m.wsURL()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, err := c.SubscribeCommands(ctx, "cluster-abc-123")
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	select {
	case <-m.ready:
	case <-time.After(2 * time.Second):
		t.Fatal("join never arrived")
	}

	// The track frame arrives right after phx_reply ok. Give the
	// write-loop a brief moment to reach the mock.
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		m.mu.Lock()
		got := m.presencePayload
		m.mu.Unlock()
		if got != nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	m.mu.Lock()
	payload := m.presencePayload
	m.mu.Unlock()
	if payload == nil {
		t.Fatal("no presence frame received within deadline")
	}
	if typ, _ := payload["type"].(string); typ != "presence" {
		t.Errorf("payload.type=%q want %q", typ, "presence")
	}
	if ev, _ := payload["event"].(string); ev != "track" {
		t.Errorf("payload.event=%q want %q", ev, "track")
	}
	state, _ := payload["payload"].(map[string]any)
	if clusterID, _ := state["cluster_id"].(string); clusterID != "cluster-abc-123" {
		t.Errorf("state.cluster_id=%q want %q", clusterID, "cluster-abc-123")
	}
	if onlineAt, _ := state["online_at"].(string); onlineAt == "" {
		t.Errorf("state.online_at empty: %+v", state)
	}
}

func TestSubscribeCommands_JoinRejectedLoopsAndRetries(t *testing.T) {
	m := newMockRealtime(t)
	m.joinReply = "error"
	defer m.Close()

	c := fakeTokenClient(t, "http://unused")
	c.realtimeURL = m.wsURL()

	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	ch, err := c.SubscribeCommands(ctx, "cluster-abc-123")
	if err != nil {
		t.Fatalf("subscribe returned err: %v", err)
	}

	select {
	case <-m.ready:
	case <-time.After(2 * time.Second):
		t.Fatal("join never arrived")
	}

	// Join is rejected — no events should ever come through.
	select {
	case cmd := <-ch:
		t.Errorf("unexpected command on rejected join: %+v", cmd)
	case <-ctx.Done():
		// Expected: ctx cancels, loop exits, channel closes. Success path.
	}
}

func TestSubscribeCommands_ForwardsInsert(t *testing.T) {
	m := newMockRealtime(t)
	defer m.Close()

	c := fakeTokenClient(t, "http://unused")
	c.realtimeURL = m.wsURL()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	ch, err := c.SubscribeCommands(ctx, "cluster-abc-123")
	if err != nil {
		t.Fatal(err)
	}

	select {
	case <-m.ready:
	case <-time.After(2 * time.Second):
		t.Fatal("join never arrived")
	}

	m.pushInsert(t, "realtime:commands:cluster-abc-123", map[string]any{
		"id":         float64(7),
		"host_id":    "node-1-host-uuid",
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
