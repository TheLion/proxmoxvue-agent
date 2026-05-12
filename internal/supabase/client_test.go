package supabase

import (
	"sync"
	"sync/atomic"
	"testing"
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

// Sentinel to keep atomic import alive when StartRefreshLoop test is added in Task 2.
var _ = atomic.LoadInt32
