# remote_users Pull Service Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a sing-box background service `remote-users` that pulls the user list from the subgen-worker SOT at startup and on a poll interval (default 1m), and drives shadowsocks multi-user `UpdateUsers`, with an on-disk last-good cache as the floor.

**Architecture:** A new `adapter.Service` in `service/remoteusers/`, modeled on the `route/rule/rule_set_remote.go` lifecycle (load cache → synchronous-but-non-fatal initial fetch → ticker poll with ETag) and the `service/ssmapi` server-resolution pattern (resolve a `servers` name→inbound-tag map into `adapter.ManagedSSMServer` targets). Each reconcile fetches the full list and calls `UpdateUsers` (whole-table replace). It never wipes a non-empty live set: a fetch error, or an empty 200 response over a non-empty set, is logged and ignored.

**Tech Stack:** Go, sing-box adapter/service framework, `sing-shadowsocks` multi-user service (`UpdateUsers`), `net/http` + `httptest`, `encoding/json`.

---

## Background facts (verified on this branch, v1.14.0-alpha.32 base)

- `adapter.ManagedSSMServer` (`adapter/ssm.go:9`): `Inbound` + `SetTracker(SSMTracker)` + `UpdateUsers(users []string, uPSKs []string) error`. Implemented by `*shadowsocks.MultiInbound` (`protocol/shadowsocks/inbound_multi.go:33`).
- A nil tracker is safe — `MultiInbound` guards `h.tracker != nil` (`inbound_multi.go:178,203`). This service does **not** call `SetTracker`.
- `UpdateUsers` is a full replace (`sing-shadowsocks@v0.2.8/shadowaead_2022/service_multi.go:97-99`); passwords are base64 uPSKs (`...:103-115`).
- A `managed: true` shadowsocks inbound **forbids** static `users` (`protocol/shadowsocks/inbound.go:35-36`) and starts with zero users (`inbound.go:38`, `inbound_multi.go:86`). So the target inbound must be `managed: true`, and the on-disk cache is the only durable floor.
- SOT contract: `POST {url}` body `{"token":...}` → `{"users":[{name,password}]}` (`subgen-worker/src/handler.ts:82,88`, `src/user-service.ts:56`).

---

## File Structure

| File | Responsibility |
|------|----------------|
| `constant/proxy.go` (modify) | Add the `TypeRemoteUsers = "remote-users"` service-type constant. |
| `option/remote_users.go` (create) | `RemoteUsersServiceOptions` config struct. |
| `service/remoteusers/fetch.go` (create) | HTTP fetch + parse + the `userUpdater` interface, `applyUsers`, `hashUsers`. |
| `service/remoteusers/cache.go` (create) | On-disk last-good cache (atomic JSON file). |
| `service/remoteusers/service.go` (create) | `Service` (lifecycle), `NewService` (target resolution), reconcile `update`, ticker loop, `RegisterService`. |
| `include/registry.go` (modify) | Register the service so the config parser knows `"type":"remote-users"`. |
| `service/remoteusers/*_test.go` (create) | Unit tests per unit. |
| `docs/configuration/service/remote-users.md` (create) | Config documentation. |

---

## Deferred from spec (conscious scope decisions)

- **Poll jitter** — the spec lists ±jitter to avoid a thundering herd. For a small node fleet this is marginal; omitted from the MVP to keep the loop deterministic and untested-path-free. Re-add later as a `rand`-based sleep in `loopUpdate` if the fleet grows.
- **Read-only node token on the SOT** — a `subgen-worker` change (new auth scope), not a sing-box task. Prerequisite for production deployment; tracked in the spec.

---

## Task 1: Service-type constant + options struct

**Files:**
- Modify: `constant/proxy.go` (the const block containing `TypeSSMAPI`)
- Create: `option/remote_users.go`

- [ ] **Step 1: Add the service-type constant**

In `constant/proxy.go`, find the line `TypeSSMAPI = "ssm-api"` and add directly below it:

```go
	TypeRemoteUsers        = "remote-users"
```

- [ ] **Step 2: Create the options struct**

Create `option/remote_users.go`:

