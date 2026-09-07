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

	sites := tr.state("alice").domains.snapshot()
	if got := sites["googlevideo.com"][0]; got != 5000 {
		t.Fatalf("the two video hostnames should share one site: got %d, want 5000", got)
	}
	if got := sites["chatgpt.com"][0]; got != 500 {
		t.Fatalf("alice chatgpt: %d", got)
	}
	if got := tr.state("bob").domains.snapshot()["chatgpt.com"][0]; got != 100 {
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

func TestSnapshotKeepsOnlyTheHeaviestSites(t *testing.T) {
	tr := newTracker(context.Background(), log.StdLogger())
	for i := 0; i < reportedDomains+5; i++ {
		transfer(t, tr, md("alice", fmt.Sprintf("site%03d.com", i)), reportedDomains+5-i)
	}

	sites := tr.state("alice").domains.snapshot()
	if len(sites) != reportedDomains {
		t.Fatalf("want %d sites, got %d", reportedDomains, len(sites))
	}
	if _, loaded := sites["site000.com"]; !loaded {
		t.Fatal("the heaviest site was dropped")
	}
	// The five smallest lost their place, and nothing collects them: this is a
	// top-N view of where the traffic went, not an exhaustive ledger.
	if _, loaded := sites[fmt.Sprintf("site%03d.com", reportedDomains+4)]; loaded {
		t.Fatal("the lightest site should not have made the cut")
	}
}

func TestRestoreContinuesCounting(t *testing.T) {
	tr := newTracker(context.Background(), log.StdLogger())
	tr.Restore([]usageEntry{{
		Name:          "alice",
		UplinkBytes:   1000,
		DownlinkBytes: 2000,
		Domains:       map[string][2]int64{"chatgpt.com": {400, 600}},
	}})
	transfer(t, tr, md("alice", "chatgpt.com"), 100)

	if got := tr.Snapshot()[0].Domains["chatgpt.com"]; got != [2]int64{500, 600} {
		t.Fatalf("restored sites should keep counting, got %v", got)
	}
}

func TestTrafficPastTheCapIsNotCounted(t *testing.T) {
	tr := newTracker(context.Background(), log.StdLogger())
	for i := 0; i < maxDomainsPerUser+10; i++ {
		transfer(t, tr, md("alice", fmt.Sprintf("site%04d.com", i)), 10)
	}
	state := tr.state("alice")
	state.domains.access.Lock()
	size := len(state.domains.sites)
	state.domains.access.Unlock()
	if size != maxDomainsPerUser {
		t.Fatalf("site table should stop at the cap, got %d", size)
	}
}
