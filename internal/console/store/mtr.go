package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/EsDmitrii/kconmon-ng/internal/console/store/gen"
)

// ---------------------------------------------------------------------------
// Path snapshots
// ---------------------------------------------------------------------------

// pathHashLen is the character length of HashPath's output: hex sha256.
const pathHashLen = sha256.Size * 2

// maxPathHops bounds a snapshot's hop list.
const maxPathHops = 64

// hopSep separates hop IPs inside the hashed byte sequence; a newline can never appear inside an IP
// literal, so no escaping is needed.
const hopSep = "\n"

// PathHop is one hop of a stored trace, and the shape migration 00005's comment on
// mtr_path_snapshots.hops names; only IP takes part in the dedupe key.
type PathHop struct {
	Number    int     `json:"number"`
	IP        string  `json:"ip"`
	Hostname  string  `json:"hostname,omitempty"`
	RTTNs     int64   `json:"rttNs"`
	LossRatio float64 `json:"lossRatio"`
}

// HashPath is the content hash that decides whether a trace describes a route the pair has already
// taken; it reads ONLY the responding hop IPs, in order: not RTTs, which jitter on every trace, and
// not silent "*" hops, which flap independently of the route and would otherwise read as a change.
func HashPath(hops []PathHop) string {
	h := sha256.New()
	wrote := false
	for i := range hops {
		if strings.TrimSpace(hops[i].IP) == "" || hops[i].IP == "*" {
			continue
		}
		if wrote {
			h.Write([]byte(hopSep))
		}
		h.Write([]byte(hops[i].IP))
		wrote = true
	}
	if !wrote {
		return ""
	}
	return hex.EncodeToString(h.Sum(nil))
}

// silentPathToken is hashed for a trace where NO hop answered: a sentinel, not an address, so it
// cannot collide with a real route -- HashPath only ever hashes hop IPs, and no IP list spells this.
const silentPathToken = "\x00kconmon-ng:no-hop-answered" //nolint:gosec // G101: a hash sentinel, not a credential

// HashSilentPath is the identity of "the trace ran and nothing on the way answered".
//
// Such a trace has no route to name, but it is still a trace: the probe left, the destination was
// reached or not, and the pair has a history. Giving it ONE stable identity keeps every all-silent
// trace to a destination folded into a single row instead of inserting one per probe, and keeps it
// distinct from every route that does have responders.
func HashSilentPath() string {
	h := sha256.New()
	h.Write([]byte(silentPathToken))
	return hex.EncodeToString(h.Sum(nil))
}

// PathSnapshot is one persisted mtr_path_snapshots row: a distinct route a
// (source, destination) pair has been observed taking, with the payload of
// the first trace that took it.
type PathSnapshot struct {
	ID          string
	SourceNode  string
	Destination string
	PathHash    string
	HopCount    int32
	// Hops is the stored JSONB array of PathHop; it is handed back raw for the same reason
	// Target.Labels is: the API layer re-serializes.
	Hops       json.RawMessage
	FirstSeen  time.Time
	LastSeen   time.Time
	TraceCount int64
	// RunID is "" once the run that first produced this path has aged out of
	// check_runs -- the column is ON DELETE SET NULL by design (migration
	// 00005), since a path outliving its run is the point of path history.
	RunID string
}

// PathSnapshotInput is UpsertPathSnapshot's write payload.
type PathSnapshotInput struct {
	SourceNode string
	// Destination is a node NAME or a target NAME, never an address -- the
	// same metric-safe label the rest of the system uses.
	Destination string
	// PathHash may be left empty: Validate derives it from Hops; a caller that computed it itself (the
	// projector does, to decide whether to write at all) may set.
	PathHash string
	Hops     []PathHop
	// SeenAt is the trace's own time. It becomes first_seen AND last_seen on
	// a new row, and bumps last_seen on a repeat.
	SeenAt time.Time
	// RunID is optional: "" writes SQL NULL.
	RunID string
}

