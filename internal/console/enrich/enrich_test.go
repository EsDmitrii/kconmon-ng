package enrich

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/EsDmitrii/kconmon-ng/internal/console/config"
	"github.com/EsDmitrii/kconmon-ng/internal/console/metrics"
	"github.com/EsDmitrii/kconmon-ng/internal/console/store"
)

const (
	asnFixture  = "testdata/GeoLite2-ASN-Test.mmdb"
	cityFixture = "testdata/GeoLite2-City-Test.mmdb"
)

// fakeCache is an in-memory store.EnrichmentStore that counts calls and can be
// told to fail either half. No database, no pgx.
type fakeCache struct {
	mu      sync.Mutex
	rows    map[string]store.Enrichment
	gets    int
	puts    int
	putRows []store.Enrichment
	getErr  error
	putErr  error
}

func newFakeCache() *fakeCache { return &fakeCache{rows: map[string]store.Enrichment{}} }

func (f *fakeCache) GetEnrichment(_ context.Context, ips []string) (map[string]store.Enrichment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gets++
	if f.getErr != nil {
		return nil, f.getErr
	}
	out := map[string]store.Enrichment{}
	for _, ip := range ips {
		if row, ok := f.rows[ip]; ok {
			out[ip] = row
		}
	}
	return out, nil
}

func (f *fakeCache) PutEnrichment(_ context.Context, rows []store.Enrichment) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.puts++
	if f.putErr != nil {
		return f.putErr
	}
	f.putRows = append(f.putRows, rows...)
	for i := range rows {
		f.rows[rows[i].IP] = rows[i]
	}
	return nil
}

func (f *fakeCache) counts() (gets, puts int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.gets, f.puts
}

func (f *fakeCache) written() []store.Enrichment {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]store.Enrichment(nil), f.putRows...)
}

func (f *fakeCache) seed(row store.Enrichment) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rows[row.IP] = row
}

// countingRDNS is the injected reverse-DNS seam. Every test uses one: this
// package must never touch a resolver, and net.Resolver is only ever reached
// through New's nil-rdns default, which no test exercises.
type countingRDNS struct {
	calls   atomic.Int64
	inFlt   atomic.Int64
	maxFlt  atomic.Int64
	answers map[string][]string
	err     error
	block   time.Duration // sleep before answering; used for the concurrency/deadline tests
	hang    bool          // block until ctx is done, then return its error
}

