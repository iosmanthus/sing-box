package remoteusers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sagernet/sing-box/log"
)

// newTestService builds a Service wired to a fake target and a real http client,
// bypassing NewService (which needs a full box context).
func newTestService(url, cachePath string, target userUpdater) *Service {
	return &Service{
		ctx:            context.Background(),
		logger:         log.StdLogger(),
		url:            url,
		token:          "tok",
		cachePath:      cachePath,
		requestTimeout: time.Second,
		targets:        []userUpdater{target},
		httpClient:     http.DefaultClient,
	}
}

func TestUpdateAppliesAndPersists(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"users":[{"name":"alice","password":"cEFzcw=="}]}`))
	}))
	defer server.Close()

	target := &fakeUpdater{}
	cachePath := filepath.Join(t.TempDir(), "users.json")
	s := newTestService(server.URL, cachePath, target)
	s.httpClient = server.Client()

	if err := s.update(context.Background()); err != nil {
		t.Fatal(err)
	}
	if target.callCount != 1 || len(target.users) != 1 || target.users[0] != "alice" {
		t.Fatalf("expected one user applied, got %v (calls=%d)", target.users, target.callCount)
	}
	if s.currentCount != 1 {
		t.Fatalf("currentCount = %d, want 1", s.currentCount)
	}
	if cache, err := loadCache(cachePath); err != nil || cache == nil || len(cache.Users) != 1 {
		t.Fatalf("cache not persisted: %+v, %v", cache, err)
	}
}

func TestUpdateKeepsLastGoodOnFetchError(t *testing.T) {
	var fail atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fail.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte(`{"users":[{"name":"alice","password":"cEFzcw=="}]}`))
	}))
	defer server.Close()

	target := &fakeUpdater{}
	s := newTestService(server.URL, "", target)
	s.httpClient = server.Client()

	if err := s.update(context.Background()); err != nil {
		t.Fatal(err)
	}
	fail.Store(true)
	if err := s.update(context.Background()); err == nil {
		t.Fatal("expected error when SOT returns 500")
	}
	if target.callCount != 1 {
		t.Fatalf("UpdateUsers called %d times; must not re-apply on failure", target.callCount)
	}
}

func TestUpdateRefusesEmptyOverNonEmpty(t *testing.T) {
	var empty atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if empty.Load() {
			_, _ = w.Write([]byte(`{"users":[]}`))
			return
		}
		_, _ = w.Write([]byte(`{"users":[{"name":"alice","password":"cEFzcw=="}]}`))
	}))
	defer server.Close()

	target := &fakeUpdater{}
	s := newTestService(server.URL, "", target)
	s.httpClient = server.Client()

	if err := s.update(context.Background()); err != nil {
		t.Fatal(err)
	}
	empty.Store(true)
	if err := s.update(context.Background()); err != nil {
		t.Fatal(err)
	}
	if target.callCount != 1 {
		t.Fatalf("UpdateUsers called %d times; must not wipe a non-empty set with an empty response", target.callCount)
	}
	if s.currentCount != 1 {
		t.Fatalf("currentCount = %d, want 1 (kept previous set)", s.currentCount)
	}
}

func TestUpdateNoopWhenUnchanged(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"users":[{"name":"alice","password":"cEFzcw=="}]}`))
	}))
	defer server.Close()

	target := &fakeUpdater{}
	s := newTestService(server.URL, "", target)
	s.httpClient = server.Client()

	_ = s.update(context.Background())
	_ = s.update(context.Background())
	if target.callCount != 1 {
		t.Fatalf("UpdateUsers called %d times; identical payload must be a no-op", target.callCount)
	}
}
