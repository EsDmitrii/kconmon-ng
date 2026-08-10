// Package enrich resolves MTR hop addresses into human-readable context -- reverse DNS; that makes
// the worst case a single slow snapshot detail request rather than a resolver storm the operator
// cannot see.
package enrich

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/oschwald/maxminddb-golang/v2"

	"github.com/EsDmitrii/kconmon-ng/internal/console/config"
	"github.com/EsDmitrii/kconmon-ng/internal/console/metrics"
	"github.com/EsDmitrii/kconmon-ng/internal/console/store"
)

// maxConcurrentResolves bounds the in-flight lookups for ONE Resolve call; a trace can carry up to
// 64 hops and the console pod is sized at 256Mi.
const maxConcurrentResolves = 8

// Store is the cache half of the resolver: store.EnrichmentStore; aliased rather than re-declared
// so there is exactly ONE spelling of the read/write pair in the tree.
type Store = store.EnrichmentStore

// RDNSLookup is the reverse-DNS seam, shaped exactly like (*net.Resolver).LookupAddr so the
// production value is a method value and not an adapter; it is injected for one reason above all
// others.
type RDNSLookup func(ctx context.Context, ip string) ([]string, error)

// Closed label values for metrics.EnrichmentLookups / EnrichmentCache. They
// are constants rather than literals at the call sites because a typo'd label
// value is a silently forked time series, not a compile error.
const (
	sourceRDNS = "rdns"
	sourceASN  = "asn"
	sourceCity = "city"

	resultOK    = "ok"
	resultMiss  = "miss"
	resultError = "error"

	cacheHit  = "hit"
	cacheMiss = "miss"
)

// Resolver answers hop-address questions from the cache, resolving what the cache does not know;
// safe for concurrent use: every field is read-only after New.
type Resolver struct {
	cache Store

	// rdns is nil when mtr.enrichment.rdns.enabled is false. nil is the ONLY
	// representation of "this source is off" for all three sources -- there is
	// no separate enabled bool to fall out of step with the reader.
	rdns        RDNSLookup
	rdnsTimeout time.Duration

	// asn and city are never reopened per lookup -- that would mmap a multi-megabyte file on every hop
	// of every trace. They ARE swapped by reloadGeoIP when the file on disk changes, which is what makes
	// the geoipupdate sidecar useful without a restart, so mu guards them and every read takes RLock.
	mu   sync.RWMutex
	asn  *maxminddb.Reader
	city *maxminddb.Reader
	// Paths and the mtime each reader was opened at; a differing mtime is the whole change signal.
	asnPath, cityPath string
	asnMod, cityMod   time.Time
	// stopReload closes to end the reload goroutine; nil when reloading is off.
	stopReload chan struct{}

	ttl time.Duration
	m   *metrics.Metrics

	// now is time.Now except in tests. TTL expiry is the one behaviour here
	// that cannot be exercised without controlling the clock, and sleeping for
	// a real TTL is not a test.
	now func() time.Time

	// closeOnce keeps Close idempotent: cmd/console defers it and a test may
	// call it explicitly, and closing an mmap twice is not a thing to find out
	// about in production.
	closeOnce sync.Once
	closeErr  error
}

// New builds a Resolver from the console's mtr.enrichment block; it returns an error ONLY for a
// composition mistake the operator cannot fix at runtime (a nil cache).
func New(cfg config.EnrichmentConfig, cache Store, rdns RDNSLookup, m *metrics.Metrics) (*Resolver, error) {
	if cache == nil {
		return nil, errors.New("enrich: cache must not be nil (the TTL cache is the resolver's whole point)")
	}

	r := &Resolver{
		cache:       cache,
		ttl:         cfg.TTL,
		rdnsTimeout: time.Duration(cfg.RDNS.TimeoutMs) * time.Millisecond,
		m:           m,
		now:         time.Now,
	}
	if cfg.RDNS.Enabled {
		r.rdns = rdns
		if r.rdns == nil {
			r.rdns = (&net.Resolver{}).LookupAddr
		}
	}
	r.asnPath, r.cityPath = cfg.GeoIP.ASNPath, cfg.GeoIP.CityPath
	r.asn, r.asnMod = openMMDB(sourceASN, r.asnPath), fileModTime(r.asnPath)
	r.city, r.cityMod = openMMDB(sourceCity, r.cityPath), fileModTime(r.cityPath)
	r.startReload(cfg.GeoIP.ReloadInterval)

	if !r.hasLiveSource() {
		// Reachable despite config validation: validate() proves a source was
		// CONFIGURED, not that its file opened. Saying so once at boot beats
		// an operator wondering why a resolver they enabled returns nothing.
		slog.Warn("hop enrichment is enabled but every source is unavailable — " +
			"lookups will be skipped entirely (an empty row must never be cached for mtr.enrichment.ttl)")
	}
	return r, nil
}

