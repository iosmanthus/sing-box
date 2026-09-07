package remoteusers

import (
	"context"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sagernet/sing-box/adapter"

	"golang.org/x/time/rate"
)

func TestTrackerCountsPerUser(t *testing.T) {
	tr := newTracker(context.Background())

	alice, remote := net.Pipe()
	tracked := tr.TrackConnection(alice, adapter.InboundContext{User: "alice"})
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = io.Copy(io.Discard, remote)
	}()
	if _, err := tracked.Write(make([]byte, 1000)); err != nil {
		t.Fatalf("write: %v", err)
	}
	_ = tracked.Close()
	<-done

	bob, bobRemote := net.Pipe()
	bobTracked := tr.TrackConnection(bob, adapter.InboundContext{User: "bob"})
	go func() { _, _ = bobRemote.Write(make([]byte, 500)) }()
	if _, err := io.ReadFull(bobTracked, make([]byte, 500)); err != nil {
		t.Fatalf("read: %v", err)
	}

	snapshot := tr.Snapshot()
	if len(snapshot) != 2 {
		t.Fatalf("want 2 users, got %d", len(snapshot))
	}
	if snapshot[0].Name != "alice" || snapshot[0].DownlinkBytes != 1000 {
		t.Fatalf("alice: %+v", snapshot[0])
	}
	if snapshot[0].UplinkBytes != 0 {
		t.Fatalf("alice uplink should be 0, got %d", snapshot[0].UplinkBytes)
	}
	if snapshot[1].Name != "bob" || snapshot[1].UplinkBytes != 500 {
		t.Fatalf("bob: %+v", snapshot[1])
	}
	if snapshot[0].TCPSessions != 1 || snapshot[1].TCPSessions != 1 {
		t.Fatalf("session counts: %+v %+v", snapshot[0], snapshot[1])
	}
}

func TestSyncConvertsMbpsAndTreatsZeroAsUnlimited(t *testing.T) {
	tr := newTracker(context.Background())
	tr.Sync([]userEntry{
		{Name: "alice", UpMbps: 8, DownMbps: 80},
		{Name: "bob"},
	})

	alice := tr.state("alice")
	if got := alice.up.Load().Limit(); got != rate.Limit(1000*1000) {
		t.Fatalf("alice up: want 1000000 B/s, got %v", got)
	}
	if got := alice.down.Load().Limit(); got != rate.Limit(10*1000*1000) {
		t.Fatalf("alice down: want 10000000 B/s, got %v", got)
	}
	if bob := tr.state("bob"); bob.up.Load() != nil || bob.down.Load() != nil {
		t.Fatal("bob has no limits set, should be unlimited")
	}
}

func TestSyncUpdatesLimiterInPlace(t *testing.T) {
	tr := newTracker(context.Background())
	tr.Sync([]userEntry{{Name: "alice", DownMbps: 10}})
	before := tr.state("alice").down.Load()

	tr.Sync([]userEntry{{Name: "alice", DownMbps: 20}})
	after := tr.state("alice").down.Load()

	// Same limiter object: connections already holding it follow the new rate.
	if before != after {
		t.Fatal("limiter was replaced; open connections would keep the old rate")
	}
	if got := after.Limit(); got != rate.Limit(20*1000*1000/8) {
		t.Fatalf("want 2500000 B/s, got %v", got)
	}
}

func TestWaitFuncThrottles(t *testing.T) {
	var slot atomic.Pointer[rate.Limiter]
	setLimit(&slot, 200_000) // burst is maxWaitChunk (64 KiB)
	wait := waitFunc(context.Background(), &slot)

	start := time.Now()
	wait(3 * maxWaitChunk)
	elapsed := time.Since(start)

	// One chunk comes out of the burst; the other two are paid for at
	// 200 KB/s, so roughly 0.65s. Assert well below that to tolerate slop.
	if elapsed < 400*time.Millisecond {
		t.Fatalf("transfer was not throttled: %v", elapsed)
	}
}

