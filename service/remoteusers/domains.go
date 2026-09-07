package remoteusers

import (
	"context"
	"net"
	"sort"
	"sync"
	"sync/atomic"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-tun"
	"github.com/sagernet/sing/common/bufio"
	N "github.com/sagernet/sing/common/network"

	"golang.org/x/net/publicsuffix"
)

var _ adapter.ConnectionTracker = (*tracker)(nil)

const (
	// maxDomainsPerUser bounds how many distinct sites one user may occupy.
	// Measured on a live relay: ~130 sites across all users in 100 minutes, so
	// this is headroom rather than a limit anyone should reach. Past it,
	// everything folds into otherDomain and the accounting still adds up.
	maxDomainsPerUser = 200

	// otherDomain collects the sites that did not make the report, plus the
	// bytes the per-site counters never saw (mux framing, handshakes). It is
	// what tells you whether the reported sites are 95% of a user's traffic or
	// 20% of it — which is the difference between having found the cause and
	// not.
	otherDomain = "other"

	// reportedDomains is how many sites per user are sent to the SoT. Sites are
	// picked by bytes, so what gets dropped is the long tail of noise.
	reportedDomains = 50
)

type domainCounter struct {
	uplink   atomic.Int64
	downlink atomic.Int64
}

// domainState is the per-user site table. It hangs off userState so that a
// user's site totals and their overall totals share one lifecycle: Prune drops
// both together, and Snapshot can derive "other" from the difference.
type domainState struct {
	access sync.Mutex
	sites  map[string]*domainCounter
	// baseline is the user's total at the moment this table started counting.
	// The two do not always start together: a relay that upgrades into this
	// feature restores its totals from cache with all of their history while
	// the site table begins empty. Without the baseline, the reconciliation
	// below would charge every pre-upgrade byte to "other" — permanently, since
	// the totals keep that history — and the shares that make "other" useful
	// would never converge.
	baseline int64
}

// siteOf collapses a destination to its registrable domain, so that the ~30
// hostnames one video session spreads across (rr1---sn-….googlevideo.com and
// friends) are one line instead of thirty. Measured on a live relay: 441
// hostnames collapse to 129 sites, and 61% of the hostnames belong to just 14
// sites — without this, the heaviest traffic is also the most fragmented and
// never shows up as the cause.
//
// Destinations with no domain (13% of connections are dialled by IP) all share
// one bucket rather than each occupying a slot.
func siteOf(metadata adapter.InboundContext) string {
	domain := metadata.Domain
	if domain == "" {
		domain = metadata.Destination.Fqdn
	}
	if domain == "" {
		return "(ip)"
	}
	site, err := publicsuffix.EffectiveTLDPlusOne(domain)
	if err != nil {
		// Not a registrable name (a bare TLD, or something malformed); keep it
		// verbatim rather than dropping the bytes on the floor.
		return domain
	}
	return site
}

// counter returns the site's counter for this user, creating it if there is
// room. Past maxDomainsPerUser everything shares the "other" counter, so a user
// hitting an unbounded number of hostnames cannot grow this map without limit.
func (d *domainState) counter(site string) *domainCounter {
	d.access.Lock()
	defer d.access.Unlock()
	if d.sites == nil {
		d.sites = make(map[string]*domainCounter)
	}
	if c, loaded := d.sites[site]; loaded {
		return c
	}
	if len(d.sites) >= maxDomainsPerUser {
		site = otherDomain
		if c, loaded := d.sites[site]; loaded {
			return c
		}
	}
	c := new(domainCounter)
	d.sites[site] = c
	return c
}

