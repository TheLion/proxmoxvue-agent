package commands

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/TheLion/proxmoxvue-agent/internal/supabase"
)

type fakeReader struct {
	calls []string
	resp  map[string]json.RawMessage
	err   error
}

func (f *fakeReader) GetRaw(ctx context.Context, path string) (json.RawMessage, error) {
	f.calls = append(f.calls, path)
	if f.err != nil {
		return nil, f.err
	}
	if r, ok := f.resp[path]; ok {
		return r, nil
	}
	return json.RawMessage(`{"data":null}`), nil
}

type fakeReadStore struct {
	mu        sync.Mutex
	claimed   map[int64]bool
	completed map[int64]readCompletion
	claimRet  func(id int64) (bool, error)
}

type readCompletion struct {
	status string
	result json.RawMessage
	errMsg string
}

func (f *fakeReadStore) ClaimReadCommand(ctx context.Context, id int64) (bool, error) {
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

func (f *fakeReadStore) CompleteReadCommand(ctx context.Context, id int64, status string, result json.RawMessage, errMsg string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.completed == nil {
		f.completed = map[int64]readCompletion{}
	}
	f.completed[id] = readCompletion{status: status, result: result, errMsg: errMsg}
	return nil
}

func newReadCmd(id int64, endpoint string, params map[string]any) supabase.ReadCommand {
	raw, _ := json.Marshal(params)
	return supabase.ReadCommand{
		ID:        id,
		HostID:    "host-abc",
		Endpoint:  endpoint,
		Params:    raw,
		Status:    "pending",
		ExpiresAt: time.Now().Add(30 * time.Second),
	}
}

func TestReadHandle_HappyPath(t *testing.T) {
	body := json.RawMessage(`{"data":[{"upid":"x"}]}`)
	reader := &fakeReader{resp: map[string]json.RawMessage{"/api2/json/cluster/tasks": body}}
	store := &fakeReadStore{claimRet: func(int64) (bool, error) { return true, nil }}

	d := NewReadDispatcher(reader, store)
	if err := d.Handle(context.Background(), newReadCmd(1, "/api2/json/cluster/tasks", nil)); err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if len(reader.calls) != 1 || reader.calls[0] != "/api2/json/cluster/tasks" {
		t.Errorf("expected one call to whitelisted endpoint, got %v", reader.calls)
	}
	c := store.completed[1]
	if c.status != "done" || string(c.result) != string(body) || c.errMsg != "" {
		t.Errorf("unexpected completion: %+v", c)
	}
}

func TestReadHandle_EndpointNotAllowed(t *testing.T) {
	reader := &fakeReader{}
	store := &fakeReadStore{claimRet: func(int64) (bool, error) { return true, nil }}

	d := NewReadDispatcher(reader, store)
	if err := d.Handle(context.Background(), newReadCmd(2, "/api2/json/access/users", nil)); err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if len(reader.calls) != 0 {
		t.Errorf("expected no Proxmox calls for blocked endpoint, got %v", reader.calls)
	}
	c := store.completed[2]
	if c.status != "failed" {
		t.Errorf("expected failed, got %s", c.status)
	}
	if c.errMsg == "" || c.result != nil {
		t.Errorf("expected error message and nil result, got %+v", c)
	}
}

func TestReadHandle_ProxmoxError(t *testing.T) {
	reader := &fakeReader{err: errors.New("status 500: gateway timeout")}
	store := &fakeReadStore{claimRet: func(int64) (bool, error) { return true, nil }}

	d := NewReadDispatcher(reader, store)
	if err := d.Handle(context.Background(), newReadCmd(3, "/api2/json/cluster/status", nil)); err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	c := store.completed[3]
	if c.status != "failed" || c.errMsg == "" {
		t.Errorf("expected failed with errMsg, got %+v", c)
	}
}

func TestReadHandle_ExpiredBeforeDispatch(t *testing.T) {
	reader := &fakeReader{}
	store := &fakeReadStore{claimRet: func(int64) (bool, error) { return true, nil }}

	d := NewReadDispatcher(reader, store)
	cmd := newReadCmd(4, "/api2/json/cluster/status", nil)
	cmd.ExpiresAt = time.Now().Add(-1 * time.Second)

	if err := d.Handle(context.Background(), cmd); err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if len(reader.calls) != 0 {
		t.Errorf("expected no Proxmox calls for expired command, got %v", reader.calls)
	}
	if store.completed[4].status != "expired" {
		t.Errorf("expected expired, got %s", store.completed[4].status)
	}
}

func TestReadHandle_AlreadyClaimedByOther(t *testing.T) {
	reader := &fakeReader{}
	store := &fakeReadStore{claimRet: func(int64) (bool, error) { return false, nil }}

	d := NewReadDispatcher(reader, store)
	if err := d.Handle(context.Background(), newReadCmd(5, "/api2/json/cluster/status", nil)); err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if len(reader.calls) != 0 {
		t.Errorf("expected no calls when claim returned false, got %v", reader.calls)
	}
	if _, ok := store.completed[5]; ok {
		t.Errorf("did not expect completion when not claimant")
	}
}

func TestBuildPath_AppendsParams(t *testing.T) {
	cases := []struct {
		name     string
		endpoint string
		params   map[string]any
		want     string
	}{
		{"none", "/api2/json/nodes/n1/tasks", nil, "/api2/json/nodes/n1/tasks"},
		{"empty", "/api2/json/nodes/n1/tasks", map[string]any{}, "/api2/json/nodes/n1/tasks"},
		{"limit", "/api2/json/nodes/n1/tasks", map[string]any{"limit": 50.0}, "/api2/json/nodes/n1/tasks?limit=50"},
		{"content", "/api2/json/nodes/n1/storage/local/content", map[string]any{"content": "iso"}, "/api2/json/nodes/n1/storage/local/content?content=iso"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var raw json.RawMessage
			if tc.params != nil {
				raw, _ = json.Marshal(tc.params)
			}
			got, err := buildPath(tc.endpoint, raw)
			if err != nil {
				t.Fatalf("unexpected: %v", err)
			}
			if got != tc.want {
				t.Errorf("got %q want %q", got, tc.want)
			}
		})
	}
}

