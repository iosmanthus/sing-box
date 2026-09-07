package remoteusers

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestReportURL(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"https://subgen.example.com/admin/users", "https://subgen.example.com/admin/usage/report"},
		{"http://127.0.0.1:8787/admin/users", "http://127.0.0.1:8787/admin/usage/report"},
	} {
		if got := reportURL(tc.in); got != tc.want {
			t.Fatalf("reportURL(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestPostUsageSendsTokenNodeAndTotals(t *testing.T) {
	var received usageReport
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &received)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	err := postUsage(context.Background(), server.Client(), server.URL, "tok", "relay-1",
		[]usageEntry{{Name: "alice", UplinkBytes: 1, DownlinkBytes: 2, TCPSessions: 3}})
	if err != nil {
		t.Fatalf("postUsage: %v", err)
	}
	if received.Token != "tok" || received.Node != "relay-1" {
		t.Fatalf("auth/identity not sent: %+v", received)
	}
	if len(received.Usage) != 1 || received.Usage[0].DownlinkBytes != 2 {
		t.Fatalf("usage not sent: %+v", received.Usage)
	}
}

func TestPostUsageRejectsNon2xx(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()

	if err := postUsage(context.Background(), server.Client(), server.URL, "tok", "n", []usageEntry{{Name: "a"}}); err == nil {
		t.Fatal("want an error so the caller retries and keeps its totals")
	}
}

func TestUsageChanged(t *testing.T) {
	base := []usageEntry{{Name: "alice", UplinkBytes: 1}}
	for _, tc := range []struct {
		name     string
		previous []usageEntry
		current  []usageEntry
		want     bool
	}{
		{"identical", base, []usageEntry{{Name: "alice", UplinkBytes: 1}}, false},
		{"bytes moved", base, []usageEntry{{Name: "alice", UplinkBytes: 2}}, true},
		{"user added", base, []usageEntry{{Name: "alice", UplinkBytes: 1}, {Name: "bob"}}, true},
		{"user removed", base, nil, true},
		{"first report", nil, base, true},
	} {
		if got := usageChanged(tc.previous, tc.current); got != tc.want {
			t.Fatalf("%s: usageChanged = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestReportSkipsWhenNothingMoved(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	s := newTestService(server.URL, "", &fakeUpdater{})
	s.reportURL = server.URL
	s.tracker.Restore([]usageEntry{{Name: "alice", UplinkBytes: 5}})

	if err := s.report(context.Background()); err != nil {
		t.Fatalf("first report: %v", err)
	}
	if err := s.report(context.Background()); err != nil {
		t.Fatalf("second report: %v", err)
	}
	if calls != 1 {
		t.Fatalf("want 1 POST for an idle node, got %d", calls)
	}
}

func TestReportPrunesOnlyAfterSuccess(t *testing.T) {
	var fail bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fail {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	s := newTestService(server.URL, "", &fakeUpdater{})
	s.reportURL = server.URL
	// alice is still live; bob was removed from the SoT but has unreported bytes.
	s.currentUsers = []userEntry{{Name: "alice"}}
	s.tracker.Restore([]usageEntry{{Name: "alice", UplinkBytes: 1}, {Name: "bob", UplinkBytes: 99}})

	fail = true
	if err := s.report(context.Background()); err == nil {
		t.Fatal("want an error from a failed report")
	}
	if len(s.tracker.Snapshot()) != 2 {
		t.Fatal("a failed report must not drop bob's unreported bytes")
	}

	fail = false
	if err := s.report(context.Background()); err != nil {
		t.Fatalf("retry: %v", err)
	}
	snapshot := s.tracker.Snapshot()
	if len(snapshot) != 1 || snapshot[0].Name != "alice" {
		t.Fatalf("bob should be pruned once reported, got %+v", snapshot)
	}
}
