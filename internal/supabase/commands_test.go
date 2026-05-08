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

// fakeTokenClient returns a Client that does not refresh — handy for
// tests that only want to verify the REST path.
func fakeTokenClient(t *testing.T, restBase string) *Client {
	t.Helper()
	return &Client{
		baseURL:        "https://test.example.com",
		publishableKey: "test_key",
		httpClient:     &http.Client{Timeout: 5 * time.Second},
		persist:        func(string) error { return nil },
		restBase:       restBase,
		authBase:       "unused",
		realtimeURL:    "wss://test.example.com/realtime/v1/websocket",
		accessToken:    "fake-jwt",
		expiresAt:      time.Now().Add(time.Hour),
		refreshToken:   "fake-refresh",
	}
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
