package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/EsDmitrii/kconmon-ng/internal/console/store"
)

// EnrichmentReader is the READ half of store.EnrichmentStore, narrowed the same way EventLister
// narrows store.EventStore.
type EnrichmentReader interface {
	GetEnrichment(ctx context.Context, ips []string) (map[string]store.Enrichment, error)
}

// MTRService is the subset of *store.DB httpapi needs for /api/v1/mtr/*; composed from store's own
// interfaces in TargetService's shape.
type MTRService interface {
	store.PathSnapshotReader
	EnrichmentReader
}

var _ MTRService = (*store.DB)(nil)

// mtrUnavailableDetail is served whenever s.mtr is nil, in targetsUnavailableDetail's shape; path
// history has no in-memory fallback for a stronger reason than targets do.
const mtrUnavailableDetail = "MTR path history lives in the database and has no in-memory fallback: " +
	"set console.database.mode in the console config (Helm: console.database.mode) to enable /api/v1/mtr"

// Page-limit bounds shared by the two new listings.
const (
	pageMinLimit     = 1
	pageMaxLimit     = 500
	pageDefaultLimit = 100
)

// parsePageLimit mirrors parseEventsLimit: an unparseable ?limit= is treated
// as unset, never a 400 -- limit is documented to clamp.
func parsePageLimit(raw string) int {
	if raw == "" {
		return 0
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0
	}
	return n
}

// clampPageLimit mirrors clampEventsLimit: 0 (unset) is the default,
// everything else clamps into [pageMinLimit, pageMaxLimit].
func clampPageLimit(n int) int {
	switch {
	case n == 0:
		return pageDefaultLimit
	case n < pageMinLimit:
		return pageMinLimit
	case n > pageMaxLimit:
		return pageMaxLimit
	default:
		return n
	}
}

// mtrUnavailable answers 503 and reports true when no MTRService is wired,
// the same shape targetsUnavailable uses.
func (s *Server) mtrUnavailable(w http.ResponseWriter) bool {
	if s.mtr == nil {
		writeProblem(w, http.StatusServiceUnavailable, "MTR path history not available", mtrUnavailableDetail)
		return true
	}
	return false
}

// mtrDestinationResponse is one (source, destination) pair on the wire.
type mtrDestinationResponse struct {
	SourceNode  string `json:"sourceNode"`
	Destination string `json:"destination"`
	// SnapshotCount is how many DISTINCT routes the pair has taken;
	// TraceCount is how many traces produced them. A pair with snapshotCount
	// 1 and traceCount 4000 has a stable route.
	SnapshotCount int64     `json:"snapshotCount"`
	TraceCount    int64     `json:"traceCount"`
	FirstSeen     time.Time `json:"firstSeen"`
	LastSeen      time.Time `json:"lastSeen"`
}

// mtrDestinationsResponse envelopes the (unpaged) pair list; an envelope rather than a bare array,
// matching every other list body in this API (targets/checks/schedules/tokens).
type mtrDestinationsResponse struct {
	Destinations []mtrDestinationResponse `json:"destinations"`
}

// mtrSnapshotResponse is one persisted route on the wire; hops is passed through as raw JSON, never
// re-marshalled through []store.PathHop.
type mtrSnapshotResponse struct {
	ID          string          `json:"id"`
	SourceNode  string          `json:"sourceNode"`
	Destination string          `json:"destination"`
	PathHash    string          `json:"pathHash"`
	HopCount    int32           `json:"hopCount"`
	Hops        json.RawMessage `json:"hops"`
	FirstSeen   time.Time       `json:"firstSeen"`
	LastSeen    time.Time       `json:"lastSeen"`
	TraceCount  int64           `json:"traceCount"`
	// RunID is omitted once the run that first produced this path has aged
	// out of check_runs (ON DELETE SET NULL, migration 00005) -- a path
	// outliving its run is the point of path history.
	RunID string `json:"runId,omitempty"`
}