```go
package option

import (
	"github.com/sagernet/sing/common/json/badjson"
	"github.com/sagernet/sing/common/json/badoption"
)

type RemoteUsersServiceOptions struct {
	URL            string                            `json:"url"`
	Token          string                            `json:"token,omitempty"`
	Interval       badoption.Duration                `json:"interval,omitempty"`
	RequestTimeout badoption.Duration                `json:"request_timeout,omitempty"`
	CachePath      string                            `json:"cache_path,omitempty"`
	DownloadDetour string                            `json:"download_detour,omitempty"`
	Servers        *badjson.TypedMap[string, string] `json:"servers"`
}
```

- [ ] **Step 3: Verify it builds**

Run: `go build ./constant/... ./option/...`
Expected: no output, exit 0.

- [ ] **Step 4: Commit**

```bash
git add constant/proxy.go option/remote_users.go
git commit -m "feat(remote-users): add service type constant and options"
```

---

## Task 2: `fetchUsers` — HTTP fetch + parse + 304

**Files:**
- Create: `service/remoteusers/fetch.go`
- Test: `service/remoteusers/fetch_test.go`

- [ ] **Step 1: Write the failing test**

Create `service/remoteusers/fetch_test.go`:

```go
package remoteusers

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestFetchUsersParsesList(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Etag", `"v1"`)
		_, _ = w.Write([]byte(`{"users":[{"name":"alice","password":"cEFzcw=="},{"name":"bob","password":"cEJzcw=="}]}`))
	}))
	defer server.Close()

	result, err := fetchUsers(context.Background(), server.Client(), server.URL, "tok", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(result.users) != 2 || result.users[0].Name != "alice" || result.users[1].Password != "cEJzcw==" {
		t.Fatalf("unexpected users: %+v", result.users)
	}
	if result.etag != `"v1"` {
		t.Fatalf("unexpected etag: %q", result.etag)
	}
}

func TestFetchUsersNotModified(t *testing.T) {
	var sentEtag string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sentEtag = r.Header.Get("If-None-Match")
		w.WriteHeader(http.StatusNotModified)
	}))
	defer server.Close()

	result, err := fetchUsers(context.Background(), server.Client(), server.URL, "tok", `"v1"`)
	if err != nil {
		t.Fatal(err)
	}
	if !result.notModified {
		t.Fatal("expected notModified=true")
	}
	if sentEtag != `"v1"` {
		t.Fatalf("If-None-Match not sent; got %q", sentEtag)
	}
}

func TestFetchUsersErrorStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	if _, err := fetchUsers(context.Background(), server.Client(), server.URL, "tok", ""); err == nil {
		t.Fatal("expected error on HTTP 500")
	}
}

func TestFetchUsersSendsToken(t *testing.T) {
	var gotBody string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		_, _ = w.Write([]byte(`{"users":[]}`))
	}))
	defer server.Close()

	if _, err := fetchUsers(context.Background(), server.Client(), server.URL, "secret", ""); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(gotBody, `"secret"`) {
		t.Fatalf("token not in request body: %s", gotBody)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./service/remoteusers/ -run TestFetchUsers -v`
Expected: build failure — `undefined: fetchUsers` (package/file does not exist yet).

- [ ] **Step 3: Write minimal implementation**

Create `service/remoteusers/fetch.go`:

```go
package remoteusers

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"io"
	"net/http"
	"sort"

	E "github.com/sagernet/sing/common/exceptions"
)

// userUpdater is the narrow capability the service needs from a managed
// inbound. adapter.ManagedSSMServer satisfies it; tests use a fake.
type userUpdater interface {
	UpdateUsers(users []string, uPSKs []string) error
}

type userEntry struct {
	Name     string `json:"name"`
	Password string `json:"password"`
}

type usersResponse struct {
	Users []userEntry `json:"users"`
}

type fetchResult struct {
	users       []userEntry
	etag        string
	notModified bool
}

// fetchUsers POSTs {"token":...} to url and parses {"users":[{name,password}]}.
// When etag is non-empty it is sent as If-None-Match; a 304 yields notModified.
func fetchUsers(ctx context.Context, client *http.Client, url, token, etag string) (fetchResult, error) {
	body, err := json.Marshal(map[string]string{"token": token})
	if err != nil {
		return fetchResult{}, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fetchResult{}, err
	}
	request.Header.Set("Content-Type", "application/json")
	if etag != "" {
		request.Header.Set("If-None-Match", etag)
	}
	response, err := client.Do(request)
	if err != nil {
		return fetchResult{}, err
	}
	defer response.Body.Close()
	switch response.StatusCode {
	case http.StatusOK:
	case http.StatusNotModified:
		return fetchResult{notModified: true}, nil
	default:
		return fetchResult{}, E.New("unexpected status: ", response.Status)
	}
	content, err := io.ReadAll(response.Body)
	if err != nil {
		return fetchResult{}, err
	}
	var parsed usersResponse
	if err = json.Unmarshal(content, &parsed); err != nil {
		return fetchResult{}, E.Cause(err, "parse user list")
	}
	return fetchResult{users: parsed.Users, etag: response.Header.Get("Etag")}, nil
}

// hashUsers returns a stable, order-independent hash of the user set.
func hashUsers(users []userEntry) [32]byte {
	sorted := make([]userEntry, len(users))
	copy(sorted, users)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })
	h := sha256.New()
	for _, u := range sorted {
		h.Write([]byte(u.Name))
		h.Write([]byte{0})
		h.Write([]byte(u.Password))
		h.Write([]byte{0})
	}
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

// applyUsers pushes the full user set to every target via UpdateUsers.
func applyUsers(targets []userUpdater, users []userEntry) error {
	names := make([]string, len(users))
	passwords := make([]string, len(users))
	for i, u := range users {
		names[i] = u.Name
		passwords[i] = u.Password
	}
	for _, target := range targets {
		if err := target.UpdateUsers(names, passwords); err != nil {
			return err
		}
	}
	return nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./service/remoteusers/ -run TestFetchUsers -v`
Expected: PASS (4 tests).

- [ ] **Step 5: Commit**

```bash
git add service/remoteusers/fetch.go service/remoteusers/fetch_test.go
git commit -m "feat(remote-users): add HTTP user-list fetch + parse"
```

---

## Task 3: `applyUsers` + `hashUsers`

**Files:**
- Modify: `service/remoteusers/fetch.go` (already contains `applyUsers`/`hashUsers` from Task 2)
- Test: `service/remoteusers/apply_test.go`

- [ ] **Step 1: Write the failing test**

Create `service/remoteusers/apply_test.go`:

```go
package remoteusers

import (
	"errors"
	"testing"
)

type fakeUpdater struct {
	users     []string
	uPSKs     []string
	callCount int
	err       error
}

func (f *fakeUpdater) UpdateUsers(users []string, uPSKs []string) error {
	if f.err != nil {
		return f.err
	}
	f.users = append([]string(nil), users...)
	f.uPSKs = append([]string(nil), uPSKs...)
	f.callCount++
	return nil
}

func TestApplyUsersPushesToAllTargets(t *testing.T) {
	a, b := &fakeUpdater{}, &fakeUpdater{}
	err := applyUsers([]userUpdater{a, b}, []userEntry{{Name: "alice", Password: "pa"}, {Name: "bob", Password: "pb"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range []*fakeUpdater{a, b} {
		if len(f.users) != 2 || f.users[0] != "alice" || f.uPSKs[1] != "pb" {
			t.Fatalf("unexpected push: %v / %v", f.users, f.uPSKs)
		}
	}
}

func TestApplyUsersPropagatesError(t *testing.T) {
	bad := &fakeUpdater{err: errors.New("boom")}
	if err := applyUsers([]userUpdater{bad}, []userEntry{{Name: "x", Password: "y"}}); err == nil {
		t.Fatal("expected error to propagate")
	}
}

func TestHashUsersOrderIndependentAndContentSensitive(t *testing.T) {
	a := hashUsers([]userEntry{{Name: "alice", Password: "1"}, {Name: "bob", Password: "2"}})
	b := hashUsers([]userEntry{{Name: "bob", Password: "2"}, {Name: "alice", Password: "1"}})
	if a != b {
		t.Fatal("hash must be order-independent")
	}
	c := hashUsers([]userEntry{{Name: "alice", Password: "1"}, {Name: "bob", Password: "3"}})
	if a == c {
		t.Fatal("hash must change when a password changes")
	}
}
```