// SnapshotFilter selects a page of path snapshots. All fields optional; Limit
// is clamped to [1,500] the same way EventFilter.Limit is.
type SnapshotFilter struct {
	SourceNode  string // exact match; empty = all
	Destination string // exact match; empty = all
	Cursor      string // opaque keyset cursor from a previous page
	Limit       int
}

// SnapshotPage is one page of ListPathSnapshots results, same shape as
// TargetPage.
type SnapshotPage struct {
	Snapshots  []PathSnapshot
	NextCursor string // "" when the page is the last one
}

// MTRDestination is one (source, destination) pair path history knows about,
// with the aggregates the Explorer's pair list shows.
type MTRDestination struct {
	SourceNode  string
	Destination string
	// SnapshotCount is how many DISTINCT routes the pair has taken;
	// TraceCount is how many traces produced them. A pair with
	// SnapshotCount 1 and TraceCount 4000 has a stable route.
	SnapshotCount int64
	TraceCount    int64
	FirstSeen     time.Time
	LastSeen      time.Time
}

// PathSnapshotStore is the write seam: the checks runner's projector is its only caller.
type PathSnapshotStore interface {
	// UpsertPathSnapshot records one trace. isNew reports whether the trace took a route this pair had
	// never taken.
	UpsertPathSnapshot(ctx context.Context, in PathSnapshotInput) (snap PathSnapshot, isNew bool, err error)
}

var _ PathSnapshotStore = (*DB)(nil)

// PathSnapshotReader is the read seam: httpapi's MTR routes (Task 4).
type PathSnapshotReader interface {
	/* ListMTRDestinations returns the pairs path history knows about, most-recently-traced first,
	   BOUNDED by limit (clamped like every other listing here).
	   It used to be unpaged, on the reasoning that "the row count is pairs, not traces" — but pairs
	   are sources x destinations, so a hundred nodes is ten thousand rows aggregated over the whole
	   snapshot table, materialised here and marshalled by the handler, on demand, for any caller
	   holding mtr:read. */
	ListMTRDestinations(ctx context.Context, limit int) ([]MTRDestination, error)
	// ListPathSnapshots pages a pair's route history newest-first, same keyset cursor shape as
	// ListTargets.
	ListPathSnapshots(ctx context.Context, f SnapshotFilter) (SnapshotPage, error)
	// GetPathSnapshot returns ErrNotFound when id does not name a snapshot --
	// including when it is not a UUID at all (GetRun's pre-check reasoning).
	GetPathSnapshot(ctx context.Context, id string) (PathSnapshot, error)
	// ListPathTraces pages the INDIVIDUAL traces recorded for a pair inside a window, newest first.
	// A route row's trace_count is a count of these; this is how a reader gets back to them.
	ListPathTraces(ctx context.Context, f TraceFilter) (TracePage, error)
}

var _ PathSnapshotReader = (*DB)(nil)

// Validate reports whether in is a well-formed snapshot; it runs before the INSERT so a caller gets
// a precise error instead of a raw constraint violation.
func (in *PathSnapshotInput) Validate() error {
	if in.SourceNode == "" {
		return errors.New("store: path snapshot: source node must not be empty")
	}
	if in.Destination == "" {
		return errors.New("store: path snapshot: destination must not be empty")
	}
	if len(in.Hops) == 0 {
		return errors.New("store: path snapshot: hops must not be empty")
	}
	if len(in.Hops) > maxPathHops {
		return fmt.Errorf("store: path snapshot: %d hops, limit is %d", len(in.Hops), maxPathHops)
	}
	for i := range in.Hops {
		if in.Hops[i].IP == "" {
			return fmt.Errorf("store: path snapshot: hop %d has no ip", i)
		}
	}
	if in.SeenAt.IsZero() {
		return errors.New("store: path snapshot: seen at must not be zero")
	}
	// The hash is derived, then compared rather than merely accepted. A trace where no hop answered
	// hashes to nothing, and takes the one reserved silent identity instead (HashSilentPath).
	want := HashPath(in.Hops)
	if want == "" {
		want = HashSilentPath()
	}
	switch in.PathHash {
	case "":
		in.PathHash = want
	case want:
	default:
		return fmt.Errorf("store: path snapshot: path hash %q does not match the hops (want %q)", in.PathHash, want)
	}
	if in.RunID != "" {
		if _, err := uuid.Parse(in.RunID); err != nil {
			return fmt.Errorf("store: path snapshot: run id: %w", err)
		}
	}
	return nil
}