func TestEndpointAllowed(t *testing.T) {
	allowed := []string{
		"/api2/json/cluster/resources",
		"/api2/json/cluster/status",
		"/api2/json/cluster/tasks",
		"/api2/json/nodes/pve1/status",
		"/api2/json/nodes/pve1/network",
		"/api2/json/nodes/pve1/firewall/rules",
		"/api2/json/nodes/pve1/tasks",
		"/api2/json/nodes/pve1/tasks/UPID:pve1:0001:abc/status",
		"/api2/json/nodes/pve1/tasks/UPID:pve1:0001:abc/log",
		"/api2/json/nodes/pve1/qemu/100/config",
		"/api2/json/nodes/pve1/lxc/200/snapshot",
		"/api2/json/nodes/pve1/qemu/100/feature",
		"/api2/json/nodes/pve1/qemu/100/agent/get-fsinfo",
		"/api2/json/nodes/pve1/storage/local/content",
	}
	denied := []string{
		"/api2/json/access/users",
		"/api2/json/access/openid/auth-url",
		"/api2/json/cluster/firewall/rules",
		"/api2/json/nodes/pve1/qemu/100/config/secret",
		"/api2/json/nodes/pve1/qemu/100",
		"/api2/json/nodes/pve1/lxc/200/agent/get-fsinfo",
		"/api2/json/nodes/pve1/qemu/100/agent/exec",
		"/api2/json/nodes/pve1/qemu/100/agent/file-write",
		"",
	}
	for _, p := range allowed {
		if !endpointAllowed(p) {
			t.Errorf("expected allowed: %s", p)
		}
	}
	for _, p := range denied {
		if endpointAllowed(p) {
			t.Errorf("expected denied: %s", p)
		}
	}
}