// openMMDB opens one geoip database, or reports the source disabled; an empty path is silent (that
// source was never asked for).
func openMMDB(source, path string) *maxminddb.Reader {
	if path == "" {
		return nil
	}
	rd, err := maxminddb.Open(path)
	if err != nil {
		slog.Warn("hop enrichment source disabled: mmdb could not be opened", //nolint:gosec // G706: structured slog fields, and path/source are operator config
			"source", source, "path", path, "error", err)
		return nil
	}
	slog.Info("hop enrichment source enabled", //nolint:gosec // G706: structured slog fields, operator config
		"source", source, "path", path, "database", rd.Metadata.DatabaseType)
	return rd
}

// fileModTime is the change signal for reloadGeoIP; a missing file reports the zero time, which is
// exactly what makes "the sidecar's first download lands after boot" a detectable change.
func fileModTime(path string) time.Time {
	if path == "" {
		return time.Time{}
	}
	fi, err := os.Stat(path)
	if err != nil {
		return time.Time{}
	}
	return fi.ModTime()
}

// startReload runs reloadGeoIP on an interval. Off (nil channel, no goroutine) when the interval is
// not positive or no geoip path is configured, so an install that mounts its own files pays nothing.
func (r *Resolver) startReload(every time.Duration) {
	if every <= 0 || (r.asnPath == "" && r.cityPath == "") {
		return
	}
	r.stopReload = make(chan struct{})
	go func() {
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-r.stopReload:
				return
			case <-t.C:
				r.reloadGeoIP()
			}
		}
	}()
}

// reloadGeoIP reopens whichever database changed on disk and swaps it in. The old reader is closed
// only after the swap completes under the write lock, so no lookup can be reading a closed mmap --
// that would be a SIGSEGV, not an error return.
func (r *Resolver) reloadGeoIP() {
	r.reloadOne(sourceASN, r.asnPath, &r.asn, &r.asnMod)
	r.reloadOne(sourceCity, r.cityPath, &r.city, &r.cityMod)
}

func (r *Resolver) reloadOne(source, path string, cur **maxminddb.Reader, mod *time.Time) {
	if path == "" {
		return
	}
	next := fileModTime(path)
	r.mu.RLock()
	unchanged := next.Equal(*mod)
	r.mu.RUnlock()
	if unchanged {
		return
	}
	// Opened OUTSIDE the write lock: reading a ~60MB database must not block lookups.
	rd := openMMDB(source, path)
	if rd == nil {
		// A half-written download is the common case; keep serving the previous database and retry on
		// the next tick. The mtime is deliberately NOT recorded, so the retry actually happens.
		return
	}
	r.mu.Lock()
	old := *cur
	*cur, *mod = rd, next
	r.mu.Unlock()
	if old != nil {
		if err := old.Close(); err != nil {
			slog.Warn("closing the replaced mmdb failed", "source", source, "error", err) //nolint:gosec // G706: structured slog fields, operator config
		}
	}
	slog.Info("hop enrichment source reloaded", "source", source, "path", path) //nolint:gosec // G706: structured slog fields, operator config
}

// hasLiveSource reports whether anything could actually be resolved. Used to
// skip the whole miss path: with no live source every "resolution" would be an
// empty row, and caching those would let the TTL protect a lie for a day.
func (r *Resolver) hasLiveSource() bool {
	return r.rdns != nil || r.asn != nil || r.city != nil
}

// Close releases the mmdb mmaps. Idempotent.
func (r *Resolver) Close() error {
	r.closeOnce.Do(func() {
		if r.stopReload != nil {
			close(r.stopReload)
		}
		r.mu.Lock()
		defer r.mu.Unlock()
		var errs []error
		if r.asn != nil {
			errs = append(errs, r.asn.Close())
		}
		if r.city != nil {
			errs = append(errs, r.city.Close())
		}
		r.closeErr = errors.Join(errs...)
	})
	return r.closeErr
}

// GetEnrichment is httpapi.EnrichmentReader; the error is always nil, and that is the point rather
// than an oversight.
func (r *Resolver) GetEnrichment(ctx context.Context, ips []string) (map[string]store.Enrichment, error) {
	return r.Resolve(ctx, ips), nil
}