- [ ] **Step 2: Run test to verify it passes (implementation already exists)**

Run: `go test ./service/remoteusers/ -run 'TestApplyUsers|TestHashUsers' -v`
Expected: PASS (3 tests). (The functions were written in Task 2; this task adds their tests and the shared `fakeUpdater`.)

- [ ] **Step 3: Commit**

```bash
git add service/remoteusers/apply_test.go
git commit -m "test(remote-users): cover applyUsers and hashUsers"
```

---

## Task 4: On-disk last-good cache

**Files:**
- Create: `service/remoteusers/cache.go`
- Test: `service/remoteusers/cache_test.go`

- [ ] **Step 1: Write the failing test**

Create `service/remoteusers/cache_test.go`:

```go
package remoteusers

import (
	"path/filepath"
	"testing"
)

func TestCacheRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "users.json")
	in := &cachedUsers{Users: []userEntry{{Name: "alice", Password: "pa"}}, Etag: `"v1"`}
	if err := saveCache(path, in); err != nil {
		t.Fatal(err)
	}
	out, err := loadCache(path)
	if err != nil {
		t.Fatal(err)
	}
	if out == nil || len(out.Users) != 1 || out.Users[0].Name != "alice" || out.Etag != `"v1"` {
		t.Fatalf("roundtrip mismatch: %+v", out)
	}
}

func TestLoadCacheMissingFileReturnsNil(t *testing.T) {
	out, err := loadCache(filepath.Join(t.TempDir(), "absent.json"))
	if err != nil {
		t.Fatal(err)
	}
	if out != nil {
		t.Fatalf("expected nil for missing file, got %+v", out)
	}
}

func TestCacheEmptyPathIsNoop(t *testing.T) {
	if err := saveCache("", &cachedUsers{}); err != nil {
		t.Fatal(err)
	}
	out, err := loadCache("")
	if err != nil || out != nil {
		t.Fatalf("empty path should be a no-op; got %+v, %v", out, err)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./service/remoteusers/ -run TestCache -v`
Expected: build failure — `undefined: cachedUsers`, `undefined: saveCache`, `undefined: loadCache`.

- [ ] **Step 3: Write minimal implementation**

Create `service/remoteusers/cache.go`:

```go
package remoteusers

import (
	"encoding/json"
	"os"
	"time"
)

type cachedUsers struct {
	Users       []userEntry `json:"users"`
	Etag        string      `json:"etag,omitempty"`
	LastUpdated time.Time   `json:"last_updated"`
}

// loadCache reads the cached user list. A missing file or empty path returns
// (nil, nil) — there is simply no floor yet.
func loadCache(path string) (*cachedUsers, error) {
	if path == "" {
		return nil, nil
	}
	content, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var cache cachedUsers
	if err = json.Unmarshal(content, &cache); err != nil {
		return nil, err
	}
	return &cache, nil
}

// saveCache atomically writes the cached user list (temp file + rename). An
// empty path is a no-op.
func saveCache(path string, cache *cachedUsers) error {
	if path == "" {
		return nil
	}
	content, err := json.Marshal(cache)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err = os.WriteFile(tmp, content, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./service/remoteusers/ -run TestCache -v`
Expected: PASS (3 tests).

- [ ] **Step 5: Commit**

```bash
git add service/remoteusers/cache.go service/remoteusers/cache_test.go
git commit -m "feat(remote-users): add atomic on-disk last-good cache"
```

---

## Task 5: `Service` skeleton + `update` reconcile (last-good + empty-guard)

**Files:**
- Create: `service/remoteusers/service.go`
- Test: `service/remoteusers/service_test.go`

This task adds the `Service` struct and the heart of the feature — the `update` reconcile — and tests it by constructing the struct directly (white-box). `NewService`, `Start`, `Close`, and the ticker loop are added in Task 6.

- [ ] **Step 1: Write the failing test**

Create `service/remoteusers/service_test.go`:

```go
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
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./service/remoteusers/ -run TestUpdate -v`
Expected: build failure — `undefined: Service` (and its fields).

- [ ] **Step 3: Write minimal implementation**

Create `service/remoteusers/service.go`:

```go
package remoteusers

import (
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/sagernet/sing-box/adapter"
	boxService "github.com/sagernet/sing-box/adapter/service"
	C "github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing-box/option"
	E "github.com/sagernet/sing/common/exceptions"
	"github.com/sagernet/sing/service"
)

const (
	defaultInterval       = time.Minute
	defaultRequestTimeout = 10 * time.Second
)

type Service struct {
	boxService.Adapter
	ctx            context.Context
	cancel         context.CancelFunc
	logger         log.ContextLogger
	url            string
	token          string
	interval       time.Duration
	requestTimeout time.Duration
	cachePath      string
	downloadDetour string
	targets        []userUpdater
	httpClient     *http.Client
	ticker         *time.Ticker
	access         sync.Mutex
	lastEtag       string
	lastHash       [32]byte
	currentCount   int
}

// update performs one reconcile: fetch -> (304 / unchanged / empty-over-nonempty
// -> no-op) -> apply -> persist. On any error it returns without mutating the
// live user set, so the last-good set is preserved.
func (s *Service) update(ctx context.Context) error {
	s.access.Lock()
	defer s.access.Unlock()

	result, err := fetchUsers(ctx, s.httpClient, s.url, s.token, s.lastEtag)
	if err != nil {
		return err
	}
	if result.notModified {
		return nil
	}
	// Safety floor: never wipe a non-empty live set with an empty response.
	if len(result.users) == 0 && s.currentCount > 0 {
		s.logger.Warn("remote user list is empty; keeping previous ", s.currentCount, " users")
		return nil
	}
	newHash := hashUsers(result.users)
	if newHash == s.lastHash {
		s.lastEtag = result.etag
		return nil
	}
	if len(result.users) > 0 {
		if err = applyUsers(s.targets, result.users); err != nil {
			return err
		}
	}
	s.lastHash = newHash
	s.lastEtag = result.etag
	s.currentCount = len(result.users)
	if err = saveCache(s.cachePath, &cachedUsers{Users: result.users, Etag: result.etag, LastUpdated: time.Now()}); err != nil {
		s.logger.Error(E.Cause(err, "save user cache"))
	}
	s.logger.Info("updated to ", s.currentCount, " users")
	return nil
}

// Ensure the package imports are all used until Task 6 fills in the rest.
var (
	_ = adapter.StartStateStart
	_ = boxService.NewAdapter
	_ = C.TypeRemoteUsers
	_ = option.RemoteUsersServiceOptions{}
	_ = service.FromContext[adapter.InboundManager]
)
```

Note: the trailing `var (...)` block keeps the `adapter`/`boxService`/`C`/`option`/`service` imports referenced so the package compiles in this intermediate state. It is **deleted** in Task 6 once `NewService`/`Start`/`Close` use those imports for real.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./service/remoteusers/ -run TestUpdate -v`
Expected: PASS (4 tests).

- [ ] **Step 5: Commit**

```bash
git add service/remoteusers/service.go service/remoteusers/service_test.go
git commit -m "feat(remote-users): add reconcile update with last-good + empty-guard"
```

---

## Task 6: `NewService` + lifecycle (`Start`/`Close`/loop) + transport

**Files:**
- Modify: `service/remoteusers/service.go`
- Test: `service/remoteusers/service_test.go` (add construction-guard tests)

- [ ] **Step 1: Write the failing test**

Append to `service/remoteusers/service_test.go`:

```go
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
```

Add the import `"github.com/sagernet/sing-box/option"` to the test file's import block (the test now references `option.RemoteUsersServiceOptions`).

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./service/remoteusers/ -run TestNewService -v`
Expected: build failure — `undefined: NewService`.

- [ ] **Step 3: Write the implementation**

In `service/remoteusers/service.go`, **delete** the temporary `var (...)` block from Task 5, and add `RegisterService`, `NewService`, `Start`, `Close`, `loopUpdate`, and `resolveTransport`:

```go
func RegisterService(registry *boxService.Registry) {
	boxService.Register[option.RemoteUsersServiceOptions](registry, C.TypeRemoteUsers, NewService)
}

func NewService(ctx context.Context, logger log.ContextLogger, tag string, options option.RemoteUsersServiceOptions) (adapter.Service, error) {
	if options.URL == "" {
		return nil, E.New("missing url")
	}
	if options.Servers == nil || options.Servers.Size() == 0 {
		return nil, E.New("missing servers")
	}
	inboundManager := service.FromContext[adapter.InboundManager](ctx)
	if inboundManager == nil {
		return nil, E.New("inbound manager not available")
	}
	var targets []userUpdater
	for i, entry := range options.Servers.Entries() {
		inbound, loaded := inboundManager.Get(entry.Value)
		if !loaded {
			return nil, E.New("remote_users server[", i, "]: inbound ", entry.Value, " not found")
		}
		managed, isManaged := inbound.(adapter.ManagedSSMServer)
		if !isManaged {
			return nil, E.New("remote_users server[", i, "]: inbound/", inbound.Type(), "[", inbound.Tag(), "] is not a managed (SSM) server")
		}
		targets = append(targets, managed)
	}
	interval := defaultInterval
	if options.Interval > 0 {
		interval = time.Duration(options.Interval)
	}
	requestTimeout := defaultRequestTimeout
	if options.RequestTimeout > 0 {
		requestTimeout = time.Duration(options.RequestTimeout)
	}
	serviceCtx, cancel := context.WithCancel(ctx)
	return &Service{
		Adapter:        boxService.NewAdapter(C.TypeRemoteUsers, tag),
		ctx:            serviceCtx,
		cancel:         cancel,
		logger:         logger,
		url:            options.URL,
		token:          options.Token,
		interval:       interval,
		requestTimeout: requestTimeout,
		cachePath:      options.CachePath,
		downloadDetour: options.DownloadDetour,
		targets:        targets,
	}, nil
}

func (s *Service) Start(stage adapter.StartStage) error {
	if stage != adapter.StartStateStart {
		return nil
	}
	transport, err := s.resolveTransport()
	if err != nil {
		return E.Cause(err, "create remote_users http client")
	}
	s.httpClient = &http.Client{Timeout: s.requestTimeout, Transport: transport}

	// Floor: apply the on-disk last-good cache before the first fetch, so the
	// node comes up with users even while the SOT is unreachable.
	if cache, cacheErr := loadCache(s.cachePath); cacheErr != nil {
		s.logger.Error(E.Cause(cacheErr, "load user cache"))
	} else if cache != nil && len(cache.Users) > 0 {
		if applyErr := applyUsers(s.targets, cache.Users); applyErr != nil {
			s.logger.Error(E.Cause(applyErr, "apply cached users"))
		} else {
			s.lastHash = hashUsers(cache.Users)
			s.lastEtag = cache.Etag
			s.currentCount = len(cache.Users)
			s.logger.Info("loaded ", s.currentCount, " users from cache")
		}
	}

	// Synchronous initial fetch, but non-fatal: never block startup on the SOT.
	fetchCtx, cancel := context.WithTimeout(s.ctx, s.requestTimeout)
	err = s.update(fetchCtx)
	cancel()
	if err != nil {
		s.logger.Error(E.Cause(err, "initial user fetch (continuing with cached/empty set)"))
	}

	s.ticker = time.NewTicker(s.interval)
	go s.loopUpdate()
	return nil
}

func (s *Service) Close() error {
	if s.cancel != nil {
		s.cancel()
	}
	if s.ticker != nil {
		s.ticker.Stop()
	}
	return nil
}

func (s *Service) loopUpdate() {
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-s.ticker.C:
			fetchCtx, cancel := context.WithTimeout(s.ctx, s.requestTimeout)
			err := s.update(fetchCtx)
			cancel()
			if err != nil {
				s.logger.Error(E.Cause(err, "update users (keeping previous set)"))
			}
		}
	}
}

// resolveTransport returns Go's default transport for a direct egress fetch, or
// a detour-bound transport from the HTTP client manager when download_detour is set.
func (s *Service) resolveTransport() (http.RoundTripper, error) {
	if s.downloadDetour == "" {
		return http.DefaultTransport, nil
	}
	httpClientManager := service.FromContext[adapter.HTTPClientManager](s.ctx)
	if httpClientManager == nil {
		return nil, E.New("download_detour set but http client manager unavailable")
	}
	return httpClientManager.ResolveTransport(s.ctx, s.logger, option.HTTPClientOptions{
		DialerOptions: option.DialerOptions{
			Detour: s.downloadDetour,
		},
		DisableEmptyDirectCheck: true,
	})
}
```

