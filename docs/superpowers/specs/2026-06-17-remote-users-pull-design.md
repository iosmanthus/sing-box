# Design: `remote_users` — pull-based dynamic user provisioning

**Date:** 2026-06-17
**Branch:** shadowtls-rate-limit
**Status:** Approved (design), pending implementation plan

## Problem

Proxy nodes (sing-box) currently carry their shadowsocks user list statically in
config. We have a source-of-truth (SOT) for users — the `subgen-worker` Cloudflare
Worker — which exposes the full user list at `POST /admin/users`. We want nodes to
become stateless w.r.t. users: pull the list from the SOT at startup and re-poll
periodically (default 1 minute), so that create / rotate / delete on the SOT
propagate to every node without touching node config or restarting.

This is the node side of the SOT's stated "P3: nodes pull `/admin/users` + reload".

## Scope

- **In scope:** shadowsocks **multi-user** inbounds only. These already implement
  `adapter.ManagedSSMServer.UpdateUsers` (`protocol/shadowsocks/inbound_multi.go:125`),
  so no inbound code changes are required.
- **Out of scope:** shadowtls users (stay static in `STATIC_CONFIG`; the shadowtls
  inbound does not implement `ManagedSSMServer`). Any other protocol.
- **Out of scope (YAGNI):** push/webhook for instant revocation. Revocation latency
  of one poll interval is acceptable.

## Why pull (not push)

- The SOT is a Cloudflare Worker; it cannot initiate connections to nodes (nodes sit
  behind arbitrary NAT/firewalls, and the Worker holds no node addresses). Nodes
  initiating outbound HTTPS to `subgen.iosmanthus.com` is the only clean direction.
- A full-list pull + whole-table `UpdateUsers` is an **idempotent full-state
  reconcile**: a missed change self-heals on the next poll; no reliable delivery
  needed.
- There is a near-identical precedent in the codebase to copy:
  `route/rule/rule_set_remote.go` (periodic remote fetch, ETag conditional request,
  on-disk last-good cache, load-cache-on-start). We reuse that lifecycle shape but
  drive `UpdateUsers` instead of rule-set loading.

## Architecture

A new sing-box **service** type `remote_users` (package `service/remoteusers/`),
parallel to `service/ssmapi/` but reversed in direction (pull, not push). It
implements `adapter.Service` (`Start(stage)/Close()`), does **not** touch inbound
code, and feeds users to target inbounds purely via the existing
`adapter.ManagedSSMServer` interface.

Data flow:

```
SOT POST /admin/users
   → service.fetch()                       (HTTPS, body-only token, optional If-None-Match)
   → parse {users:[{name,password}]}
   → for each mapped inbound: UpdateUsers(names, passwords)   (whole-table replace)
   → persist {users, etag, lastUpdated} to cache_path         (new last-good)
```

## Configuration

```jsonc
"services": [{
  "type": "remote_users",
  "url": "https://subgen.iosmanthus.com/admin/users",
  "token": "<node-token>",          // read-only node-scoped token (see Security)
  "interval": "1m",                  // default 1m
  "request_timeout": "10s",          // default 10s
  "cache_path": "users.db",          // last-good cache (the real floor)
  "download_detour": "direct",       // optional: route the fetch through a named outbound
  "servers": {                       // name -> managed inbound tag (same shape as ssmapi)
    "ss-in": "ss-multi-inbound-tag"
  }
}]
```

Target inbound (note: **must be `managed: true` with NO static `users`**):

```jsonc
"inbounds": [{
  "type": "shadowsocks",
  "tag": "ss-multi-inbound-tag",
  "method": "2022-blake3-aes-128-gcm",
  "password": "<server-psk>",        // server PSK stays static (STATIC_CONFIG)
  "managed": true                    // forbids static `users`; starts with zero users
}]
```

Option type (`option/remoteusers.go`):

```go
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

## Components

- `option/remoteusers.go` — `RemoteUsersServiceOptions` (above). Register the service
  option type wherever `SSMAPIServiceOptions` is registered.
- `service/remoteusers/service.go` — the `adapter.Service`. Holds: ctx, http client
  (built against `download_detour` if set), ticker, resolved target
  `[]adapter.ManagedSSMServer` (one per `servers` entry), `adapter.CacheFile`,
  `lastEtag string`, `lastHash [32]byte`.
- Service registration — add `remote_users` to the service registry alongside
  `ssmapi`, and to `include` if a build tag gate is used.

The service resolves each `servers` value (inbound tag) to a `ManagedSSMServer` at
`Start`. If a tag does not resolve to a managed multi-user server, fail fast with a
config error.

## Lifecycle & failure semantics (the core correctness contract)

Modeled on `route/rule/rule_set_remote.go:84` (`StartContext`) / `loopUpdate`.

1. **Start:**
   - Resolve target inbounds → `ManagedSSMServer`; fail fast on bad tag.
   - Load `cache_path` via `adapter.CacheFile`. If a cached list exists, immediately
     `UpdateUsers(cache)` on every target. **This cache is the floor.**
   - Attempt a **synchronous** initial `fetch()`, bounded by `request_timeout`. On
     success, apply + persist. On failure, log an error and continue running on the
     cached (or empty) set — failure is **non-fatal**; never block node startup on the
     SOT being reachable. (This is the one deliberate departure from
     `rule_set_remote`, which treats an initial-fetch failure as fatal.)
   - Start the poll ticker (`interval`, default 1m), with ±jitter to avoid a
     thundering herd across nodes.

2. **Each tick — `fetch()`:**
   - `POST {url}` with JSON body `{"token": "<token>"}`, `Content-Type:
     application/json`, plus `If-None-Match: <lastEtag>` when available.
   - **304 Not Modified** → no-op.
   - **200** → parse `{"users":[{name,password}]}`. Compute a hash of the normalized
     payload; if unchanged from `lastHash`, no-op (avoids needless reload / connection
     churn). If changed: `UpdateUsers(names, passwords)` on every target, then persist
     `{users, etag, lastUpdated}` and update `lastHash`/`lastEtag`.
   - **Any failure** (network error, non-2xx/304, malformed JSON, decode error from
     `UpdateUsersWithPasswords`) → log an error and **keep the current user table**.
     Never call `UpdateUsers([])` over a non-empty last-good. Wait for the next tick.

3. **Close:** stop the ticker, cancel in-flight fetch.

### Why the floor is the cache, not a static user

Confirmed against the code:

- `protocol/shadowsocks/inbound.go:35-36`: a `managed: true` inbound that also
  specifies `users` is a **hard config error** (`"users and destinations options are
  not supported in managed servers"`). So a managed inbound *cannot* carry a static
  floor user.