// Resolve answers every address it can, in four steps: read the cache, keep the rows younger than
// the TTL.
func (r *Resolver) Resolve(ctx context.Context, ips []string) map[string]store.Enrichment {
	want := dedupe(ips)
	out := make(map[string]store.Enrichment, len(want))
	if len(want) == 0 {
		return out
	}

	cached, err := r.cache.GetEnrichment(ctx, want)
	if err != nil {
		// Degraded, not fatal: treat the whole batch as a miss. The lookups
		// that follow are bounded, and the write-back will fail the same way
		// (harmlessly) if the cache is genuinely down.
		slog.Warn("hop enrichment cache read failed — resolving every hop for this request", "error", err)
		cached = nil
	}

	now := r.now()
	misses := make([]string, 0, len(want))
	for _, ip := range want {
		row, ok := cached[ip]
		if ok && now.Sub(row.ResolvedAt) < r.ttl {
			out[ip] = row
			r.countCache(cacheHit)
			continue
		}
		r.countCache(cacheMiss)
		misses = append(misses, ip)
	}
	if len(misses) == 0 || !r.hasLiveSource() {
		return out
	}

	resolved := r.resolveAll(ctx, misses)
	for ip, row := range resolved {
		out[ip] = row
	}
	r.writeBack(ctx, resolved)
	return out
}

// dedupe trims, drops blanks and removes repeats, preserving first-seen order.
// A trace routinely repeats a hop across TTL probes, and an unresponsive hop
// has no address at all; resolving either twice would be pure waste.
func dedupe(ips []string) []string {
	seen := make(map[string]bool, len(ips))
	out := make([]string, 0, len(ips))
	for _, raw := range ips {
		ip := strings.TrimSpace(raw)
		if ip == "" || seen[ip] {
			continue
		}
		seen[ip] = true
		out = append(out, ip)
	}
	return out
}

// resolveAll runs the misses through a fixed pool of maxConcurrentResolves workers; the feed loop
// selects on ctx.Done, so a cancelled request stops handing out work immediately and the pool
// drains within one per-lookup budget instead of one budget per remaining hop.
func (r *Resolver) resolveAll(ctx context.Context, ips []string) map[string]store.Enrichment {
	workers := maxConcurrentResolves
	if len(ips) < workers {
		workers = len(ips)
	}

	jobs := make(chan string)
	var mu sync.Mutex
	out := make(map[string]store.Enrichment, len(ips))

	var wg sync.WaitGroup
	wg.Add(workers)
	for range workers {
		go func() {
			defer wg.Done()
			for ip := range jobs {
				row, ok := r.resolveOne(ctx, ip)
				if !ok {
					continue
				}
				mu.Lock()
				out[ip] = row
				mu.Unlock()
			}
		}()
	}

feed:
	for _, ip := range ips {
		select {
		case <-ctx.Done():
			break feed
		case jobs <- ip:
		}
	}
	close(jobs)
	wg.Wait()
	return out
}

// resolveOne builds one cache row.
func (r *Resolver) resolveOne(ctx context.Context, ip string) (store.Enrichment, bool) {
	if ctx.Err() != nil {
		return store.Enrichment{}, false
	}
	row := store.Enrichment{IP: ip, ResolvedAt: r.now()}

	if r.rdns != nil {
		r.lookupRDNS(ctx, ip, &row)
	}
	// One RLock for the whole geoip section: a lookup reads an mmap, so the reader must not be swapped
	// out from under it. Uncontended RLock costs tens of nanoseconds against a multi-hop trace.
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.asn != nil || r.city != nil {
		addr, err := netip.ParseAddr(ip)
		switch {
		case err != nil:
			// mtr reports a hop it could not identify with a placeholder; that is bad INPUT, not a broken
			// database, so neither source is counted.
			slog.Debug("hop address is not an IP — skipping geoip sources", "ip", ip) //nolint:gosec // G706: structured slog field; hop addresses stay in the console's own logs, never in a metric label
		default:
			if r.asn != nil {
				r.lookupASN(addr, &row)
			}
			if r.city != nil {
				r.lookupCity(addr, &row)
			}
		}
	}

	if ctx.Err() != nil {
		return store.Enrichment{}, false
	}
	return row, true
}

// lookupRDNS fills row.RDNS within its OWN budget, derived from the caller's
// context so whichever expires first wins. The trailing dot of the PTR name is
// stripped: it is correct DNS and noise in a hop table.
func (r *Resolver) lookupRDNS(ctx context.Context, ip string, row *store.Enrichment) {
	lctx, cancel := context.WithTimeout(ctx, r.rdnsTimeout)
	defer cancel()

	names, err := r.rdns(lctx, ip)
	switch {
	case err != nil:
		r.countLookup(sourceRDNS, resultError)
		slog.Debug("reverse DNS lookup failed", "ip", ip, "error", err) //nolint:gosec // G706: structured slog fields; IPs stay in the console's own logs
	case len(names) == 0 || strings.TrimSuffix(names[0], ".") == "":
		// No PTR record is an ordinary answer about the address, not a
		// failure of the source -- keeping the two apart is what makes the
		// error series alertable.
		r.countLookup(sourceRDNS, resultMiss)
	default:
		row.RDNS = strings.TrimSuffix(names[0], ".")
		r.countLookup(sourceRDNS, resultOK)
	}
}