func snapshotFromRow(s *gen.MtrPathSnapshot) PathSnapshot {
	return PathSnapshot{
		ID:          formatUUID(s.ID),
		SourceNode:  s.SourceNode,
		Destination: s.Destination,
		PathHash:    s.PathHash,
		HopCount:    s.HopCount,
		Hops:        s.Hops,
		FirstSeen:   s.FirstSeen,
		LastSeen:    s.LastSeen,
		TraceCount:  s.TraceCount,
		RunID:       optionalUUIDString(s.RunID),
	}
}

func (db *DB) UpsertPathSnapshot(ctx context.Context, in PathSnapshotInput) (PathSnapshot, bool, error) { //nolint:gocritic // hugeParam: PathSnapshotInput mirrors the other write-payload structs in this package
	if err := in.Validate(); err != nil {
		return PathSnapshot{}, false, err
	}
	// The id is minted here rather than left to the column DEFAULT, keeping
	// the package's one id story (CreateTarget's comment). On a conflict it is
	// simply never used -- the existing row's id comes back instead.
	sid, err := parseUUID(uuid.NewString())
	if err != nil {
		return PathSnapshot{}, false, fmt.Errorf("store: upsert path snapshot: %w", err)
	}
	runID, err := optionalUUID(in.RunID)
	if err != nil {
		return PathSnapshot{}, false, fmt.Errorf("store: upsert path snapshot: %w", err)
	}
	hops, err := json.Marshal(in.Hops)
	if err != nil {
		return PathSnapshot{}, false, fmt.Errorf("store: upsert path snapshot: encode hops: %w", err)
	}

	start := time.Now()
	row, err := gen.New(db.pool).UpsertPathSnapshot(ctx, gen.UpsertPathSnapshotParams{
		ID:          sid,
		SourceNode:  in.SourceNode,
		Destination: in.Destination,
		PathHash:    in.PathHash,
		HopCount:    int32(len(in.Hops)), //nolint:gosec // len is bounded by maxPathHops above
		Hops:        hops,
		SeenAt:      in.SeenAt,
		RunID:       runID,
	})
	db.observe(queryUpsertPathSnapshot, start, queryResult(err))
	if err != nil {
		return PathSnapshot{}, false, fmt.Errorf("store: upsert path snapshot: %w", err)
	}

	snap := snapshotFromRow(&gen.MtrPathSnapshot{
		ID:          row.ID,
		SourceNode:  row.SourceNode,
		Destination: row.Destination,
		PathHash:    row.PathHash,
		HopCount:    row.HopCount,
		Hops:        row.Hops,
		FirstSeen:   row.FirstSeen,
		LastSeen:    row.LastSeen,
		TraceCount:  row.TraceCount,
		RunID:       row.RunID,
	})
	return snap, row.Inserted, nil
}

func (db *DB) ListMTRDestinations(ctx context.Context, limit int) ([]MTRDestination, error) {
	start := time.Now()
	rows, err := gen.New(db.pool).ListMTRDestinations(ctx, int32(clampLimit(limit))) //nolint:gosec // clampLimit bounds this to maxLimit
	db.observe(queryListMTRDestinations, start, queryResult(err))
	if err != nil {
		return nil, fmt.Errorf("store: list mtr destinations: %w", err)
	}
	dests := make([]MTRDestination, len(rows))
	for i := range rows {
		dests[i] = MTRDestination{
			SourceNode:    rows[i].SourceNode,
			Destination:   rows[i].Destination,
			SnapshotCount: rows[i].SnapshotCount,
			TraceCount:    rows[i].TraceCount,
			FirstSeen:     rows[i].FirstSeen,
			LastSeen:      rows[i].LastSeen,
		}
	}
	return dests, nil
}

