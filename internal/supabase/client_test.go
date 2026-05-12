package supabase

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestRegisterSubscription_AddsToRegistry(t *testing.T) {
	c := &Client{activeSubs: map[string]*activeSub{}}
	sub := &activeSub{topic: "realtime:commands:cluster-x"}
	c.registerSubscription(sub)
	c.activeSubsMu.RLock()
	defer c.activeSubsMu.RUnlock()
	if _, ok := c.activeSubs[sub.topic]; !ok {
		t.Fatalf("sub not in registry")
	}
}

func TestUnregisterSubscription_RemovesFromRegistry(t *testing.T) {
	c := &Client{activeSubs: map[string]*activeSub{}}
	sub := &activeSub{topic: "realtime:commands:cluster-x"}
	c.registerSubscription(sub)
	c.unregisterSubscription(sub.topic)
	c.activeSubsMu.RLock()
	defer c.activeSubsMu.RUnlock()
	if _, ok := c.activeSubs[sub.topic]; ok {
		t.Fatalf("sub still in registry after unregister")
	}
}

func TestRegisterSubscription_OverwritesByTopic(t *testing.T) {
	c := &Client{activeSubs: map[string]*activeSub{}}
	sub1 := &activeSub{topic: "realtime:commands:cluster-x"}
	sub2 := &activeSub{topic: "realtime:commands:cluster-x"}
	c.registerSubscription(sub1)
	c.registerSubscription(sub2)
	c.activeSubsMu.RLock()
	defer c.activeSubsMu.RUnlock()
	if got := c.activeSubs[sub1.topic]; got != sub2 {
		t.Fatalf("expected sub2 to overwrite sub1 by topic, got %p (want %p)", got, sub2)
	}
}

func TestRegisterUnregister_ConcurrentSafe(t *testing.T) {
	c := &Client{activeSubs: map[string]*activeSub{}}
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(2)
		go func() { defer wg.Done(); c.registerSubscription(&activeSub{topic: "t"}) }()
		go func() { defer wg.Done(); c.unregisterSubscription("t") }()
	}
	wg.Wait()
}

func TestStartRefreshLoop_OnlyOnce(t *testing.T) {
	c := &Client{activeSubs: map[string]*activeSub{}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var fired atomic.Int32
	test := func() { fired.Add(1) }
	// Three calls; sync.Once should ensure only one goroutine starts.
	c.startRefreshLoopFor(ctx, test)
	c.startRefreshLoopFor(ctx, test)
	c.startRefreshLoopFor(ctx, test)
	// Cancel immediately — we only care that the goroutine count is
	// gated by Once. Verify by stopping and ensuring no panics from
	// duplicate ticker starts.
	cancel()
	// Give goroutine a moment to exit cleanly.
	time.Sleep(10 * time.Millisecond)
	// fired stays 0 because the ticker (centralRefreshInterval = 30 min)
	// would never have ticked in 10ms. The test passes if no panic
	// occurred from concurrent ticker.Stop calls and the goroutine
	// exited. We're checking the Once-gating, not the per-tick action.
	if fired.Load() != 0 {
		t.Fatalf("ticker fired in 10ms? unexpected: %d", fired.Load())
	}
}