// asnRecord is the GeoLite2-ASN subset the console uses; provider is the organization string, which
// is what an operator actually reads in a hop table.
type asnRecord struct {
	Number uint32 `maxminddb:"autonomous_system_number"`
	Org    string `maxminddb:"autonomous_system_organization"`
}

func (r *Resolver) lookupASN(addr netip.Addr, row *store.Enrichment) {
	res := r.asn.Lookup(addr)
	if err := res.Err(); err != nil {
		r.countLookup(sourceASN, resultError)
		slog.Debug("asn lookup failed", "error", err)
		return
	}
	if !res.Found() {
		r.countLookup(sourceASN, resultMiss)
		return
	}
	var rec asnRecord
	if err := res.Decode(&rec); err != nil {
		r.countLookup(sourceASN, resultError)
		slog.Debug("asn record decode failed", "error", err)
		return
	}
	row.ASN = int64(rec.Number)
	row.Provider = rec.Org
	r.countLookup(sourceASN, resultOK)
}

// cityRecord is the GeoLite2-City subset the console uses; only the English name is read: the
// console UI is English-only and carrying a dozen localisations per hop into a JSONB column the
// frontend would ignore is storage spent on nothing.
type cityRecord struct {
	City struct {
		Names map[string]string `maxminddb:"names"`
	} `maxminddb:"city"`
	Country struct {
		ISOCode string `maxminddb:"iso_code"`
	} `maxminddb:"country"`
	Location struct {
		Latitude  float64 `maxminddb:"latitude"`
		Longitude float64 `maxminddb:"longitude"`
	} `maxminddb:"location"`
}

// geo is store.Enrichment.Geo's shape on the wire and in the column; the JSON is written by this
// package and read by the frontend.
type geo struct {
	Country string  `json:"country,omitempty"`
	City    string  `json:"city,omitempty"`
	Lat     float64 `json:"lat,omitempty"`
	Lon     float64 `json:"lon,omitempty"`
}

func (r *Resolver) lookupCity(addr netip.Addr, row *store.Enrichment) {
	res := r.city.Lookup(addr)
	if err := res.Err(); err != nil {
		r.countLookup(sourceCity, resultError)
		slog.Debug("city lookup failed", "error", err)
		return
	}
	if !res.Found() {
		r.countLookup(sourceCity, resultMiss)
		return
	}
	var rec cityRecord
	if err := res.Decode(&rec); err != nil {
		r.countLookup(sourceCity, resultError)
		slog.Debug("city record decode failed", "error", err)
		return
	}

	g := geo{Country: rec.Country.ISOCode, City: rec.City.Names["en"], Lat: rec.Location.Latitude, Lon: rec.Location.Longitude}
	if g == (geo{}) {
		// The address is in the database but carries nothing the console records.
		r.countLookup(sourceCity, resultMiss)
		return
	}
	encoded, err := json.Marshal(g)
	if err != nil {
		// Unreachable for a struct of four scalars, handled anyway: a panic
		// here would take down a request that asked for a traceroute.
		r.countLookup(sourceCity, resultError)
		slog.Debug("geo encode failed", "error", err)
		return
	}
	row.Geo = encoded
	r.countLookup(sourceCity, resultOK)
}

// writeBack persists what was resolved.
func (r *Resolver) writeBack(ctx context.Context, rows map[string]store.Enrichment) {
	if len(rows) == 0 {
		return
	}
	batch := make([]store.Enrichment, 0, len(rows))
	for _, row := range rows {
		batch = append(batch, row)
	}
	if err := r.cache.PutEnrichment(ctx, batch); err != nil {
		slog.Warn("hop enrichment write-back failed — the next read will re-resolve", "rows", len(batch), "error", err)
	}
}

// countCache and countLookup are the ONLY places this package touches metrics.
func (r *Resolver) countCache(result string) {
	if r.m == nil {
		return
	}
	r.m.EnrichmentCache.WithLabelValues(result).Inc()
}

func (r *Resolver) countLookup(source, result string) {
	if r.m == nil {
		return
	}
	r.m.EnrichmentLookups.WithLabelValues(source, result).Inc()
}

// String reports the enabled sources, for cmd/console's boot log. It names
// switches, never data.
func (r *Resolver) String() string {
	return fmt.Sprintf("enrich.Resolver{rdns:%t asn:%t city:%t ttl:%v}", r.rdns != nil, r.asn != nil, r.city != nil, r.ttl)
}