func (db *DB) ListPathSnapshots(ctx context.Context, f SnapshotFilter) (SnapshotPage, error) { //nolint:gocritic // hugeParam: SnapshotFilter mirrors EventFilter's value semantics (events.go)
	limit := clampLimit(f.Limit)

	curTime, curID, err := decodeKeyset(f.Cursor)
	if err != nil {
		return SnapshotPage{}, fmt.Errorf("store: list path snapshots: %w", err)
	}

	var sourceNode, destination pgtype.Text
	if f.SourceNode != "" {
		sourceNode = pgtype.Text{String: f.SourceNode, Valid: true}
	}
	if f.Destination != "" {
		destination = pgtype.Text{String: f.Destination, Valid: true}
	}

	start := time.Now()
	rows, err := gen.New(db.pool).ListPathSnapshots(ctx, gen.ListPathSnapshotsParams{
		SourceNode:  sourceNode,
		Destination: destination,
		CurTime:     curTime,
		CurID:       curID,
		Lim:         int32(limit), //nolint:gosec // limit is clamped to [1,500] above
	})
	db.observe(queryListPathSnapshots, start, queryResult(err))
	if err != nil {
		return SnapshotPage{}, fmt.Errorf("store: list path snapshots: %w", err)
	}

	snaps := make([]PathSnapshot, len(rows))
	for i := range rows {
		snaps[i] = snapshotFromRow(&rows[i])
	}

	var nextCursor string
	if len(rows) == limit {
		last := snaps[len(snaps)-1]
		// FirstSeen, matching the query's own ORDER BY: a cursor over last_seen walks a key that
		// every repeat trace moves, and rows fall through the gap. See ListPathSnapshots.
		nextCursor = EncodeUUIDCursor(last.FirstSeen, last.ID)
	}

	return SnapshotPage{Snapshots: snaps, NextCursor: nextCursor}, nil
}

// TraceFilter scopes ListPathTraces: one pair, one time window, one page.
type TraceFilter struct {
	SourceNode  string
	Destination string
	// From and To are the ROUTE's own window (a snapshot's first_seen/last_seen), so the scan is
	// bounded by the thing being asked about rather than by the whole results table.
	From   time.Time
	To     time.Time
	Limit  int
	Cursor string
}

// TracePage is ListPathTraces' keyset page. The rows are RunResult — the same shape the run
// permalink already serves — because that is literally what they are: check_results rows, read by
// pair and window instead of by run. Hops are not a field: the caller reads them out of Result with
// checks.TraceFromResult, which is also what decides which route a trace belongs to.
type TracePage struct {
	Traces     []RunResult
	NextCursor string
}

// ListPathTraces reads the individual traces recorded for one pair inside one window, newest first.
//
// This is what makes a route row's "147 traces" more than a number: the path history folds every
// trace that walked a path into one row, and until this existed there was no way back to the traces
// themselves. Filtering by PATH is the caller's job (the hash lives inside the JSONB payload), so
// this returns everything in the window and the handler keeps what matches.
func (db *DB) ListPathTraces(ctx context.Context, f TraceFilter) (TracePage, error) { //nolint:gocritic // hugeParam: mirrors SnapshotFilter's value semantics above
	limit := clampLimit(f.Limit)

	curTime, curID, ok, err := DecodeCursor(f.Cursor)
	if err != nil {
		return TracePage{}, fmt.Errorf("store: list path traces: %w", err)
	}
	var cur pgtype.Timestamptz
	var curIDArg pgtype.Int8
	if ok {
		cur = pgtype.Timestamptz{Time: curTime, Valid: true}
		curIDArg = pgtype.Int8{Int64: curID, Valid: true}
	}

	start := time.Now()
	rows, err := gen.New(db.pool).ListPathTraces(ctx, gen.ListPathTracesParams{
		SourceNode:  f.SourceNode,
		Destination: f.Destination,
		FromTime:    f.From,
		ToTime:      f.To,
		CurTime:     cur,
		CurID:       curIDArg,
		Lim:         int32(limit), //nolint:gosec // limit is clamped to [1,500] above
	})
	db.observe(queryListPathTraces, start, queryResult(err))
	if err != nil {
		return TracePage{}, fmt.Errorf("store: list path traces: %w", err)
	}

	traces := make([]RunResult, len(rows))
	for i := range rows {
		traces[i] = runResultFromRow(&rows[i])
	}

	var nextCursor string
	if len(rows) == limit {
		last := traces[len(traces)-1]
		nextCursor = EncodeCursor(last.RecordedAt, last.ID)
	}
	return TracePage{Traces: traces, NextCursor: nextCursor}, nil
}