- [ ] **Step 4: Run the package tests to verify everything passes**

Run: `go test ./service/remoteusers/ -v`
Expected: PASS (all tests from Tasks 2–6).

- [ ] **Step 5: Commit**

```bash
git add service/remoteusers/service.go service/remoteusers/service_test.go
git commit -m "feat(remote-users): add NewService, lifecycle, and transport"
```

---

## Task 7: Register the service + verify end-to-end wiring

**Files:**
- Modify: `include/registry.go`
- Test: `service/remoteusers/register_test.go`

- [ ] **Step 1: Write the failing test**

Create `service/remoteusers/register_test.go`:

```go
package remoteusers

import (
	"testing"

	boxService "github.com/sagernet/sing-box/adapter/service"
	C "github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-box/option"
)

func TestServiceRegistered(t *testing.T) {
	registry := boxService.NewRegistry()
	RegisterService(registry)
	options, loaded := registry.CreateOptions(C.TypeRemoteUsers)
	if !loaded {
		t.Fatalf("service type %q not registered", C.TypeRemoteUsers)
	}
	if _, ok := options.(*option.RemoteUsersServiceOptions); !ok {
		t.Fatalf("registered options has wrong type: %T", options)
	}
}
```

- [ ] **Step 2: Run test to verify it passes**

Run: `go test ./service/remoteusers/ -run TestServiceRegistered -v`
Expected: PASS (`RegisterService` already exists from Task 6).

- [ ] **Step 3: Wire the service into the global registry**

In `include/registry.go`, add the import next to the existing ssmapi import (around line 42):

```go
	"github.com/sagernet/sing-box/service/remoteusers"
```

And in `func ServiceRegistry()`, add the registration call directly after `ssmapi.RegisterService(registry)` (around line 139):

```go
	remoteusers.RegisterService(registry)
```

- [ ] **Step 4: Verify the whole project builds and `sing-box check` parses a real config**

Run:
```bash
go build ./...
```
Expected: exit 0.

Then create `/tmp/remote-users-check.json`:

```json
{
  "log": { "level": "error" },
  "inbounds": [
    {
      "type": "shadowsocks",
      "tag": "ss-in",
      "listen": "127.0.0.1",
      "listen_port": 18080,
      "method": "2022-blake3-aes-128-gcm",
      "password": "Kpep1tTLBjkbZS4Ms5tELw==",
      "managed": true
    }
  ],
  "services": [
    {
      "type": "remote-users",
      "tag": "users",
      "url": "https://example.invalid/admin/users",
      "token": "test-token",
      "interval": "1m",
      "cache_path": "/tmp/remote-users-cache.json",
      "servers": { "ss-in": "ss-in" }
    }
  ],
  "outbounds": [{ "type": "direct", "tag": "direct" }]
}
```

Run:
```bash
go run ./cmd/sing-box check -c /tmp/remote-users-check.json
```
Expected: exit 0, no error. This exercises config parsing **and** `NewService` resolving `ss-in` to a `ManagedSSMServer`. (If it reports the PSK is the wrong length for the method, regenerate a 16-byte key: `go run ./cmd/sing-box generate rand --base64 16` and replace `password`.)

- [ ] **Step 5: Commit**

```bash
git add include/registry.go service/remoteusers/register_test.go
git commit -m "feat(remote-users): register service in the global registry"
```

---

## Task 8: Documentation + final verification

**Files:**
- Create: `docs/configuration/service/remote-users.md`

- [ ] **Step 1: Write the documentation**

Create `docs/configuration/service/remote-users.md`:

```markdown
# Remote Users

The `remote-users` service pulls the inbound user list from a remote
source-of-truth (SOT) at startup and on a poll interval, and applies it to
managed shadowsocks multi-user inbounds via whole-table replacement.

It is firewall-friendly (the node initiates an outbound HTTPS request) and
self-healing (every poll is a full-state reconcile). It never wipes a non-empty
live user set: a fetch error, or an empty response over a non-empty set, is
logged and ignored. The on-disk cache (`cache_path`) is the floor — a node
restarting while the SOT is down comes up with the last-known-good users.

## Structure

​```json
{
  "type": "remote-users",
  "tag": "users",
  "url": "https://sot.example.com/admin/users",
  "token": "<node-token>",
  "interval": "1m",
  "request_timeout": "10s",
  "cache_path": "users.json",
  "download_detour": "",
  "servers": {
    "ss-in": "shadowsocks-inbound-tag"
  }
}
​```

## Fields

| Field | Type | Description |
|-------|------|-------------|
| `url` | string (required) | SOT endpoint. The service sends `POST {url}` with body `{"token":...}` and expects `{"users":[{"name":...,"password":...}]}`. `password` is the base64 uPSK. |
| `token` | string | Bearer-equivalent token sent in the request body. Use a read-only, node-scoped credential. |
| `interval` | duration | Poll interval. Default `1m`. |
| `request_timeout` | duration | Per-request timeout. Default `10s`. |
| `cache_path` | string | Path to the last-good cache file. When set, the cache is applied at startup before the first fetch. |
| `download_detour` | string | Optional outbound tag to route the fetch through. Empty = direct egress. |
| `servers` | map[string]string | Maps an arbitrary name to a managed inbound tag. Each target inbound must be a shadowsocks multi-user inbound with `"managed": true`. |

## Target inbound

The target shadowsocks inbound must set `"managed": true` and must **not**
declare static `users` (the two are mutually exclusive). A managed inbound
starts with zero users; `remote-users` populates it.
```

(Note: the ```` ​``` ```` fences inside the doc above contain a zero-width space only to keep this plan readable — when creating the file, use normal triple-backtick fences.)

- [ ] **Step 2: Run the full verification suite**

Run:
```bash
gofmt -l service/remoteusers/ option/remote_users.go
go vet ./service/remoteusers/...
go test ./service/remoteusers/ -v
go build ./...
```
Expected: `gofmt -l` prints nothing (all formatted); `go vet` clean; all tests PASS; build exit 0.

- [ ] **Step 3: Commit**

```bash
git add docs/configuration/service/remote-users.md
git commit -m "docs(remote-users): document the remote-users service"
```

---

## Self-Review (completed by plan author)

**Spec coverage:**
- Standalone `remote_users` service → Tasks 1, 5, 6, 7. ✓
- shadowsocks multi-user via `UpdateUsers`, no inbound changes → `userUpdater`/`applyUsers` (Task 2/3), target resolution (Task 6). ✓
- rule_set_remote lifecycle (cache load → sync initial fetch → ticker + ETag) → Task 6 `Start`/`loopUpdate`, Task 2 ETag/304. ✓
- Never `UpdateUsers([])` over non-empty last-good; initial fetch non-fatal → Task 5 `update` (empty-guard + error path), Task 6 `Start`. ✓
- Cache = floor; `managed:true` target → Task 4 cache, Task 6 `Start` cache-apply, Task 8 docs. ✓
- Contract `POST {token}` → `{users:[{name,password}]}`, password=base64 uPSK → Task 2 + docs. ✓
- `download_detour`, `request_timeout`, configurable `interval` → Tasks 1, 6. ✓
- Security: single writer + read-only node token → docs (Task 8); SOT token scope is cross-repo, noted as deferred. ✓
- ETag local-hash no-op → Task 5 `hashUsers` compare. ✓
- Jitter → consciously deferred (see "Deferred from spec"). ✓ (noted, not silently dropped)

**Placeholder scan:** No TBD/TODO. Config PSK is a concrete valid value with a regenerate fallback. No "add error handling" hand-waves — every code step is complete.

**Type consistency:** `userUpdater.UpdateUsers(users, uPSKs)`, `userEntry{Name,Password}`, `fetchResult{users,etag,notModified}`, `cachedUsers{Users,Etag,LastUpdated}`, `Service` fields, and `update`/`fetchUsers`/`applyUsers`/`hashUsers`/`loadCache`/`saveCache`/`resolveTransport` signatures are consistent across all tasks.
