package remoteusers

import (
	"context"
	"fmt"
	"io"
	"net"
	"testing"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/log"
	M "github.com/sagernet/sing/common/metadata"
)

func md(user, domain string) adapter.InboundContext {
	return adapter.InboundContext{
		User:        user,
		Domain:      domain,
		Destination: M.ParseSocksaddrHostPort(domain, 443),
	}
}

func TestSiteOfCollapsesHostnames(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		// The 30-odd hostnames one video session spreads across must land on
		// one site, or the heaviest traffic is also the most fragmented.
		{"rr1---sn-4g5e6nez.googlevideo.com", "googlevideo.com"},
		{"rr2---sn-4g5ednse.googlevideo.com", "googlevideo.com"},
		{"ab.chatgpt.com", "chatgpt.com"},
		{"chatgpt.com", "chatgpt.com"},
		// Multi-part public suffixes are the reason for using publicsuffix
		// rather than "last two labels".
		{"www.example.co.uk", "example.co.uk"},
		{"cdn.example.com.cn", "example.com.cn"},
	} {
		if got := siteOf(md("u", tc.in)); got != tc.want {
			t.Fatalf("siteOf(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestSiteOfBucketsAddressesWithoutDomain(t *testing.T) {
	metadata := adapter.InboundContext{User: "u", Destination: M.ParseSocksaddr("1.2.3.4:443")}
	if got := siteOf(metadata); got != "(ip)" {
		t.Fatalf("want a single bucket for IP destinations, got %q", got)
	}
	// Sniffing fills Domain even when the destination is an address.
	metadata.Domain = "example.com"
	if got := siteOf(metadata); got != "example.com" {
		t.Fatalf("sniffed domain should win, got %q", got)
	}
}

// transfer drives n bytes through a routed connection so the site counters see
// real traffic rather than being written directly.
func transfer(t *testing.T, tr *tracker, metadata adapter.InboundContext, n int) {
	t.Helper()
	local, remote := net.Pipe()
	tracked := tr.RoutedConnection(context.Background(), local, metadata, nil, nil)
	go func() { _, _ = remote.Write(make([]byte, n)) }()
	if _, err := io.ReadFull(tracked, make([]byte, n)); err != nil {
		t.Fatalf("read: %v", err)
	}
}

func TestRoutedConnectionCountsPerSite(t *testing.T) {
	tr := newTracker(context.Background(), log.StdLogger())

	transfer(t, tr, md("alice", "rr1---sn-abc.googlevideo.com"), 3000)
	transfer(t, tr, md("alice", "rr2---sn-def.googlevideo.com"), 2000)
	transfer(t, tr, md("alice", "chatgpt.com"), 500)
	transfer(t, tr, md("bob", "chatgpt.com"), 100)

	sites := tr.state("alice").domains.snapshot(0)
	if got := sites["googlevideo.com"][0]; got != 5000 {
		t.Fatalf("the two video hostnames should share one site: got %d, want 5000", got)
	}
	if got := sites["chatgpt.com"][0]; got != 500 {
		t.Fatalf("alice chatgpt: %d", got)
	}
	if got := tr.state("bob").domains.snapshot(0)["chatgpt.com"][0]; got != 100 {
		t.Fatalf("bob's traffic leaked into alice's table or vice versa: %d", got)
	}
}

func TestRoutedConnectionSkipsUnattributedTraffic(t *testing.T) {
	tr := newTracker(context.Background(), log.StdLogger())
	local, remote := net.Pipe()
	defer remote.Close()
	// No user: not ours to account for, and wrapping would cost for nothing.
	if got := tr.RoutedConnection(context.Background(), local, md("", "example.com"), nil, nil); got != local {
		t.Fatal("connection without a user should be returned unwrapped")
	}
	if len(tr.Snapshot()) != 0 {
		t.Fatal("an anonymous connection created user state")
	}
}

func TestSnapshotPutsTheTailInOther(t *testing.T) {
	tr := newTracker(context.Background(), log.StdLogger())
	// One site per reported slot, plus two more that must not get their own.
	for i := 0; i < reportedDomains+2; i++ {
		transfer(t, tr, md("alice", fmt.Sprintf("site%03d.com", i)), reportedDomains+2-i)
	}

	sites := tr.state("alice").domains.snapshot(0)
	if len(sites) != reportedDomains+1 {
		t.Fatalf("want %d sites plus other, got %d", reportedDomains, len(sites))
	}
	if _, loaded := sites[otherDomain]; !loaded {
		t.Fatal("the sites that did not fit should be in other")
	}
	// The two smallest are the ones that lost their slot: 1 + 2 bytes.
	if got := sites[otherDomain][0]; got != 3 {
		t.Fatalf("other should hold the tail's bytes, got %d", got)
	}
	// And the biggest kept its own line.
	if _, loaded := sites["site000.com"]; !loaded {
		t.Fatal("the heaviest site was dropped")
	}
}

func TestSnapshotReconcilesWithTheUserTotal(t *testing.T) {
	tr := newTracker(context.Background(), log.StdLogger())
	transfer(t, tr, md("alice", "chatgpt.com"), 1000)

	// The user total is measured on the outer connection, so it also covers mux
	// framing that the per-site counters never see. That difference has to land
	// somewhere or the sites silently fail to add up.
	sites := tr.state("alice").domains.snapshot(1200)
	var sum int64
	for _, v := range sites {
		sum += v[0] + v[1]
	}
	if sum != 1200 {
		t.Fatalf("sites plus other should equal the user total 1200, got %d", sum)
	}
	if got := sites[otherDomain][1]; got != 200 {
		t.Fatalf("the unattributed 200 bytes should be in other, got %d", got)
	}
}

func TestDomainTableIsBounded(t *testing.T) {
	tr := newTracker(context.Background(), log.StdLogger())
	for i := 0; i < maxDomainsPerUser+50; i++ {
		transfer(t, tr, md("alice", fmt.Sprintf("site%04d.com", i)), 10)
	}
	state := tr.state("alice")
	state.domains.access.Lock()
	size := len(state.domains.sites)
	state.domains.access.Unlock()
	if size > maxDomainsPerUser+1 { // +1 for the other bucket itself
		t.Fatalf("site table grew unbounded: %d entries", size)
	}
}

func TestSnapshotRidesAlongWithTheUsageReport(t *testing.T) {
	tr := newTracker(context.Background(), log.StdLogger())
	// TrackConnection is the inbound (outer) path that produces the totals.
	outer, outerRemote := net.Pipe()
	tracked := tr.TrackConnection(outer, adapter.InboundContext{User: "alice"})
	go func() { _, _ = outerRemote.Write(make([]byte, 900)) }()
	if _, err := io.ReadFull(tracked, make([]byte, 900)); err != nil {
		t.Fatalf("read: %v", err)
	}
	transfer(t, tr, md("alice", "chatgpt.com"), 700)

	usage := tr.Snapshot()
	if len(usage) != 1 || usage[0].Name != "alice" {
		t.Fatalf("usage: %+v", usage)
	}
	if usage[0].UplinkBytes != 900 {
		t.Fatalf("totals come from the outer connection: got %d", usage[0].UplinkBytes)
	}
	if usage[0].Domains["chatgpt.com"][0] != 700 {
		t.Fatalf("per-site breakdown missing from the report: %+v", usage[0].Domains)
	}
	if usage[0].Domains[otherDomain][1] != 200 {
		t.Fatalf("the 200 bytes of framing should be in other: %+v", usage[0].Domains)
	}
}

func TestPruneDropsSitesWithTheUser(t *testing.T) {
	tr := newTracker(context.Background(), log.StdLogger())
	transfer(t, tr, md("alice", "chatgpt.com"), 100)
	transfer(t, tr, md("bob", "chatgpt.com"), 100)

	tr.Prune([]string{"alice"})

	usage := tr.Snapshot()
	if len(usage) != 1 || usage[0].Name != "alice" {
		t.Fatalf("want only alice, got %+v", usage)
	}
	if usage[0].Domains["chatgpt.com"][0] != 100 {
		t.Fatal("alice's sites were dropped along with bob's")
	}
}

func TestRestoreWithoutSitesDoesNotChargeHistoryToOther(t *testing.T) {
	tr := newTracker(context.Background(), log.StdLogger())
	// A relay upgrading into this feature: its cache carries totals with all
	// their history, but no site table.
	tr.Restore([]usageEntry{{Name: "alice", UplinkBytes: 1_000_000, DownlinkBytes: 2_000_000}})
	transfer(t, tr, md("alice", "chatgpt.com"), 400)

	usage := tr.Snapshot()
	if got := usage[0].Domains["chatgpt.com"][0]; got != 400 {
		t.Fatalf("site bytes: %d", got)
	}
	// The 3 MB of history predates the site counters, so it is not theirs to
	// explain. Charging it to "other" would bury every real site under it.
	if other, loaded := usage[0].Domains[otherDomain]; loaded {
		t.Fatalf("history was charged to other: %v", other)
	}
}

func TestRestoreWithSitesContinuesCounting(t *testing.T) {
	tr := newTracker(context.Background(), log.StdLogger())
	tr.Restore([]usageEntry{{
		Name:          "alice",
		UplinkBytes:   1000,
		DownlinkBytes: 2000,
		Domains:       map[string][2]int64{"chatgpt.com": {400, 600}},
	}})
	transfer(t, tr, md("alice", "chatgpt.com"), 100)

	usage := tr.Snapshot()
	if got := usage[0].Domains["chatgpt.com"]; got != [2]int64{500, 600} {
		t.Fatalf("restored sites should keep counting, got %v", got)
	}
	// The restored sites accounted for 1000 of the 3000; the rest is history
	// already reported, so the new remainder starts at zero and only the 100
	// bytes transferred since are unexplained — and those went to a site.
	if other, loaded := usage[0].Domains[otherDomain]; loaded {
		t.Fatalf("history was re-explained as other: %v", other)
	}
}

func TestSnapshotToleratesTotalsBelowSites(t *testing.T) {
	tr := newTracker(context.Background(), log.StdLogger())
	transfer(t, tr, md("alice", "chatgpt.com"), 5000)
	// A period reset zeroes the SoT's totals but not the relay's site table.
	sites := tr.state("alice").domains.snapshot(10)
	if _, loaded := sites[otherDomain]; loaded {
		t.Fatal("a total below the site sum must not invent an other entry")
	}
	if got := sites["chatgpt.com"][0]; got != 5000 {
		t.Fatalf("sites should be reported as measured, got %d", got)
	}
}

func TestRestoreDropsTheDerivedOther(t *testing.T) {
	tr := newTracker(context.Background(), log.StdLogger())
	// A cache written before this fix: "other" was persisted as though it had
	// been measured. Restoring it would compound once per restart.
	tr.Restore([]usageEntry{{
		Name:          "alice",
		UplinkBytes:   0,
		DownlinkBytes: 3_000_000,
		Domains: map[string][2]int64{
			"chatgpt.com": {0, 1000},
			otherDomain:   {0, 2_900_000},
		},
	}})
	transfer(t, tr, md("alice", "chatgpt.com"), 500)

	usage := tr.Snapshot()
	if got := usage[0].Domains["chatgpt.com"]; got != [2]int64{500, 1000} {
		t.Fatalf("real sites should survive the restore, got %v", got)
	}
	if other, loaded := usage[0].Domains[otherDomain]; loaded {
		t.Fatalf("the stale derived other came back: %v", other)
	}
}
