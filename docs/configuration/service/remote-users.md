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

```json
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
```

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