// snapshot returns the user's top sites by bytes plus an "other" entry holding
// everything else. total is the user's overall byte count, which is measured on
// the outer connection and so also covers what the per-site counters never see;
// the difference lands in "other" so the parts add up to the whole.
func (d *domainState) snapshot(total int64) map[string][2]int64 {
	d.access.Lock()
	total -= d.baseline
	type entry struct {
		site     string
		uplink   int64
		downlink int64
	}
	entries := make([]entry, 0, len(d.sites))
	for site, c := range d.sites {
		if site == otherDomain {
			continue
		}
		entries = append(entries, entry{site, c.uplink.Load(), c.downlink.Load()})
	}
	otherUplink, otherDownlink := int64(0), int64(0)
	if c, loaded := d.sites[otherDomain]; loaded {
		otherUplink, otherDownlink = c.uplink.Load(), c.downlink.Load()
	}
	d.access.Unlock()
	if total < 0 {
		total = 0
	}

	if len(entries) == 0 && otherUplink == 0 && otherDownlink == 0 && total == 0 {
		return nil
	}
	sort.Slice(entries, func(i, j int) bool {
		li := entries[i].uplink + entries[i].downlink
		lj := entries[j].uplink + entries[j].downlink
		if li != lj {
			return li > lj
		}
		return entries[i].site < entries[j].site
	})

	out := make(map[string][2]int64, reportedDomains+1)
	var reported int64
	for i, e := range entries {
		if i >= reportedDomains {
			otherUplink += e.uplink
			otherDownlink += e.downlink
			continue
		}
		out[e.site] = [2]int64{e.uplink, e.downlink}
		reported += e.uplink + e.downlink
	}
	// Whatever the site counters never attributed — mux framing, handshakes,
	// traffic that closed before routing — belongs in "other" too, otherwise
	// the sites would silently fail to add up to the user's total.
	if unattributed := total - reported - otherUplink - otherDownlink; unattributed > 0 {
		otherDownlink += unattributed
	}
	if otherUplink > 0 || otherDownlink > 0 {
		out[otherDomain] = [2]int64{otherUplink, otherDownlink}
	}
	return out
}

// RoutedConnection is called once per routed connection, after sniffing and
// rule matching but before the outbound dials. The outer mux session never
// reaches here — common/mux.Router hands it to the mux service instead — so
// each inner stream is counted and logged exactly once, and connections that
// do not use mux behave identically.
func (t *tracker) RoutedConnection(ctx context.Context, conn net.Conn, metadata adapter.InboundContext, matchedRule adapter.Rule, matchOutbound adapter.Outbound) net.Conn {
	if metadata.User == "" {
		return conn
	}
	// The sing-mux log line carries the destination but not the user, and the
	// user's own line carries only the mux session. This is the one place that
	// has both, which is what makes the logs greppable per user. Note it is
	// logged before the dial, so a line here means "attempted", not "reached".
	t.logger.InfoContext(ctx, "[", metadata.User, "] ", metadata.Destination)
	c := t.domainCounter(metadata.User, siteOf(metadata))
	return bufio.NewCounterConn(conn,
		[]N.CountFunc{addFunc(&c.uplink)},
		[]N.CountFunc{addFunc(&c.downlink)},
	)
}

func (t *tracker) RoutedPacketConnection(ctx context.Context, conn N.PacketConn, metadata adapter.InboundContext, matchedRule adapter.Rule, matchOutbound adapter.Outbound) N.PacketConn {
	if metadata.User == "" {
		return conn
	}
	t.logger.InfoContext(ctx, "[", metadata.User, "] ", metadata.Destination)
	c := t.domainCounter(metadata.User, siteOf(metadata))
	return bufio.NewCounterPacketConn(conn,
		[]N.CountFunc{addFunc(&c.uplink)},
		[]N.CountFunc{addFunc(&c.downlink)},
	)
}

// RoutedFlow is for TUN flow accounting, which a relay never does. Returning
// nil is explicitly supported by the router.
func (t *tracker) RoutedFlow(ctx context.Context, metadata adapter.InboundContext, matchedRule adapter.Rule, matchOutbound adapter.Outbound) tun.FlowTracker {
	return nil
}

func (t *tracker) domainCounter(user string, site string) *domainCounter {
	return t.state(user).domains.counter(site)
}

// restore reloads a cached site table, or — when there is none to reload —
// records where the user's totals already stood so that history the site
// counters were never present for is not charged to "other".
func (d *domainState) restore(sites map[string][2]int64, total int64) {
	d.access.Lock()
	defer d.access.Unlock()
	// "other" is derived at snapshot time, not measured. Restoring it would
	// fold each restart's derived remainder back in as if it had been counted,
	// compounding once per restart until it dwarfs every real site.
	d.sites = make(map[string]*domainCounter, len(sites))
	var measured int64
	for site, v := range sites {
		if site == otherDomain {
			continue
		}
		c := new(domainCounter)
		c.uplink.Store(v[0])
		c.downlink.Store(v[1])
		d.sites[site] = c
		measured += v[0] + v[1]
	}
	// Whatever the restored sites do not account for is history: either from
	// before this feature existed, or unattributed bytes already reported. It
	// is not the new remainder's job to explain it again.
	d.baseline = total - measured
	if d.baseline < 0 {
		d.baseline = 0
	}
}
