package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// teardownServer answers DELETE /players/me with deleteStatus and logout with
// 204, and records which tokens made which call.
type teardownServer struct {
	*httptest.Server
	mu      sync.Mutex
	deletes []string
	logouts []string
}

func newTeardownServer(t *testing.T, deleteStatus int) *teardownServer {
	t.Helper()
	ts := &teardownServer{}
	ts.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		ts.mu.Lock()
		defer ts.mu.Unlock()
		switch {
		case r.Method == http.MethodDelete && r.URL.Path == "/api/v1/players/me":
			ts.deletes = append(ts.deletes, tok)
			w.WriteHeader(deleteStatus)
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/auth/logout":
			ts.logouts = append(ts.logouts, tok)
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusTeapot)
		}
	}))
	t.Cleanup(ts.Close)
	return ts
}

func teardownBench(ts *teardownServer, n int) *bench {
	b := &bench{api: newAPIClient(ts.URL, 1000)}
	for i := range n {
		b.players = append(b.players, &player{idx: i, username: "bench" + string(rune('a'+i)), token: "tok" + string(rune('a'+i)), registered: true})
	}
	return b
}

func TestTeardownDeletesAccounts(t *testing.T) {
	ts := newTeardownServer(t, http.StatusNoContent)
	b := teardownBench(ts, 5)
	b.doTeardown()
	if b.teardown.AccountsDeleted != 5 {
		t.Errorf("AccountsDeleted = %d, want 5", b.teardown.AccountsDeleted)
	}
	if len(ts.logouts) != 0 || b.teardown.LoggedOut != 0 {
		t.Errorf("logout made for deleted accounts: %v (LoggedOut %d)", ts.logouts, b.teardown.LoggedOut)
	}
	if n := b.registeredCount(); n != 0 {
		t.Errorf("registeredCount = %d, want 0", n)
	}
	if len(b.teardown.Errors) != 0 {
		t.Errorf("errors: %v", b.teardown.Errors)
	}
}

func TestTeardownFallsBackToLogoutOn404(t *testing.T) {
	ts := newTeardownServer(t, http.StatusNotFound)
	b := teardownBench(ts, 5)
	b.doTeardown()
	if b.teardown.AccountsDeleted != 0 {
		t.Errorf("AccountsDeleted = %d, want 0", b.teardown.AccountsDeleted)
	}
	if len(ts.logouts) != 5 || b.teardown.LoggedOut != 5 {
		t.Errorf("logouts = %d (LoggedOut %d), want 5", len(ts.logouts), b.teardown.LoggedOut)
	}
	if n := b.registeredCount(); n != 5 {
		t.Errorf("registeredCount = %d, want 5", n)
	}
	if len(b.teardown.Errors) != 0 {
		t.Errorf("errors: %v", b.teardown.Errors)
	}
}

func TestTeardownDeleteErrorIsRecordedAndLogsOut(t *testing.T) {
	ts := newTeardownServer(t, http.StatusInternalServerError)
	b := teardownBench(ts, 3)
	b.doTeardown()
	if len(b.teardown.Errors) != 3 {
		t.Errorf("errors = %v, want 3", b.teardown.Errors)
	}
	if len(ts.logouts) != 3 || b.teardown.LoggedOut != 3 {
		t.Errorf("logouts = %d (LoggedOut %d), want 3", len(ts.logouts), b.teardown.LoggedOut)
	}
	if b.teardown.AccountsDeleted != 0 || b.registeredCount() != 3 {
		t.Errorf("deleted %d, registered %d", b.teardown.AccountsDeleted, b.registeredCount())
	}
}
