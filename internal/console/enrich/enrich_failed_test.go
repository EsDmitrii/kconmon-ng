package enrich

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/EsDmitrii/kconmon-ng/internal/console/config"
)

// A failed PTR lookup is remembered in memory for failedLookupRetry (never past the TTL) and never
// written to the cache table.

// With rDNS the only source a failed lookup leaves nothing to say about the hop: views inside the
// retry window do not wait on DNS again, and the first view after it asks again.
func TestResolveRDNSOnlyErrorIsNotCachedForTheTTL(t *testing.T) {
	cache := newFakeCache()
	rdns := &countingRDNS{err: errors.New("read udp 10.0.0.1:53: i/o timeout")}
	r := newTestResolver(t, enabledConfig(), cache, rdns.lookup, testMetrics(t))
	now := time.Now()
	r.now = func() time.Time { return now }

	r.Resolve(t.Context(), []string{"203.0.113.9"})
	if rows := cache.written(); len(rows) != 0 {
		t.Fatalf("written rows after a failed rdns lookup = %+v, want none: it is remembered in memory", rows)
	}

	r.now = func() time.Time { return now.Add(time.Minute) }
	r.Resolve(t.Context(), []string{"203.0.113.9"})
	if n := rdns.calls.Load(); n != 1 {
		t.Errorf("rdns lookups for two views a minute apart = %d, want 1", n)
	}

	rdns.err = nil
	rdns.answers = map[string][]string{"203.0.113.9": {"edge.example."}}
	r.now = func() time.Time { return now.Add(failedLookupRetry + time.Second) }
	got := r.Resolve(t.Context(), []string{"203.0.113.9"})
	if got["203.0.113.9"].RDNS != "edge.example" {
		t.Errorf("rdns after DNS recovered = %q, want edge.example", got["203.0.113.9"].RDNS)
	}
	if n := rdns.calls.Load(); n != 2 {
		t.Errorf("rdns lookups = %d, want 2: the failure must not have been cached", n)
	}
}

func TestResolveRDNSFailureNextToGeoIPIsRetriedAfterTheShortWindow(t *testing.T) {
	cfg := enabledConfig() // ttl 1h
	cfg.RDNS.TimeoutMs = 20
	cfg.GeoIP = config.GeoIPConfig{ASNPath: asnFixture}
	cache := newFakeCache()
	rdns := &countingRDNS{hang: true}
	r := newTestResolver(t, cfg, cache, rdns.lookup, testMetrics(t))
	now := time.Now()
	r.now = func() time.Time { return now }

	got := r.Resolve(t.Context(), []string{"192.0.2.1"})
	if row := got["192.0.2.1"]; row.ASN != 64496 || row.RDNS != "" || !row.ResolvedAt.Equal(now) {
		t.Fatalf("first answer = %+v, want asn 64496, no rdns, resolved now", row)
	}

	rdns.hang = false
	rdns.answers = map[string][]string{"192.0.2.1": {"edge.example."}}

	r.now = func() time.Time { return now.Add(time.Minute) }
	r.Resolve(t.Context(), []string{"192.0.2.1"})
	if n := rdns.calls.Load(); n != 1 {
		t.Errorf("rdns lookups inside the retry window = %d, want 1: the failure is still remembered", n)
	}

	r.now = func() time.Time { return now.Add(failedLookupRetry + time.Second) }
	got = r.Resolve(t.Context(), []string{"192.0.2.1"})
	if got["192.0.2.1"].RDNS != "edge.example" {
		t.Errorf("rdns after the retry window = %q, want edge.example; the failure outlived its retry window",
			got["192.0.2.1"].RDNS)
	}
	if got["192.0.2.1"].ASN != 64496 {
		t.Errorf("asn after the retry = %d, want 64496", got["192.0.2.1"].ASN)
	}
}

// A later view of a failed lookup shows when the lookup was made, not when the view was: resolvedAt
// is what GET /api/v1/mtr/snapshots/{id}?enrich=true shows for a hop.
func TestResolveKeepsTheRealResolveTimeOfAFailedLookup(t *testing.T) {
	cfg := enabledConfig() // ttl 1h
	cfg.GeoIP = config.GeoIPConfig{ASNPath: asnFixture}
	rdns := &countingRDNS{err: errors.New("read udp 10.0.0.1:53: i/o timeout")}
	r := newTestResolver(t, cfg, newFakeCache(), rdns.lookup, testMetrics(t))
	now := time.Now()
	r.now = func() time.Time { return now }

	r.Resolve(t.Context(), []string{"192.0.2.1"})
	r.now = func() time.Time { return now.Add(time.Minute) }
	got := r.Resolve(t.Context(), []string{"192.0.2.1"})
	if row := got["192.0.2.1"]; !row.ResolvedAt.Equal(now) || row.ASN != 64496 {
		t.Errorf("a later view = %+v, want the lookup's own resolvedAt %v and asn 64496", row, now)
	}
}