func mtrSnapshotResponseFrom(s *store.PathSnapshot) mtrSnapshotResponse {
	hops := s.Hops
	if len(hops) == 0 {
		// Defensive: the projector never writes a hopless snapshot (store's
		// own Validate refuses one), but a nil here would marshal as JSON
		// null and the frontend iterates hops as an array.
		hops = json.RawMessage(`[]`)
	}
	return mtrSnapshotResponse{
		ID: s.ID, SourceNode: s.SourceNode, Destination: s.Destination,
		PathHash: s.PathHash, HopCount: s.HopCount, Hops: hops,
		FirstSeen: s.FirstSeen, LastSeen: s.LastSeen, TraceCount: s.TraceCount, RunID: s.RunID,
	}
}

// mtrSnapshotsResponse is GET /api/v1/mtr/snapshots's body -- the same
// keyset-cursor shape as targetsListResponse.
type mtrSnapshotsResponse struct {
	Snapshots  []mtrSnapshotResponse `json:"snapshots"`
	NextCursor string                `json:"nextCursor"`
}

// mtrEnrichmentResponse is one cached lookup about a hop address.
type mtrEnrichmentResponse struct {
	IP         string          `json:"ip"`
	RDNS       string          `json:"rdns,omitempty"`
	ASN        int64           `json:"asn,omitempty"`
	Provider   string          `json:"provider,omitempty"`
	Geo        json.RawMessage `json:"geo,omitempty"`
	ResolvedAt time.Time       `json:"resolvedAt"`
}

// mtrSnapshotDetailResponse is GET /api/v1/mtr/snapshots/{id}?enrich=true's body: the snapshot;
// WITHOUT ?enrich=true the handler serves the bare mtrSnapshotResponse.
type mtrSnapshotDetailResponse struct {
	mtrSnapshotResponse
	Enrichment map[string]mtrEnrichmentResponse `json:"enrichment"`
}

// snapshotIDFrom resolves the {id} path parameter, answering 404 and reporting false for anything
// that is not a canonical UUID.
func snapshotIDFrom(w http.ResponseWriter, r *http.Request) (string, bool) {
	id := chi.URLParam(r, "id")
	if _, err := uuid.Parse(id); err != nil {
		writeProblem(w, http.StatusNotFound, "path snapshot not found", "no path snapshot with that id")
		return "", false
	}
	return id, true
}

// handleMTRDestinations serves every (source, destination) pair path history
// knows about, most-recently-traced first. Unpaged on purpose: the row count
// is PAIRS, not traces.
func (s *Server) handleMTRDestinations(w http.ResponseWriter, r *http.Request) {
	if s.mtrUnavailable(w) {
		return
	}
	dests, err := s.mtr.ListMTRDestinations(r.Context())
	if err != nil {
		// Logged, never surfaced: the driver error can carry a DSN or other
		// upstream detail that has no business in an HTTP response body.
		slog.Error("list mtr destinations failed", "error", err)
		writeProblem(w, http.StatusBadGateway, "MTR path history unavailable", "failed to query MTR destinations")
		return
	}
	out := make([]mtrDestinationResponse, 0, len(dests))
	for i := range dests {
		out = append(out, mtrDestinationResponse{
			SourceNode: dests[i].SourceNode, Destination: dests[i].Destination,
			SnapshotCount: dests[i].SnapshotCount, TraceCount: dests[i].TraceCount,
			FirstSeen: dests[i].FirstSeen, LastSeen: dests[i].LastSeen,
		})
	}
	writeJSON(w, mtrDestinationsResponse{Destinations: out})
}

