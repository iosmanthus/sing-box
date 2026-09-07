package remoteusers

import (
	"context"
	"net"
	"sort"
	"sync"
	"sync/atomic"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing/common/bufio"
	N "github.com/sagernet/sing/common/network"

	"golang.org/x/time/rate"
)

var _ adapter.SSMTracker = (*tracker)(nil)

const (
	// bitsPerMbit is the SI megabit used by the up_mbps/down_mbps fields, kept
	// identical to the shadowtls v3_flow_control conversion so the two features
	// cannot disagree about what "50 mbps" means.
	bitsPerMbit = 1000 * 1000

	// maxWaitChunk bounds how many bytes a single rate.Limiter reservation may
	// cover. It caps how long a transfer stays parked in WaitN, which is what
	// makes a limit change (including its removal) take effect promptly on
	// connections that are already running.
	maxWaitChunk = 64 * 1024
)

// userState holds one user's cumulative counters and its current limiters.
//
// The limiters are stored as atomic pointers rather than being captured by the
// connection wrappers: every transferred chunk re-reads them, so raising,
// lowering or removing a limit applies to connections that are already open.
// A nil pointer means unlimited and costs one atomic load per chunk.
type userState struct {
	uplink      atomic.Int64
	downlink    atomic.Int64
	tcpSessions atomic.Int64
	udpSessions atomic.Int64

	up   atomic.Pointer[rate.Limiter]
	down atomic.Pointer[rate.Limiter]

	// domains is the per-site breakdown behind the totals above. See domains.go.
	domains domainState
}

// setLimit installs, updates or removes one direction's limiter.
// bytesPerSecond <= 0 means unlimited.
func setLimit(slot *atomic.Pointer[rate.Limiter], bytesPerSecond int64) {
	if bytesPerSecond <= 0 {
		slot.Store(nil)
		return
	}
	burst := int(bytesPerSecond / 10)
	if burst < maxWaitChunk {
		burst = maxWaitChunk
	}
	if current := slot.Load(); current != nil {
		// Update in place so open connections holding this pointer follow the
		// new rate without reconnecting.
		current.SetLimit(rate.Limit(bytesPerSecond))
		current.SetBurst(burst)
		return
	}
	slot.Store(rate.NewLimiter(rate.Limit(bytesPerSecond), burst))
}

// waitFunc returns an N.CountFunc that throttles to the limiter currently in
// slot. Expressing the limiter as a count function (rather than as another
// net.Conn wrapper) means it keeps working on sing's splice and vectorised
// fast paths, which bypass Read/Write but still invoke every count function.
func waitFunc(ctx context.Context, slot *atomic.Pointer[rate.Limiter]) N.CountFunc {
	return func(n int64) {
		if n <= 0 {
			return
		}
		for n > 0 {
			limiter := slot.Load()
			if limiter == nil {
				return
			}
			chunk := n
			if chunk > maxWaitChunk {
				chunk = maxWaitChunk
			}
			if limiter.WaitN(ctx, int(chunk)) != nil {
				// Shutting down, or the reservation could not be granted.
				// Never fail the transfer over accounting.
				return
			}
			n -= chunk
		}
	}
}

func addFunc(counter *atomic.Int64) N.CountFunc {
	return func(n int64) {
		counter.Add(n)
	}
}

// tracker counts per-user traffic and enforces per-user rate limits. It is
// installed on every managed inbound via adapter.ManagedSSMServer.SetTracker
// and is otherwise passive: it performs no I/O and never calls back into the
// service. The service reads it (Snapshot) and writes it (Sync, Restore,
// Prune).
//
// One tracker instance is shared by all of a service's target inbounds, so a
// user's counters are their total across inbounds.
type tracker struct {
	ctx    context.Context
	logger log.ContextLogger
	access sync.RWMutex
	users  map[string]*userState
}

// accountOf maps an inbound user name to the identity that is billed and rate
// limited. Everything from the first '#' is an alias suffix: the SoT emits
// aliases such as "alice#prev" so a rotated credential keeps working during its
// overlap window, and both must share one budget — otherwise a user gets double
// their limit for the length of every rotation. Real names cannot contain '#'.
func accountOf(user string) string {
	for i := 0; i < len(user); i++ {
		if user[i] == '#' {
			return user[:i]
		}
	}
	return user
}