// GetPathSnapshot applies GetRun's UUID pre-check for the same reason and with the same ErrNotFound
// answer.
func (db *DB) GetPathSnapshot(ctx context.Context, id string) (PathSnapshot, error) {
	sid, err := parseUUID(id)
	if err != nil {
		return PathSnapshot{}, fmt.Errorf("store: get path snapshot: %w: %w", ErrNotFound, err)
	}
	start := time.Now()
	s, err := gen.New(db.pool).GetPathSnapshot(ctx, sid)
	db.observe(queryGetPathSnapshot, start, queryResult(wrapNoRows(err)))
	if err != nil {
		return PathSnapshot{}, fmt.Errorf("store: get path snapshot: %w", wrapNoRows(err))
	}
	return snapshotFromRow(&s), nil
}

// DeletePathSnapshotsBefore deletes up to limit snapshots last seen before before.
func (db *DB) DeletePathSnapshotsBefore(ctx context.Context, before time.Time, limit int32) (int64, error) {
	n, err := gen.New(db.pool).DeletePathSnapshotsBefore(ctx, gen.DeletePathSnapshotsBeforeParams{
		LastSeen: before,
		Limit:    limit,
	})
	if err != nil {
		return 0, fmt.Errorf("store: delete path snapshots before: %w", err)
	}
	return n, nil
}

// ---------------------------------------------------------------------------
// Hop enrichment (TTL cache)
// ---------------------------------------------------------------------------

// enrichmentIPMaxLen bounds the cache key. 45 is the longest plain IPv6
// literal; the extra room covers a zone suffix without letting an arbitrary
// string become a primary key.
const enrichmentIPMaxLen = 64

// Enrichment is one cached lookup about a hop address.
type Enrichment struct {
	IP       string
	RDNS     string
	ASN      int64
	Provider string
	// Geo is a JSON object. nil, an empty slice and the literal JSON null are
	// all written as {} (orEmptyJSON), same contract as TargetInput.Labels.
	Geo        json.RawMessage
	ResolvedAt time.Time
}

// EnrichmentStore is the seam the enrichment resolver (Task 5) takes: read the
// cache for a whole trace's hops, write back what it had to resolve.
type EnrichmentStore interface {
	// GetEnrichment returns the cached rows for ips, keyed by IP. A miss is
	// simply an absent key, never an error -- it is the signal to resolve.
	// An empty ips takes no round trip at all.
	GetEnrichment(ctx context.Context, ips []string) (map[string]Enrichment, error)
	// PutEnrichment upserts rows in one statement. Every row is validated
	// first: a partially-written cache refresh is worse than a rejected one,
	// since the caller could not tell which half landed.
	PutEnrichment(ctx context.Context, rows []Enrichment) error
}

var _ EnrichmentStore = (*DB)(nil)

// Validate reports whether e is a well-formed cache row.
func (e *Enrichment) Validate() error {
	if e.IP == "" {
		return errors.New("store: enrichment: ip must not be empty")
	}
	if len(e.IP) > enrichmentIPMaxLen {
		return fmt.Errorf("store: enrichment: ip is %d bytes, limit is %d", len(e.IP), enrichmentIPMaxLen)
	}
	if e.ResolvedAt.IsZero() {
		return errors.New("store: enrichment: resolved at must not be zero")
	}
	if err := validateJSON("geo", e.Geo); err != nil {
		return fmt.Errorf("store: enrichment: %w", err)
	}
	return nil
}

