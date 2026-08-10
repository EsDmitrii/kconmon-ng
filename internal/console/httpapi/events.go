package httpapi

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/EsDmitrii/kconmon-ng/internal/console/events"
	"github.com/EsDmitrii/kconmon-ng/internal/console/store"
)

// EventLister is the read half of store.EventStore, narrowed so httpapi cannot
// write. store.EventStore's method set is a superset of this one, so the value
// store.NewEventStore returns assigns here directly, with no cast.
type EventLister interface {
	ListEvents(ctx context.Context, f store.EventFilter) (store.EventPage, error)
}

// Limit bounds for GET /api/v1/events, mirroring store.EventFilter.Limit's own contract.
const (
	eventsMinLimit     = 1
	eventsMaxLimit     = 500
	eventsDefaultLimit = 100
)

// knownEventTypes is the closed set events.Type* declares. A ?type= value
// outside it can never match a row, so it is rejected as a likely typo rather
// than silently returning an empty page.
var knownEventTypes = map[string]bool{
	events.TypeTopologyChanged:    true,
	events.TypeCheckObserved:      true,
	events.TypeMTRTriggered:       true,
	events.TypeMTRCompleted:       true,
	events.TypeDiagnosticProgress: true,
}

// eventsResponse is GET /api/v1/events's body. Events is never nil — built
// with make(..., 0, ...) below — because the frontend indexes into it the
// same way it does /api/v1/version's capabilities.
type eventsResponse struct {
	Events     []events.LiveEvent `json:"events"`
	NextCursor string             `json:"nextCursor"`
}

// handleEvents serves one page of persisted controller events, newest first, behind an opaque
// keyset cursor.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	if s.events == nil {
		writeProblem(w, http.StatusServiceUnavailable, "event history not available",
			"set console.database.mode in the console config (Helm: console.database.mode) to enable GET /api/v1/events")
		return
	}

	q := r.URL.Query()

	types := q["type"]
	for _, t := range types {
		if !knownEventTypes[t] {
			writeProblem(w, http.StatusBadRequest, "invalid type", fmt.Sprintf("unknown event type %q", t))
			return
		}
	}

	// scope and scopeNode answer different questions about the same column.
	scope := q.Get("scope")
	scopeNode := q.Get("scopeNode")
	if scope != "" && scopeNode != "" {
		writeProblem(w, http.StatusUnprocessableEntity, "conflicting scope filters",
			"scope and scopeNode are mutually exclusive: scope matches the event's scope exactly, "+
				"scopeNode matches a node/target name on either side of a pair scope")
		return
	}

	from, ok := parseEventsTime(w, q.Get("from"), "from")
	if !ok {
		return
	}
	to, ok := parseEventsTime(w, q.Get("to"), "to")
	if !ok {
		return
	}
	if !from.IsZero() && !to.IsZero() && !from.Before(to) {
		writeProblem(w, http.StatusBadRequest, "invalid range", "from must be before to")
		return
	}

	cursor := q.Get("cursor")
	if cursor != "" {
		// Validated here, up front: a decode failure must answer 400, never
		// reach the lister and be treated as the generic 502 "lister error"
		// case below, and never silently restart pagination from the top.
		if _, _, _, err := store.DecodeCursor(cursor); err != nil {
			writeProblem(w, http.StatusBadRequest, "invalid cursor", "cursor is malformed or does not match this server")
			return
		}
	}

	filter := store.EventFilter{
		Types:     types,
		Scope:     scope,
		ScopeNode: scopeNode,
		From:      from,
		To:        to,
		Cursor:    cursor,
		Limit:     clampEventsLimit(parseEventsLimit(q.Get("limit"))),
	}

	page, err := s.events.ListEvents(r.Context(), filter)
	if err != nil {
		// Logged, never surfaced: the driver error can carry a DSN or other
		// upstream detail that has no business in an HTTP response body.
		slog.Error("list events failed", "error", err)
		writeProblem(w, http.StatusBadGateway, "event history unavailable", "failed to query event history")
		return
	}

	out := make([]events.LiveEvent, 0, len(page.Events))
	for i := range page.Events {
		out = append(out, toLiveEvent(&page.Events[i]))
	}
	writeJSON(w, eventsResponse{Events: out, NextCursor: page.NextCursor})
}

// parseEventsTime parses an RFC3339 query param.
func parseEventsTime(w http.ResponseWriter, raw, field string) (t time.Time, ok bool) {
	if raw == "" {
		return time.Time{}, true
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid "+field, field+" must be RFC3339")
		return time.Time{}, false
	}
	return t, true
}

// parseEventsLimit parses ?limit=. Anything that fails to parse is treated as
// unset (0): limit is documented to clamp, never to 400.
func parseEventsLimit(raw string) int {
	if raw == "" {
		return 0
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0
	}
	return n
}

// clampEventsLimit mirrors store.EventFilter.Limit's contract so the fake (and
// the real store) are asked for the same number: 0 is "unset", clamped to the
// default; everything else clamps into [eventsMinLimit, eventsMaxLimit].
func clampEventsLimit(n int) int {
	switch {
	case n == 0:
		return eventsDefaultLimit
	case n < eventsMinLimit:
		return eventsMinLimit
	case n > eventsMaxLimit:
		return eventsMaxLimit
	default:
		return n
	}
}

// toLiveEvent projects a persisted EventRecord onto the exact shape events.ToLiveEvent produces for
// the "live" WebSocket topic (same field names, RFC3339 timestamp, raw JSON details).
func toLiveEvent(rec *store.EventRecord) events.LiveEvent {
	return events.LiveEvent{
		ID:        fmt.Sprintf("%d-%d", rec.EventSeq, rec.EventTime.UnixNano()),
		Seq:       uint64(rec.EventSeq), //nolint:gosec // EventSeq is a controller-assigned monotonic sequence, always >= 0
		Type:      rec.Type,
		Severity:  rec.Severity,
		Scope:     rec.Scope,
		Timestamp: rec.EventTime,
		Summary:   rec.Summary,
		Details:   rec.Details,
	}
}