// handleMTRSnapshots serves one pair's route history; BOTH source and destination are required.
func (s *Server) handleMTRSnapshots(w http.ResponseWriter, r *http.Request) {
	if s.mtrUnavailable(w) {
		return
	}
	q := r.URL.Query()

	source, destination := q.Get("source"), q.Get("destination")
	if source == "" || destination == "" {
		writeProblem(w, http.StatusUnprocessableEntity, "invalid filter",
			"both source and destination are required: an unfiltered snapshot listing has no bound "+
				"(list the pairs with GET /api/v1/mtr/destinations first)")
		return
	}

	cursor := q.Get("cursor")
	if cursor != "" {
		// Validated up front, like handleEvents does: a decode failure must
		// answer 400, never reach the store and be reported as the generic
		// 502 below, and never silently restart pagination from the top.
		if _, _, _, err := store.DecodeUUIDCursor(cursor); err != nil {
			writeProblem(w, http.StatusBadRequest, "invalid cursor", "cursor is malformed or does not match this server")
			return
		}
	}

	page, err := s.mtr.ListPathSnapshots(r.Context(), store.SnapshotFilter{
		SourceNode:  source,
		Destination: destination,
		Cursor:      cursor,
		Limit:       clampPageLimit(parsePageLimit(q.Get("limit"))),
	})
	if err != nil {
		slog.Error("list path snapshots failed", "error", err)
		writeProblem(w, http.StatusBadGateway, "MTR path history unavailable", "failed to query path snapshots")
		return
	}

	out := make([]mtrSnapshotResponse, 0, len(page.Snapshots))
	for i := range page.Snapshots {
		out = append(out, mtrSnapshotResponseFrom(&page.Snapshots[i]))
	}
	writeJSON(w, mtrSnapshotsResponse{Snapshots: out, NextCursor: page.NextCursor})
}

// handleMTRSnapshotGet serves one stored route.
func (s *Server) handleMTRSnapshotGet(w http.ResponseWriter, r *http.Request) {
	if s.mtrUnavailable(w) {
		return
	}
	id, ok := snapshotIDFrom(w, r)
	if !ok {
		return
	}

	snap, err := s.mtr.GetPathSnapshot(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeProblem(w, http.StatusNotFound, "path snapshot not found", "no path snapshot with that id")
			return
		}
		slog.Error("get path snapshot failed", "snapshot", id, "error", err) //nolint:gosec // G706: structured slog fields, not string-built log injection
		writeProblem(w, http.StatusBadGateway, "MTR path history unavailable", "failed to query the path snapshot")
		return
	}

	body := mtrSnapshotResponseFrom(&snap)
	if r.URL.Query().Get("enrich") != "true" {
		writeJSON(w, body)
		return
	}
	writeJSON(w, mtrSnapshotDetailResponse{mtrSnapshotResponse: body, Enrichment: s.enrichHops(r.Context(), &snap)})
}

// Both are EnrichmentReader, so the handler below is byte-identical either way and the resolver
// still cannot write the cache through this interface.
func (s *Server) enrichmentReader() EnrichmentReader {
	if s.enricher != nil {
		return s.enricher
	}
	return s.mtr
}

// enrichHops looks the snapshot's hop addresses up through enrichmentReader.
func (s *Server) enrichHops(ctx context.Context, snap *store.PathSnapshot) map[string]mtrEnrichmentResponse {
	out := map[string]mtrEnrichmentResponse{}

	hops, err := store.DecodeHops(snap.Hops)
	if err != nil {
		slog.Warn("decode snapshot hops for enrichment failed", "snapshot", snap.ID, "error", err) //nolint:gosec // G706: structured slog fields
		return out
	}
	ips := store.HopIPs(hops)
	if len(ips) == 0 {
		return out
	}

	rows, err := s.enrichmentReader().GetEnrichment(ctx, ips)
	if err != nil {
		slog.Warn("read hop enrichment cache failed", "snapshot", snap.ID, "error", err) //nolint:gosec // G706: structured slog fields
		return out
	}
	for ip, row := range rows {
		out[ip] = mtrEnrichmentResponse{
			IP: row.IP, RDNS: row.RDNS, ASN: row.ASN, Provider: row.Provider,
			Geo: row.Geo, ResolvedAt: row.ResolvedAt,
		}
	}
	return out
}