func newTracker(ctx context.Context, logger log.ContextLogger) *tracker {
	return &tracker{
		ctx:    ctx,
		logger: logger,
		users:  make(map[string]*userState),
	}
}

// state returns the user's state, creating it if absent. Sync runs before the
// inbound accepts a user, so the create path is only a safety net against
// traffic for a user the service has not seen; without it the choice would be
// between panicking and silently dropping the usage.
func (t *tracker) state(user string) *userState {
	user = accountOf(user)
	t.access.RLock()
	st, loaded := t.users[user]
	t.access.RUnlock()
	if loaded {
		return st
	}
	t.access.Lock()
	defer t.access.Unlock()
	if st, loaded = t.users[user]; loaded {
		return st
	}
	st = new(userState)
	t.users[user] = st
	return st
}

func (t *tracker) TrackConnection(conn net.Conn, metadata adapter.InboundContext) net.Conn {
	st := t.state(metadata.User)
	st.tcpSessions.Add(1)
	return bufio.NewCounterConn(conn,
		[]N.CountFunc{addFunc(&st.uplink), waitFunc(t.ctx, &st.up)},
		[]N.CountFunc{addFunc(&st.downlink), waitFunc(t.ctx, &st.down)},
	)
}

func (t *tracker) TrackPacketConnection(conn N.PacketConn, metadata adapter.InboundContext) N.PacketConn {
	st := t.state(metadata.User)
	st.udpSessions.Add(1)
	return bufio.NewCounterPacketConn(conn,
		[]N.CountFunc{addFunc(&st.uplink), waitFunc(t.ctx, &st.up)},
		[]N.CountFunc{addFunc(&st.downlink), waitFunc(t.ctx, &st.down)},
	)
}

// Sync creates state for new users and applies each user's current limits. It
// does not remove anyone: a user dropped from the SoT keeps their counters
// until Prune runs, so their final usage still gets reported.
func (t *tracker) Sync(users []userEntry) {
	for _, u := range users {
		st := t.state(u.Name)
		setLimit(&st.up, int64(u.UpMbps)*bitsPerMbit/8)
		setLimit(&st.down, int64(u.DownMbps)*bitsPerMbit/8)
	}
}

// Prune drops state for users that are absent from live. Call it only after a
// successful report, otherwise a removed user's last bytes are lost.
func (t *tracker) Prune(live []string) {
	keep := make(map[string]bool, len(live))
	for _, name := range live {
		keep[accountOf(name)] = true
	}
	t.access.Lock()
	defer t.access.Unlock()
	for name := range t.users {
		if !keep[name] {
			delete(t.users, name)
		}
	}
}

// Snapshot reads every user's cumulative counters, sorted by name. Counters are
// never cleared: the report carries running totals so that a lost report is
// made up by the next one instead of being double counted or dropped.
func (t *tracker) Snapshot() []usageEntry {
	t.access.RLock()
	defer t.access.RUnlock()
	out := make([]usageEntry, 0, len(t.users))
	for name, st := range t.users {
		uplink, downlink := st.uplink.Load(), st.downlink.Load()
		out = append(out, usageEntry{
			Name:          name,
			UplinkBytes:   uplink,
			DownlinkBytes: downlink,
			TCPSessions:   st.tcpSessions.Load(),
			UDPSessions:   st.udpSessions.Load(),
			Domains:       st.domains.snapshot(uplink + downlink),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Restore seeds the counters from the on-disk cache at startup so the running
// totals continue across a restart and the SoT never sees them go backwards.
func (t *tracker) Restore(usage []usageEntry) {
	for _, u := range usage {
		st := t.state(u.Name)
		st.uplink.Store(u.UplinkBytes)
		st.downlink.Store(u.DownlinkBytes)
		st.tcpSessions.Store(u.TCPSessions)
		st.udpSessions.Store(u.UDPSessions)
		st.domains.restore(u.Domains, u.UplinkBytes+u.DownlinkBytes)
	}
}
