package checker

import (
	"fmt"
	"net"
	"testing"
	"time"
)

func TestMTRCheckerTryAcquire(t *testing.T) {
	c := NewMTRChecker(30, 1*time.Second, 5*time.Second)

	// First call: should succeed and record the time.
	if !c.TryAcquire("node-1", "node-2") {
		t.Error("expected TryAcquire=true on first call")
	}

	// Immediate second call: cooldown not elapsed.
	if c.TryAcquire("node-1", "node-2") {
		t.Error("expected TryAcquire=false within cooldown")
	}

	// Different destination: independent cooldown, should succeed.
	if !c.TryAcquire("node-1", "node-3") {
		t.Error("expected TryAcquire=true for different pair")
	}
}

func TestMTRCheckerTryAcquireCooldownExpired(t *testing.T) {
	c := NewMTRChecker(30, 1*time.Second, 100*time.Millisecond)

	// Seed an old entry so the cooldown is already expired.
	c.mu.Lock()
	c.lastRun["node-1->node-2"] = time.Now().Add(-200 * time.Millisecond)
	c.mu.Unlock()

	if !c.TryAcquire("node-1", "node-2") {
		t.Error("expected TryAcquire=true after cooldown expired")
	}
}

func TestMTRCheckerTryAcquireAtomicRecord(t *testing.T) {
	c := NewMTRChecker(30, 1*time.Second, 1*time.Second)

	// TryAcquire must record the run so that a subsequent ShouldRun-like
	// check immediately blocks without a separate MarkRun step.
	if !c.TryAcquire("src", "dst") {
		t.Fatal("expected first TryAcquire=true")
	}

	c.mu.Lock()
	_, recorded := c.lastRun["src->dst"]
	c.mu.Unlock()

	if !recorded {
		t.Error("TryAcquire must record the key atomically")
	}
}

func TestMTRCheckerExpiredEntriesPurged(t *testing.T) {
	c := NewMTRChecker(30, 1*time.Second, 100*time.Millisecond)

	// Seed several expired entries directly.
	c.mu.Lock()
	for i := range 5 {
		key := fmt.Sprintf("node-%d->node-x", i)
		c.lastRun[key] = time.Now().Add(-200 * time.Millisecond)
	}
	c.mu.Unlock()

	// TryAcquire for a new pair triggers cleanup of the expired entries.
	if !c.TryAcquire("node-new", "node-x") {
		t.Fatal("expected TryAcquire=true for new pair")
	}

	c.mu.Lock()
	remaining := len(c.lastRun)
	c.mu.Unlock()

	// Only the newly recorded entry should remain.
	if remaining != 1 {
		t.Errorf("expected 1 entry after purge, got %d", remaining)
	}
}

// MTR was the ONE checker that never filled CheckResult.Duration, so every
// successful trace persisted durationNs 0 while a FAILED one showed 2-3ms --
// the console's own wall clock, filled in by the runner's error branch. A run
// where the failures are the only rows with a duration is backwards.
func TestMTRCheckerFillsDurationOnEveryPath(t *testing.T) {
	c := NewMTRChecker(30, 1*time.Second, 0)

	// The invalid-IP path returns before any packet is sent and is the one
	// failure this test can produce without a network.
	got := c.Check(t.Context(), Target{PodIP: "not-an-ip", NodeName: "node-b"})
	if got.Success {
		t.Fatalf("Check(invalid IP) succeeded, want a failure: %+v", got)
	}
	if got.Duration <= 0 {
		t.Errorf("Duration = %s, want the time actually spent (> 0)", got.Duration)
	}
}

func TestHopIPFromAddrStripsPort(t *testing.T) {
	cases := []struct {
		name string
		addr net.Addr
		want string
	}{
		{"IPAddr v4", &net.IPAddr{IP: net.ParseIP("10.244.1.11")}, "10.244.1.11"},
		{"IPAddr v6", &net.IPAddr{IP: net.ParseIP("fe80::1")}, "fe80::1"},
		{"UDPAddr with port", &net.UDPAddr{IP: net.ParseIP("10.244.1.11"), Port: 0}, "10.244.1.11"},
		{"UDPAddr v6 with port", &net.UDPAddr{IP: net.ParseIP("fe80::1"), Port: 12345}, "fe80::1"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := hopIPFromAddr(tc.addr)
			if got != tc.want {
				t.Errorf("hopIPFromAddr(%v) = %q, want %q", tc.addr, got, tc.want)
			}
			// A "host:port" string (as produced by UDPAddr.String()) must not
			// survive extraction; a bare IPv6 address is not itself a valid
			// "host:port" pair, so SplitHostPort must fail on the result.
			if _, _, err := net.SplitHostPort(got); err == nil {
				t.Errorf("hopIPFromAddr(%v) = %q, still looks like host:port", tc.addr, got)
			}
		})
	}
}