func TestWaitFuncStopsWhenLimitRemoved(t *testing.T) {
	var slot atomic.Pointer[rate.Limiter]
	setLimit(&slot, 100_000)
	wait := waitFunc(context.Background(), &slot)

	// Drain the burst so the next call has to wait.
	wait(maxWaitChunk)

	done := make(chan time.Duration, 1)
	go func() {
		start := time.Now()
		wait(20 * maxWaitChunk) // ~13s if the limit stayed in force
		done <- time.Since(start)
	}()

	time.Sleep(50 * time.Millisecond)
	setLimit(&slot, 0) // unlimited

	select {
	case elapsed := <-done:
		// It finishes the chunk it already reserved, then returns.
		if elapsed > 3*time.Second {
			t.Fatalf("removal did not take effect promptly: %v", elapsed)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("still throttled after the limit was removed")
	}
}

func TestWaitFuncIsNoOpWhenUnlimited(t *testing.T) {
	var slot atomic.Pointer[rate.Limiter]
	wait := waitFunc(context.Background(), &slot)
	start := time.Now()
	wait(100 * 1024 * 1024)
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Fatalf("unlimited transfer was delayed: %v", elapsed)
	}
}

func TestPruneKeepsLiveUsersOnly(t *testing.T) {
	tr := newTracker(context.Background())
	tr.Sync([]userEntry{{Name: "alice"}, {Name: "bob"}})
	tr.state("alice").uplink.Store(10)
	tr.state("bob").uplink.Store(20)

	tr.Prune([]string{"alice"})

	snapshot := tr.Snapshot()
	if len(snapshot) != 1 || snapshot[0].Name != "alice" {
		t.Fatalf("want only alice, got %+v", snapshot)
	}
}

func TestRestoreContinuesTotals(t *testing.T) {
	tr := newTracker(context.Background())
	tr.Restore([]usageEntry{{Name: "alice", UplinkBytes: 7, DownlinkBytes: 9, TCPSessions: 2}})

	alice, remote := net.Pipe()
	tracked := tr.TrackConnection(alice, adapter.InboundContext{User: "alice"})
	go func() { _, _ = remote.Write(make([]byte, 3)) }()
	if _, err := io.ReadFull(tracked, make([]byte, 3)); err != nil {
		t.Fatalf("read: %v", err)
	}

	snapshot := tr.Snapshot()
	if snapshot[0].UplinkBytes != 10 {
		t.Fatalf("want 7+3=10 uplink, got %d", snapshot[0].UplinkBytes)
	}
	if snapshot[0].DownlinkBytes != 9 {
		t.Fatalf("want restored 9 downlink, got %d", snapshot[0].DownlinkBytes)
	}
	if snapshot[0].TCPSessions != 3 {
		t.Fatalf("want 2+1=3 sessions, got %d", snapshot[0].TCPSessions)
	}
}

func TestAliasesShareOneBudget(t *testing.T) {
	tr := newTracker(context.Background())
	// The SoT emits both entries during a uPSK overlap window.
	tr.Sync([]userEntry{
		{Name: "alice", DownMbps: 10},
		{Name: "alice#prev", DownMbps: 10},
	})

	snapshot := tr.Snapshot()
	if len(snapshot) != 1 || snapshot[0].Name != "alice" {
		t.Fatalf("alias should fold into the principal, got %+v", snapshot)
	}

	// Traffic on the rotated-out credential bills the principal.
	conn, remote := net.Pipe()
	tracked := tr.TrackConnection(conn, adapter.InboundContext{User: "alice#prev"})
	go func() { _, _ = remote.Write(make([]byte, 42)) }()
	if _, err := io.ReadFull(tracked, make([]byte, 42)); err != nil {
		t.Fatalf("read: %v", err)
	}
	if got := tr.Snapshot(); len(got) != 1 || got[0].UplinkBytes != 42 {
		t.Fatalf("want 42 bytes on alice, got %+v", got)
	}

	// And it is the same limiter, so the alias buys no extra bandwidth.
	if tr.state("alice").down.Load() != tr.state("alice#prev").down.Load() {
		t.Fatal("alias has its own limiter; the user would get double their limit")
	}
}

func TestPruneKeepsPrincipalOfLiveAlias(t *testing.T) {
	tr := newTracker(context.Background())
	tr.Sync([]userEntry{{Name: "alice"}})
	tr.state("alice").uplink.Store(5)

	tr.Prune([]string{"alice#prev"})

	if got := tr.Snapshot(); len(got) != 1 {
		t.Fatalf("principal was pruned by its own alias: %+v", got)
	}
}
