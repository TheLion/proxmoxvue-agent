package proxmox

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
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

func TestTaskStatus_ParsesRunningAndOK(t *testing.T) {
	cases := []struct {
		name     string
		body     string
		wantDone bool
		wantExit string
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
		t.Errorf("expected >=3 polls, got %d", calls)
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
