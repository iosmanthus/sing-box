package remoteusers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing-box/option"
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

func TestNewServiceRejectsMissingURL(t *testing.T) {
	_, err := NewService(context.Background(), log.StdLogger(), "users", option.RemoteUsersServiceOptions{})
	if err == nil {
		t.Fatal("expected error for missing url")
	}
}

func TestNewServiceRejectsMissingServers(t *testing.T) {
	_, err := NewService(context.Background(), log.StdLogger(), "users", option.RemoteUsersServiceOptions{
		URL: "https://example.invalid/admin/users",
	})
	if err == nil {
		t.Fatal("expected error for missing servers")
	}
}

func TestUpdateKeepsLastGoodOnTimeout(t *testing.T) {
	var slow atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if slow.Load() {
			time.Sleep(200 * time.Millisecond)
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
	if target.callCount != 1 {
		t.Fatalf("expected one user applied initially, got callCount=%d", target.callCount)
	}

	slow.Store(true)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := s.update(ctx); err == nil {
		t.Fatal("expected error when SOT response exceeds the context timeout")
	}
	if target.callCount != 1 {
		t.Fatalf("UpdateUsers called %d times; must keep last-good on timeout", target.callCount)
	}
}

func TestStartAppliesCacheBeforeFetch(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"users":[{"name":"fresh","password":"cEZyc2g="}]}`))
	}))
	defer server.Close()

	cachePath := filepath.Join(t.TempDir(), "users.json")
	if err := saveCache(cachePath, &cachedUsers{Users: []userEntry{{Name: "cached", Password: "cENhY2g="}}}); err != nil {
		t.Fatal(err)
	}

	target := &fakeUpdater{}
	s := &Service{
		ctx:            context.Background(),
		logger:         log.StdLogger(),
		url:            server.URL,
		token:          "tok",
		interval:       time.Minute,
		requestTimeout: time.Second,
		cachePath:      cachePath,
		downloadDetour: "",
		targets:        []userUpdater{target},
	}

	if err := s.Start(adapter.StartStateStart); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	if len(target.history) < 2 {
		t.Fatalf("expected at least two UpdateUsers calls (cache then fetch), got %d: %v", len(target.history), target.history)
	}
	if len(target.history[0]) != 1 || target.history[0][0] != "cached" {
		t.Fatalf("first apply must be the cached set, got %v", target.history[0])
	}
	if len(target.history[1]) != 1 || target.history[1][0] != "fresh" {
		t.Fatalf("second apply must be the fetched set, got %v", target.history[1])
	}
}

func TestStartColdStartNonFatal(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	unreachableURL := server.URL
	server.Close()

	target := &fakeUpdater{}
	s := &Service{
		ctx:            context.Background(),
		logger:         log.StdLogger(),
		url:            unreachableURL,
		token:          "tok",
		interval:       time.Minute,
		requestTimeout: 100 * time.Millisecond,
		cachePath:      "",
		downloadDetour: "",
		targets:        []userUpdater{target},
	}

	if err := s.Start(adapter.StartStateStart); err != nil {
		t.Fatalf("Start must be non-fatal on cold start, got error: %v", err)
	}
	defer func() {
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
	}()

	if target.callCount != 0 {
		t.Fatalf("UpdateUsers must not be called with no cache and unreachable SOT, callCount=%d", target.callCount)
	}
	if s.currentCount != 0 {
		t.Fatalf("currentCount = %d, want 0", s.currentCount)
	}
}