func (c *countingRDNS) lookup(ctx context.Context, ip string) ([]string, error) {
	c.calls.Add(1)
	n := c.inFlt.Add(1)
	for {
		m := c.maxFlt.Load()
		if n <= m || c.maxFlt.CompareAndSwap(m, n) {
			break
		}
	}
	defer c.inFlt.Add(-1)

	if c.hang {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if c.block > 0 {
		select {
		case <-time.After(c.block):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if c.err != nil {
		return nil, c.err
	}
	return c.answers[ip], nil
}

func testMetrics(t *testing.T) *metrics.Metrics {
	t.Helper()
	return metrics.New("kconmon_ng", prometheus.NewRegistry())
}

// enabledConfig is the "every source on" config every test starts from; each
// one switches off what it does not want.
func enabledConfig() config.EnrichmentConfig {
	return config.EnrichmentConfig{
		Enabled: true,
		RDNS:    config.RDNSConfig{Enabled: true, TimeoutMs: 200},
		TTL:     time.Hour,
	}
}

func newTestResolver(t *testing.T, cfg config.EnrichmentConfig, cache Store, rdns RDNSLookup, m *metrics.Metrics) *Resolver {
	t.Helper()
	r, err := New(cfg, cache, rdns, m)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = r.Close() })
	return r
}

// --- construction ---------------------------------------------------------

func TestNewRejectsANilCache(t *testing.T) {
	if _, err := New(enabledConfig(), nil, func(context.Context, string) ([]string, error) { return nil, nil }, nil); err == nil {
		t.Fatal("New with a nil cache should error: the whole point of the resolver is the write-back")
	}
}

// TestNewMissingGeoIPPathsAreSilent: an empty path is that source switched
// off, not a failure -- rdns-only is a supported deployment.
func TestNewMissingGeoIPPathsAreSilent(t *testing.T) {
	rdns := &countingRDNS{}
	r := newTestResolver(t, enabledConfig(), newFakeCache(), rdns.lookup, testMetrics(t))
	if r.asn != nil || r.city != nil {
		t.Error("empty geoip paths should leave both mmdb readers nil")
	}
}

func TestNewUnreadableMMDBDisablesOnlyThatSource(t *testing.T) {
	junk := filepath.Join(t.TempDir(), "not-an-mmdb.bin")
	if err := os.WriteFile(junk, []byte("this is definitely not a MaxMind DB"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := enabledConfig()
	cfg.RDNS.Enabled = false
	cfg.GeoIP = config.GeoIPConfig{ASNPath: junk, CityPath: cityFixture}

	r := newTestResolver(t, cfg, newFakeCache(), nil, testMetrics(t))
	if r.asn != nil {
		t.Error("an unreadable asn mmdb must leave the ASN source disabled")
	}
	if r.city == nil {
		t.Fatal("a readable city mmdb must stay enabled when its sibling failed to open")
	}

	got := r.Resolve(t.Context(), []string{"192.0.2.1"})
	row := got["192.0.2.1"]
	if row.ASN != 0 || row.Provider != "" {
		t.Errorf("asn fields = %d/%q, want empty (source disabled)", row.ASN, row.Provider)
	}
	if len(row.Geo) == 0 {
		t.Error("city source should still have produced geo")
	}
}

// TestNewMissingGeoIPFileDisablesTheSource: a path that does not exist at all
// is the commonest mount mistake and takes the same warn-and-degrade route as
// a corrupt one.
func TestNewMissingGeoIPFileDisablesTheSource(t *testing.T) {
	cfg := enabledConfig()
	cfg.GeoIP = config.GeoIPConfig{ASNPath: filepath.Join(t.TempDir(), "nope.mmdb")}
	rdns := &countingRDNS{}
	r := newTestResolver(t, cfg, newFakeCache(), rdns.lookup, testMetrics(t))
	if r.asn != nil {
		t.Error("a nonexistent asn path must leave the ASN source disabled, not fail the boot")
	}
}

// --- cache semantics ------------------------------------------------------

// TestResolveCacheHitSkipsEveryResolver is the whole reason the cache exists:
// a fresh row answers without a single lookup, and it is counted as a cache
// hit exactly once (not once per source).
func TestResolveCacheHitSkipsEveryResolver(t *testing.T) {
	cache := newFakeCache()
	now := time.Now()
	cache.seed(store.Enrichment{
		IP: "192.0.2.1", RDNS: "cached.example.", ASN: 64496,
		Provider: "Cached Org", Geo: json.RawMessage(`{"country":"GB"}`), ResolvedAt: now.Add(-time.Minute),
	})
	rdns := &countingRDNS{answers: map[string][]string{"192.0.2.1": {"fresh.example."}}}
	m := testMetrics(t)

	cfg := enabledConfig()
	cfg.GeoIP = config.GeoIPConfig{ASNPath: asnFixture, CityPath: cityFixture}
	r := newTestResolver(t, cfg, cache, rdns.lookup, m)
	r.now = func() time.Time { return now }

	got := r.Resolve(t.Context(), []string{"192.0.2.1"})
	if got["192.0.2.1"].RDNS != "cached.example." {
		t.Errorf("rdns = %q, want the CACHED value", got["192.0.2.1"].RDNS)
	}
	if n := rdns.calls.Load(); n != 0 {
		t.Errorf("rdns lookups = %d, want 0 on a cache hit", n)
	}
	_, puts := cache.counts()
	if puts != 0 {
		t.Errorf("PutEnrichment calls = %d, want 0 -- nothing was resolved", puts)
	}
	if v := testutil.ToFloat64(m.EnrichmentCache.WithLabelValues("hit")); v != 1 {
		t.Errorf("EnrichmentCache(hit) = %v, want 1", v)
	}
	if v := testutil.ToFloat64(m.EnrichmentLookups.WithLabelValues("rdns", "ok")); v != 0 {
		t.Errorf("EnrichmentLookups(rdns, ok) = %v, want 0 on a cache hit", v)
	}
}

// TestResolveTTLExpiryReResolves: a row older than the TTL is a miss. This is
// the only refresh mechanism in M5 -- there is no background refresher.
func TestResolveTTLExpiryReResolves(t *testing.T) {
	cache := newFakeCache()
	now := time.Now()
	cache.seed(store.Enrichment{IP: "192.0.2.1", RDNS: "stale.example.", ResolvedAt: now.Add(-2 * time.Hour)})
	rdns := &countingRDNS{answers: map[string][]string{"192.0.2.1": {"fresh.example."}}}
	m := testMetrics(t)

	r := newTestResolver(t, enabledConfig(), cache, rdns.lookup, m) // ttl 1h
	r.now = func() time.Time { return now }

	got := r.Resolve(t.Context(), []string{"192.0.2.1"})
	if got["192.0.2.1"].RDNS != "fresh.example" {
		t.Errorf("rdns = %q, want the freshly resolved value with the trailing dot stripped", got["192.0.2.1"].RDNS)
	}
	if n := rdns.calls.Load(); n != 1 {
		t.Errorf("rdns lookups = %d, want 1", n)
	}
	if _, puts := cache.counts(); puts != 1 {
		t.Errorf("PutEnrichment calls = %d, want 1 (write-back)", puts)
	}
	written := cache.written()
	if len(written) != 1 || written[0].IP != "192.0.2.1" || written[0].RDNS != "fresh.example" {
		t.Errorf("write-back rows = %+v, want one refreshed row for 192.0.2.1", written)
	}
	if !written[0].ResolvedAt.Equal(now) {
		t.Errorf("write-back resolvedAt = %v, want the resolve-time clock %v", written[0].ResolvedAt, now)
	}
	if v := testutil.ToFloat64(m.EnrichmentCache.WithLabelValues("miss")); v != 1 {
		t.Errorf("EnrichmentCache(miss) = %v, want 1", v)
	}
}

// TestResolveCacheReadFailureResolvesEverything: an unreadable cache degrades
// to "resolve them all", never to an error the caller has to handle -- the
// handler's contract is that enrichment never fails the trace.
func TestResolveCacheReadFailureResolvesEverything(t *testing.T) {
	cache := newFakeCache()
	cache.getErr = errors.New("store: get enrichment: connection refused")
	rdns := &countingRDNS{answers: map[string][]string{"192.0.2.1": {"a.example."}}}

	r := newTestResolver(t, enabledConfig(), cache, rdns.lookup, testMetrics(t))
	got := r.Resolve(t.Context(), []string{"192.0.2.1"})
	if got["192.0.2.1"].RDNS != "a.example" {
		t.Errorf("rdns = %q, want a.example", got["192.0.2.1"].RDNS)
	}
}

// TestResolveWriteBackFailureStillReturnsResults: the write-back is
// best-effort. Losing it costs the NEXT read a re-resolve, nothing more.
func TestResolveWriteBackFailureStillReturnsResults(t *testing.T) {
	cache := newFakeCache()
	cache.putErr = errors.New("store: put enrichment: deadlock detected")
	rdns := &countingRDNS{answers: map[string][]string{"192.0.2.1": {"a.example."}}}

	r := newTestResolver(t, enabledConfig(), cache, rdns.lookup, testMetrics(t))
	got := r.Resolve(t.Context(), []string{"192.0.2.1"})
	if got["192.0.2.1"].RDNS != "a.example" {
		t.Errorf("rdns = %q, want the resolved value despite the failed write-back", got["192.0.2.1"].RDNS)
	}
}

// TestResolveDeduplicatesAndDropsBlanks: a trace routinely repeats a hop, and
// an unresponsive hop has no address at all.
func TestResolveDeduplicatesAndDropsBlanks(t *testing.T) {
	cache := newFakeCache()
	rdns := &countingRDNS{answers: map[string][]string{"192.0.2.1": {"a.example."}}}
	r := newTestResolver(t, enabledConfig(), cache, rdns.lookup, testMetrics(t))

	got := r.Resolve(t.Context(), []string{"192.0.2.1", "192.0.2.1", "", "  "})
	if len(got) != 1 {
		t.Errorf("result size = %d, want 1", len(got))
	}
	if n := rdns.calls.Load(); n != 1 {
		t.Errorf("rdns lookups = %d, want 1 (the duplicate must not be resolved twice)", n)
	}
}

func TestResolveEmptyInputTakesNoRoundTrip(t *testing.T) {
	cache := newFakeCache()
	rdns := &countingRDNS{}
	r := newTestResolver(t, enabledConfig(), cache, rdns.lookup, testMetrics(t))

	if got := r.Resolve(t.Context(), nil); len(got) != 0 {
		t.Errorf("result = %v, want empty", got)
	}
	if gets, puts := cache.counts(); gets != 0 || puts != 0 {
		t.Errorf("cache calls = %d/%d, want 0/0", gets, puts)
	}
}

// TestResolveWithNoLiveSourceNeverCachesEmptyRows guards the failure mode config validation exists
// to prevent but a failed mmdb open can still reach.
func TestResolveWithNoLiveSourceNeverCachesEmptyRows(t *testing.T) {
	cfg := enabledConfig()
	cfg.RDNS.Enabled = false
	cache := newFakeCache()
	r := newTestResolver(t, cfg, cache, nil, testMetrics(t))

	got := r.Resolve(t.Context(), []string{"192.0.2.1"})
	if len(got) != 0 {
		t.Errorf("result = %v, want empty with no live source", got)
	}
	if _, puts := cache.counts(); puts != 0 {
		t.Errorf("PutEnrichment calls = %d, want 0 -- an empty row must never be cached", puts)
	}
}

// --- rDNS -----------------------------------------------------------------

// TestResolveRDNSTimeoutLeavesFieldEmptyAndCountsError: the hop still gets a
// row (with the other sources' fields), the failure is counted, and the whole
// thing is bounded by rdns.timeoutMs, NOT by the caller's patience.
func TestResolveRDNSTimeoutLeavesFieldEmptyAndCountsError(t *testing.T) {
	cfg := enabledConfig()
	cfg.RDNS.TimeoutMs = 20
	cfg.GeoIP = config.GeoIPConfig{ASNPath: asnFixture}
	cache := newFakeCache()
	rdns := &countingRDNS{hang: true}
	m := testMetrics(t)
	r := newTestResolver(t, cfg, cache, rdns.lookup, m)

	start := time.Now()
	got := r.Resolve(t.Context(), []string{"192.0.2.1"})
	elapsed := time.Since(start)

	row, ok := got["192.0.2.1"]
	if !ok {
		t.Fatal("the hop must still get a row when rdns times out")
	}
	if row.RDNS != "" {
		t.Errorf("rdns = %q, want empty after a timeout", row.RDNS)
	}
	if row.ASN != 64496 {
		t.Errorf("asn = %d, want 64496 -- a per-source failure must not cost the other sources", row.ASN)
	}
	if elapsed > 2*time.Second {
		t.Errorf("Resolve took %v; the rdns budget is 20ms and must bound it", elapsed)
	}
	if v := testutil.ToFloat64(m.EnrichmentLookups.WithLabelValues("rdns", "error")); v != 1 {
		t.Errorf("EnrichmentLookups(rdns, error) = %v, want 1", v)
	}
	if _, puts := cache.counts(); puts != 1 {
		t.Errorf("PutEnrichment calls = %d, want 1 -- a partially resolved row is still worth caching", puts)
	}
}

// TestResolveRDNSNoPTRRecordCountsMiss: "this address has no PTR record" is an
// ordinary answer, not an error, and the two must stay distinguishable in the
// metric or a healthy resolver looks broken.
func TestResolveRDNSNoPTRRecordCountsMiss(t *testing.T) {
	m := testMetrics(t)
	rdns := &countingRDNS{answers: map[string][]string{}} // no names for anything
	r := newTestResolver(t, enabledConfig(), newFakeCache(), rdns.lookup, m)

	r.Resolve(t.Context(), []string{"192.0.2.1"})
	if v := testutil.ToFloat64(m.EnrichmentLookups.WithLabelValues("rdns", "miss")); v != 1 {
		t.Errorf("EnrichmentLookups(rdns, miss) = %v, want 1", v)
	}
	if v := testutil.ToFloat64(m.EnrichmentLookups.WithLabelValues("rdns", "error")); v != 0 {
		t.Errorf("EnrichmentLookups(rdns, error) = %v, want 0 -- an absent PTR is not a failure", v)
	}
}

// --- mmdb -----------------------------------------------------------------

// TestResolveMMDBFixtureLookups runs the REAL maxminddb reader against the
// committed fixtures (testdata/README.md documents how they were built).
func TestResolveMMDBFixtureLookups(t *testing.T) {
	cfg := enabledConfig()
	cfg.RDNS.Enabled = false
	cfg.GeoIP = config.GeoIPConfig{ASNPath: asnFixture, CityPath: cityFixture}
	m := testMetrics(t)
	r := newTestResolver(t, cfg, newFakeCache(), nil, m)

	got := r.Resolve(t.Context(), []string{"192.0.2.1", "192.0.2.130", "2001:db8::1"})

	for _, tc := range []struct {
		ip       string
		asn      int64
		provider string
		geo      string
	}{
		{"192.0.2.1", 64496, "Example Transit Network", `{"country":"GB","city":"London","lat":51.5074,"lon":-0.1278}`},
		{"192.0.2.130", 64497, "Example Edge Network", `{"country":"US","city":"Ashburn","lat":39.0438,"lon":-77.4874}`},
		{"2001:db8::1", 64500, "Example IPv6 Network", `{"country":"DE","city":"Frankfurt","lat":50.1109,"lon":8.6821}`},
	} {
		row, ok := got[tc.ip]
		if !ok {
			t.Errorf("%s: no row", tc.ip)
			continue
		}
		if row.ASN != tc.asn || row.Provider != tc.provider {
			t.Errorf("%s: asn/provider = %d/%q, want %d/%q", tc.ip, row.ASN, row.Provider, tc.asn, tc.provider)
		}
		if string(row.Geo) != tc.geo {
			t.Errorf("%s: geo = %s, want %s", tc.ip, row.Geo, tc.geo)
		}
	}

	if v := testutil.ToFloat64(m.EnrichmentLookups.WithLabelValues("asn", "ok")); v != 3 {
		t.Errorf("EnrichmentLookups(asn, ok) = %v, want 3", v)
	}
	if v := testutil.ToFloat64(m.EnrichmentLookups.WithLabelValues("city", "ok")); v != 3 {
		t.Errorf("EnrichmentLookups(city, ok) = %v, want 3", v)
	}
}

// TestResolveMMDBUnknownAddressCountsMiss: 192.0.2.192/26 is deliberately
// absent from both fixtures.
func TestResolveMMDBUnknownAddressCountsMiss(t *testing.T) {
	cfg := enabledConfig()
	cfg.RDNS.Enabled = false
	cfg.GeoIP = config.GeoIPConfig{ASNPath: asnFixture, CityPath: cityFixture}
	m := testMetrics(t)
	r := newTestResolver(t, cfg, newFakeCache(), nil, m)

	got := r.Resolve(t.Context(), []string{"192.0.2.200"})
	row, ok := got["192.0.2.200"]
	if !ok {
		t.Fatal("an address neither source knows must still get a row")
	}
	if row.ASN != 0 || row.Provider != "" || len(row.Geo) != 0 {
		t.Errorf("row = %+v, want every field empty", row)
	}
	for _, source := range []string{"asn", "city"} {
		if v := testutil.ToFloat64(m.EnrichmentLookups.WithLabelValues(source, "miss")); v != 1 {
			t.Errorf("EnrichmentLookups(%s, miss) = %v, want 1", source, v)
		}
	}
}

// TestResolveUnparseableHopAddressNeverReachesTheMMDB; it must not panic, must not be counted as an
// mmdb error (the database is fine, the input is not).
func TestResolveUnparseableHopAddressNeverReachesTheMMDB(t *testing.T) {
	cfg := enabledConfig()
	cfg.RDNS.Enabled = false
	cfg.GeoIP = config.GeoIPConfig{ASNPath: asnFixture, CityPath: cityFixture}
	m := testMetrics(t)
	r := newTestResolver(t, cfg, newFakeCache(), nil, m)

	got := r.Resolve(t.Context(), []string{"???"})
	if _, ok := got["???"]; !ok {
		t.Fatal("an unparseable hop address must still get a row")
	}
	for _, source := range []string{"asn", "city"} {
		for _, result := range []string{"ok", "miss", "error"} {
			if v := testutil.ToFloat64(m.EnrichmentLookups.WithLabelValues(source, result)); v != 0 {
				t.Errorf("EnrichmentLookups(%s, %s) = %v, want 0 -- the source never ran", source, result, v)
			}
		}
	}
}

// --- concurrency and cancellation -----------------------------------------

// TestResolveBoundsConcurrencyAtEight: a 64-hop trace must not open 64
// simultaneous resolver lookups from a 256Mi console pod.
func TestResolveBoundsConcurrencyAtEight(t *testing.T) {
	ips := make([]string, 0, 40)
	answers := map[string][]string{}
	for i := 1; i <= 40; i++ {
		ip := "192.0.2." + itoa(i)
		ips = append(ips, ip)
		answers[ip] = []string{"h" + itoa(i) + ".example."}
	}
	rdns := &countingRDNS{answers: answers, block: 5 * time.Millisecond}
	r := newTestResolver(t, enabledConfig(), newFakeCache(), rdns.lookup, testMetrics(t))

	got := r.Resolve(t.Context(), ips)
	if len(got) != len(ips) {
		t.Errorf("resolved %d of %d", len(got), len(ips))
	}
	if peak := rdns.maxFlt.Load(); peak > maxConcurrentResolves {
		t.Errorf("peak concurrent lookups = %d, want <= %d", peak, maxConcurrentResolves)
	}
}

// TestResolveRespectsCallerDeadlineAndReturnsPartials: the handler hands its
// REQUEST context in. When the client goes away mid-resolve, Resolve returns
// whatever finished instead of hanging on to the rest.
func TestResolveRespectsCallerDeadlineAndReturnsPartials(t *testing.T) {
	ips := make([]string, 0, 60)
	for i := 1; i <= 60; i++ {
		ips = append(ips, "192.0.2."+itoa(i))
	}
	cfg := enabledConfig()
	cfg.RDNS.TimeoutMs = 5000 // deliberately far beyond the caller's own deadline
	rdns := &countingRDNS{hang: true}
	cache := newFakeCache()
	r := newTestResolver(t, cfg, cache, rdns.lookup, testMetrics(t))

	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	got := r.Resolve(ctx, ips)
	elapsed := time.Since(start)

	if elapsed > 3*time.Second {
		t.Fatalf("Resolve ignored the caller's 50ms deadline: took %v", elapsed)
	}
	if len(got) > len(ips) {
		t.Errorf("result size = %d, want at most %d", len(got), len(ips))
	}
}

// --- the httpapi seam -----------------------------------------------------

// TestGetEnrichmentIsResolveBehindTheReadOnlySeam; it NEVER returns an error -- every failure
// inside is already degraded to a missing key.
func TestGetEnrichmentIsResolveBehindTheReadOnlySeam(t *testing.T) {
	cache := newFakeCache()
	cache.getErr = errors.New("cache down")
	rdns := &countingRDNS{answers: map[string][]string{"192.0.2.1": {"a.example."}}}
	r := newTestResolver(t, enabledConfig(), cache, rdns.lookup, testMetrics(t))

	got, err := r.GetEnrichment(t.Context(), []string{"192.0.2.1"})
	if err != nil {
		t.Fatalf("GetEnrichment must never surface an error, got %v", err)
	}
	if got["192.0.2.1"].RDNS != "a.example" {
		t.Errorf("rdns = %q, want a.example", got["192.0.2.1"].RDNS)
	}
}

func TestCloseIsIdempotentAndSafeWithoutReaders(t *testing.T) {
	cfg := enabledConfig()
	cfg.GeoIP = config.GeoIPConfig{ASNPath: asnFixture, CityPath: cityFixture}
	r, err := New(cfg, newFakeCache(), func(context.Context, string) ([]string, error) { return nil, nil }, testMetrics(t))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := r.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := r.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

// itoa avoids pulling strconv in for two call sites' worth of loop indices.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [4]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// --- geoip hot reload (M8) -------------------------------------------------
//
// The sidecar path (maxmind/geoipupdate refreshing /geoip on an interval) is
// only honest if the reader actually notices. These tests drive reloadGeoIP
// directly rather than waiting on the ticker: the ticker is one line, the
// swap is the behaviour.

// copyFixture puts the ASN fixture at a writable path so the test can age it.
func copyFixture(t *testing.T, dst string) {
	t.Helper()
	b, err := os.ReadFile(asnFixture)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestGeoIPReloadPicksUpAReplacedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "GeoLite2-ASN.mmdb")
	copyFixture(t, path)

	cfg := enabledConfig()
	cfg.RDNS.Enabled = false
	cfg.GeoIP = config.GeoIPConfig{ASNPath: path}
	r := newTestResolver(t, cfg, newFakeCache(), nil, testMetrics(t))

	before := r.asn
	if before == nil {
		t.Fatal("asn reader should be open at boot")
	}
	// geoipupdate writes a new database; only the mtime tells us so.
	copyFixture(t, path)
	future := time.Now().Add(2 * time.Hour)
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatal(err)
	}

	r.reloadGeoIP()

	if r.asn == nil {
		t.Fatal("asn reader must stay open across a reload")
	}
	if r.asn == before {
		t.Error("a changed mmdb must be reopened, so the sidecar's refresh is actually served")
	}
}

func TestGeoIPReloadIgnoresAnUnchangedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "GeoLite2-ASN.mmdb")
	copyFixture(t, path)

	cfg := enabledConfig()
	cfg.RDNS.Enabled = false
	cfg.GeoIP = config.GeoIPConfig{ASNPath: path}
	r := newTestResolver(t, cfg, newFakeCache(), nil, testMetrics(t))

	before := r.asn
	r.reloadGeoIP()
	if r.asn != before {
		t.Error("an untouched mmdb must not be reopened: reopening mmaps a multi-megabyte file for nothing")
	}
}

func TestGeoIPReloadEnablesASourceThatWasMissingAtBoot(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "GeoLite2-ASN.mmdb")

	cfg := enabledConfig() // rdns stays on, so the resolver has a live source at boot
	cfg.GeoIP = config.GeoIPConfig{ASNPath: path}
	r := newTestResolver(t, cfg, newFakeCache(), nil, testMetrics(t))
	if r.asn != nil {
		t.Fatal("a missing mmdb must leave the source off at boot")
	}

	// The sidecar's FIRST download lands after the console started.
	copyFixture(t, path)
	r.reloadGeoIP()

	if r.asn == nil {
		t.Error("the first download after boot must switch the source on without a restart")
	}
}