- `protocol/shadowsocks/inbound.go:38`: `managed: true` with zero users still routes
  to `MultiInbound`, and `inbound_multi.go:86` only initializes the table when
  `len(users) > 0` — so a managed inbound **starts cleanly with zero users** (listens
  fine; nobody can complete a handshake until `UpdateUsers` populates it).
- `sing-shadowsocks@v0.2.8/shadowaead_2022/service_multi.go:97-99`: `UpdateUsers`
  builds fresh `uPSK`/`uPSKHash`/`uCipher` maps and assigns them wholesale — it is a
  **full replace, not a merge**. A static floor user (if it could exist) would be
  wiped on the first successful poll anyway.

Therefore the on-disk last-good cache is the only durable floor. Cold start + no
cache + SOT unreachable = zero users (process healthy, no one can connect, error
logged), recovering on the next successful poll. This mirrors the SOT's own
"floor may be empty, degradation is visible, never throw" model.

## Protocol contract with the SOT

- **Request:** `POST /admin/users`, body `{"token": "<node-token>"}` (body-only token,
  matching `subgen-worker/src/handler.ts:82` which verifies via SHA-256).
- **Response 200:** `{"users":[{"name":string,"password":string}]}`
  (`listNodeUsers`, `subgen-worker/src/user-service.ts:56`).
- **`password` = base64-encoded uPSK.** `UpdateUsersWithPasswords`
  (`shadowaead_2022/service_multi.go:103-115`) `base64.StdEncoding.DecodeString`s each
  password and rejects empty ones. The SOT's `ss_key` (the decrypted `password`) must
  already be base64 of the correct key length for the configured method
  (e.g. 16 bytes for `2022-blake3-aes-128-gcm`). **Verify `generateSsKey` output
  format aligns** before relying on this end-to-end.
- **ETag (optional SOT-side optimization):** if the SOT emits an `ETag` for the user
  list and honors `If-None-Match`, polls become cheap and the SOT skips per-user AES
  decryption on unchanged lists. If absent, the node falls back to local payload
  hashing (the 200 path above) — no SOT change required for correctness, only for
  efficiency.

## Security

- **Read-only node-scoped token.** The SOT gains a new auth scope that may only *list*
  users (`POST /admin/users`), and is rejected for `create`/`rotate`/`delete`. Nodes
  carry only this token, so a compromised node cannot mutate the user table. (SOT-side
  change in `subgen-worker`; out of scope for the sing-box implementation but a
  prerequisite for deployment.)
- **Token storage:** lives in the sing-box config; protect via file permissions. TLS
  is HTTPS to the SOT; the fetch may be pinned to a `download_detour` outbound so it
  does not loop back through the proxy being configured.
- **Single writer.** `remote_users` is the sole writer of the target inbound's user
  table. Do **not** also point an `ssmapi` push service at the same inbound — they
  would fight over the table. `ssmapi`, if used, should be read-only (stats) for these
  inbounds.

## Jitter & scale

- Poll interval default 1m; add ±jitter (e.g. up to 10% of interval) so multiple nodes
  don't hit the SOT in lockstep.
- ETag/304 (or local hash no-op) keeps unchanged polls cheap on both ends.

## Testing

- **Unit (httptest SOT):**
  - 200 with users → `UpdateUsers` called with the right names/passwords.
  - 500 / timeout / malformed JSON / bad base64 → last-good preserved, no
    `UpdateUsers([])`.
  - Start with a populated cache → `UpdateUsers(cache)` applied before first fetch.
  - 304 → no-op; unchanged 200 payload → no-op (hash compare).
- **Integration:** real shadowsocks multi-user inbound (`managed: true`, zero users)
  + `remote_users` pointed at a fake SOT. Assert: users appear after first poll; a
  user removed from the SOT disappears after the next poll; SOT going down does **not**
  clear the table.
- **Cold-start floor:** start with no cache + unreachable SOT → service starts, zero
  users, error logged, process healthy.

## Open items to verify during implementation

1. `managed: true` without any `ssmapi` service configured constructs and runs cleanly
   (the `managed` flag is expected to be a marker only; confirm with a minimal config).
2. `generateSsKey` (SOT) output is base64 of the exact key length for the deployed
   shadowsocks method, so `password` feeds `UpdateUsersWithPasswords` without error.
3. How the service option type is registered and how `download_detour` resolves an
   outbound for the fetch (follow `rule_set_remote`'s `download_detour` handling).
