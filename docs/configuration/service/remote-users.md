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
  "node": "",
  "servers": {
    "ss-in": "shadowsocks-inbound-tag"
  }
}
```

## Fields

| Field | Type | Description |
|-------|------|-------------|
| `url` | string (required) | SOT endpoint. The service sends `POST {url}` with body `{"token":...}` and expects `{"users":[{"name":...,"password":...}]}`. `password` is the base64 uPSK. Each user may also carry `up_mbps` and `down_mbps` (see Rate limiting). |
| `token` | string | Bearer-equivalent token sent in the request body. Use a read-only, node-scoped credential. |
| `interval` | duration | Poll interval. Default `1m`. |
| `request_timeout` | duration | Per-request timeout. Default `10s`. |
| `cache_path` | string | Path to the last-good cache file. When set, the cache is applied at startup before the first fetch. |
| `download_detour` | string | Optional outbound tag to route the fetch through. Empty = direct egress. |
| `node` | string | Identifies this node in usage reports. Defaults to the service tag. |
| `servers` | map[string]string | Maps an arbitrary name to a managed inbound tag. Each target inbound must be a shadowsocks multi-user inbound with `"managed": true`. |

## Target inbound

The target shadowsocks inbound must set `"managed": true` and must **not**
declare static `users` (the two are mutually exclusive). A managed inbound
starts with zero users; `remote-users` populates it.

## Traffic accounting

The service installs a tracker on every target inbound, which counts each user's
uplink and downlink bytes plus TCP and UDP session counts. Counters are running
totals, not per-interval deltas, and they are persisted to `cache_path` so they
survive a restart.

On every poll the totals are sent to the SoT:

```
POST {url with the last path segment replaced by "usage/report"}
{
  "token": "<node-token>",
  "node": "<node>",
  "usage": [
    {"name": "alice", "uplink_bytes": 1, "downlink_bytes": 2, "tcp_sessions": 3, "udp_sessions": 0}
  ]
}
```

Totals rather than deltas means a report that fails to arrive is superseded by
the next one: nothing is lost and nothing is counted twice. The POST is skipped
entirely while no counter has moved. A user removed from the SoT keeps their
counters until a report has been acknowledged, so their final bytes are not
lost.

### Alias users

Everything from the first `#` in a user name is an alias suffix, and the name
before it is the identity that is billed and rate limited. A SoT that keeps a
rotated-out credential alive during an overlap window emits it as a second entry
(`alice#prev` alongside `alice`); both share one set of counters and one rate
limit, so a rotation neither splits a user's usage across two rows nor hands
them double their bandwidth while it is in progress.

### Per-site breakdown

Each report also carries a `domains` object mapping a registrable domain to
`[uplink, downlink]`:

```json
{"name": "alice", "uplink_bytes": 1, "downlink_bytes": 2,
 "domains": {"googlevideo.com": [1000, 412000000], "other": [200, 3000]}}
```

Destinations are collapsed to their registrable domain, so the ~30 hostnames a
single video session spreads across (`rr1---sn-….googlevideo.com` and friends)
form one entry rather than thirty — otherwise the heaviest traffic would also be
the most fragmented and would never stand out. Destinations dialled by IP share
one `(ip)` bucket. At most 50 sites per user are reported, picked by bytes, and
a user's table is capped at 200 sites in memory.

`other` holds the sites that did not make the cut **plus** the bytes the
per-site counters never saw: the totals are measured on the outer connection and
so include multiplex framing and handshakes, while the sites are counted on the
routed streams inside it. Folding the difference into `other` is what lets the
sites add up to the user's total, which is how you tell whether the reported
sites are 95% of their traffic or 20% of it.

The site table is restored from `cache_path` alongside the totals, so both
continue across a restart. When a relay upgrades into this feature its cache has
totals but no site table; the totals it restores are recorded as a baseline and
excluded from `other`, since bytes the site counters were never present for are
not theirs to explain — charging them to `other` would bury every real site
under the relay's entire history, permanently.

Unlike the totals, `domains` is a **snapshot** of this relay's counters rather
than something the SoT accumulates. A period reset does not clear it, and losing
a relay's cache restarts it. It is a diagnostic view, not an accounting record.

Every routed connection is also logged with its user:

```
service/remote-users[relay]: [alice] chatgpt.com:443
```

The multiplex log line upstream of this carries the destination but not the
user, and the user's own line carries only the multiplex session, so this is the
one place with both. It is written before the outbound dials, so a line means
the connection was attempted, not that it was established.

!!! warning "Conflicts with ssm-api"

    An inbound holds exactly one tracker. Pointing both this service and an
    [SSM API](./ssm-api.md) service at the same inbound makes whichever starts
    last silently displace the other, and the two also overwrite each other's
    user tables. Use one or the other per inbound.

## Rate limiting

A user entry in the SoT response may carry per-user limits in SI megabits per
second:

```json
{"users": [{"name": "alice", "password": "...", "up_mbps": 50, "down_mbps": 50}]}
```

The limit is per user, not per connection: all of a user's connections share one
budget in each direction. The two directions are independent — setting only
`down_mbps` leaves the uplink unlimited. Omitting a field, or setting it to `0`,
means unlimited.

Changes apply to connections that are already open, without a reconnect. That
includes removing a limit: an in-flight transfer finishes the reservation it has
already made (at most 64 KiB worth) and then runs unthrottled.