// enrichmentRow is one element of PutEnrichment's jsonb batch. The field names
// are the column aliases jsonb_to_recordset's AS list declares (queries/
// mtr.sql) and must stay in step with them.
type enrichmentRow struct {
	IP         string          `json:"ip"`
	RDNS       string          `json:"rdns"`
	ASN        int64           `json:"asn"`
	Provider   string          `json:"provider"`
	Geo        json.RawMessage `json:"geo"`
	ResolvedAt time.Time       `json:"resolved_at"`
}

func (db *DB) GetEnrichment(ctx context.Context, ips []string) (map[string]Enrichment, error) {
	// The empty case short-circuits before the pool is touched: asking about
	// no addresses is a normal read-path outcome, not a query with an empty
	// result.
	if len(ips) == 0 {
		return map[string]Enrichment{}, nil
	}
	start := time.Now()
	rows, err := gen.New(db.pool).GetEnrichment(ctx, ips)
	db.observe(queryGetEnrichment, start, queryResult(err))
	if err != nil {
		return nil, fmt.Errorf("store: get enrichment: %w", err)
	}
	out := make(map[string]Enrichment, len(rows))
	for i := range rows {
		out[rows[i].Ip] = Enrichment{
			IP:         rows[i].Ip,
			RDNS:       rows[i].Rdns,
			ASN:        rows[i].Asn,
			Provider:   rows[i].Provider,
			Geo:        rows[i].Geo,
			ResolvedAt: rows[i].ResolvedAt,
		}
	}
	return out, nil
}

func (db *DB) PutEnrichment(ctx context.Context, rows []Enrichment) error {
	if len(rows) == 0 {
		return nil
	}
	batch := make([]enrichmentRow, len(rows))
	for i := range rows {
		if err := rows[i].Validate(); err != nil {
			return err
		}
		batch[i] = enrichmentRow{
			IP:         rows[i].IP,
			RDNS:       rows[i].RDNS,
			ASN:        rows[i].ASN,
			Provider:   rows[i].Provider,
			Geo:        orEmptyJSON(rows[i].Geo),
			ResolvedAt: rows[i].ResolvedAt,
		}
	}
	payload, err := json.Marshal(batch)
	if err != nil {
		return fmt.Errorf("store: put enrichment: encode batch: %w", err)
	}

	start := time.Now()
	err = gen.New(db.pool).PutEnrichment(ctx, payload)
	db.observe(queryPutEnrichment, start, queryResult(err))
	if err != nil {
		return fmt.Errorf("store: put enrichment: %w", err)
	}
	return nil
}

// DeleteEnrichmentBefore deletes up to limit cache rows resolved before
// before, oldest first, and reports how many were removed. Used by Pruner's
// sweep; exposed for the same testability reason as DeleteRunsBefore.
func (db *DB) DeleteEnrichmentBefore(ctx context.Context, before time.Time, limit int32) (int64, error) {
	n, err := gen.New(db.pool).DeleteEnrichmentBefore(ctx, gen.DeleteEnrichmentBeforeParams{
		ResolvedAt: before,
		Limit:      limit,
	})
	if err != nil {
		return 0, fmt.Errorf("store: delete enrichment before: %w", err)
	}
	return n, nil
}

// DecodeHops parses a PathSnapshot.Hops payload back into typed hops; the store itself never needs
// this -- it hands the JSONB straight through.
func DecodeHops(raw json.RawMessage) ([]PathHop, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var hops []PathHop
	if err := json.Unmarshal(raw, &hops); err != nil {
		return nil, fmt.Errorf("store: decode hops: %w", err)
	}
	return hops, nil
}

// HopIPs is the hop-address list a caller hands GetEnrichment, deduplicated
// and with empty entries dropped, in first-seen order. Order is preserved so
// a caller can zip the result back onto the trace without a second pass.
func HopIPs(hops []PathHop) []string {
	seen := make(map[string]bool, len(hops))
	ips := make([]string, 0, len(hops))
	for i := range hops {
		ip := strings.TrimSpace(hops[i].IP)
		if ip == "" || seen[ip] {
			continue
		}
		seen[ip] = true
		ips = append(ips, ip)
	}
	return ips
}