// A TTL shorter than the retry window is not stretched by it.
func TestResolveRetryWindowNeverOutlivesTheTTL(t *testing.T) {
	cfg := enabledConfig()
	cfg.TTL = time.Minute
	cache := newFakeCache()
	rdns := &countingRDNS{err: errors.New("i/o timeout")}
	r := newTestResolver(t, cfg, cache, rdns.lookup, testMetrics(t))
	now := time.Now()
	r.now = func() time.Time { return now }

	r.Resolve(t.Context(), []string{"203.0.113.9"})
	r.now = func() time.Time { return now.Add(time.Minute + time.Second) }
	r.Resolve(t.Context(), []string{"203.0.113.9"})
	if n := rdns.calls.Load(); n != 2 {
		t.Errorf("rdns lookups = %d, want 2: a 1m TTL expires the failed row after 1m", n)
	}
}

// A hop that resolves after a failure is cached for the full TTL again, and the remembered failure no
// longer answers for it.
func TestResolveRecoveredLookupReplacesTheRememberedFailure(t *testing.T) {
	cache := newFakeCache()
	rdns := &countingRDNS{err: errors.New("i/o timeout")}
	r := newTestResolver(t, enabledConfig(), cache, rdns.lookup, testMetrics(t))
	now := time.Now()
	r.now = func() time.Time { return now }
	r.Resolve(t.Context(), []string{"203.0.113.9"})

	rdns.err = nil
	rdns.answers = map[string][]string{"203.0.113.9": {"edge.example."}}
	later := now.Add(failedLookupRetry + time.Second)
	r.now = func() time.Time { return later }
	if got := r.Resolve(t.Context(), []string{"203.0.113.9"}); got["203.0.113.9"].RDNS != "edge.example" {
		t.Fatalf("rdns after DNS recovered = %q, want edge.example", got["203.0.113.9"].RDNS)
	}
	r.now = func() time.Time { return later.Add(30 * time.Minute) }
	got := r.Resolve(t.Context(), []string{"203.0.113.9"})
	if row := got["203.0.113.9"]; row.RDNS != "edge.example" || !row.ResolvedAt.Equal(later) {
		t.Errorf("view inside the TTL = %+v, want edge.example resolved at %v", row, later)
	}
	if n := rdns.calls.Load(); n != 2 {
		t.Errorf("rdns lookups = %d, want 2", n)
	}
	if n := r.rememberedFailures(); n != 0 {
		t.Errorf("remembered failures = %d, want 0 once the hop resolved", n)
	}
}

// The failures live in memory, so they are bounded: a trace-heavy console with DNS down must not grow
// without limit.
func TestResolveRememberedFailuresAreBounded(t *testing.T) {
	rdns := &countingRDNS{err: errors.New("i/o timeout")}
	r := newTestResolver(t, enabledConfig(), newFakeCache(), rdns.lookup, testMetrics(t))
	ips := make([]string, 0, maxRememberedFailures+64)
	for i := range maxRememberedFailures + 64 {
		ips = append(ips, fmt.Sprintf("10.%d.%d.%d", i>>16&0xff, i>>8&0xff, i&0xff))
	}
	r.Resolve(t.Context(), ips)
	if n := r.rememberedFailures(); n > maxRememberedFailures {
		t.Errorf("remembered failures = %d, want at most %d", n, maxRememberedFailures)
	}

	// Expired entries make room.
	now := time.Now()
	r.now = func() time.Time { return now.Add(failedLookupRetry + time.Second) }
	r.Resolve(t.Context(), []string{"192.0.2.77"})
	if n := r.rememberedFailures(); n != 1 {
		t.Errorf("remembered failures after the window = %d, want only the new one", n)
	}
}

// Resolve is called from concurrent HTTP handlers; the remembered failures are shared between them.
func TestResolveRememberedFailuresUnderConcurrentViews(t *testing.T) {
	rdns := &countingRDNS{err: errors.New("i/o timeout")}
	r := newTestResolver(t, enabledConfig(), newFakeCache(), rdns.lookup, testMetrics(t))
	var wg sync.WaitGroup
	for g := range 16 {
		wg.Go(func() {
			for i := range 20 {
				got := r.Resolve(t.Context(), []string{fmt.Sprintf("198.51.100.%d", (g+i)%32)})
				if len(got) != 1 {
					t.Errorf("view got %d rows, want 1", len(got))
				}
			}
		})
	}
	wg.Wait()
	if n := r.rememberedFailures(); n != 32 {
		t.Errorf("remembered failures = %d, want 32", n)
	}
}

func (r *Resolver) rememberedFailures() int {
	r.failedMu.Lock()
	defer r.failedMu.Unlock()
	return len(r.failed)
}
