package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/EsDmitrii/kconmon-ng/internal/console/metrics"
	"github.com/EsDmitrii/kconmon-ng/internal/console/store/gen"
)

// Limit bounds for EventFilter.Limit: zero means "unset, use defaultLimit";
// anything else is clamped into [minLimit, maxLimit] so one caller cannot
// force an unbounded table scan through the API.
const (
	minLimit     = 1
	maxLimit     = 500
	defaultLimit = 100
)

// Query and result labels for the store metrics below. query is the closed
// set of generated gen.Queries method names this package actually calls;
// result is ok|conflict|error. Never widen either set with per-call data
// (table names, row counts, user or run IDs).
const (
	queryInsertTopologyEvent = "InsertTopologyEvent"
	queryListTopologyEvents  = "ListTopologyEvents"

	// The auth-path queries (auth.go): users, roles and role bindings, API
	// tokens, audit log. Every one of *DB's auth.go methods is metered, so
	// this list and the set of gen.Queries calls that file makes must stay
	// exactly in step.
	queryGetUserByID              = "GetUserByID"
	queryGetUserByUsername        = "GetUserByUsername"
	queryCreateUser               = "CreateUser"
	queryUpdateUserPassword       = "UpdateUserPassword"
	queryListUsers                = "ListUsers"
	queryCountUsers               = "CountUsers"
	querySetUserDisabled          = "SetUserDisabled"
	queryListRoles                = "ListRoles"
	queryUpsertRole               = "UpsertRole"
	queryDeleteRole               = "DeleteRole"
	queryListBindingsForSubject   = "ListBindingsForSubject"
	queryListBindings             = "ListBindings"
	queryCreateBinding            = "CreateBinding"
	queryDeleteBinding            = "DeleteBinding"
	queryGetTokenByHash           = "GetTokenByHash"
	queryCreateToken              = "CreateToken"
	queryListTokens               = "ListTokens"
	queryRevokeToken              = "RevokeToken"
	queryTouchTokenLastUsed       = "TouchTokenLastUsed"
	queryInsertAuditEntry         = "InsertAuditEntry"
	queryListAuditEntries         = "ListAuditEntries"
	queryDeleteAuditEntriesBefore = "DeleteAuditEntriesBefore"

	resultOK       = "ok"
	resultConflict = "conflict"
	resultError    = "error"
)

// EventRecord is one persisted controller event. Field-for-field the durable
// twin of events.LiveEvent (internal/console/events/live_event.go:45-58) minus
// the derived ID, which is (EventSeq, EventTime).
type EventRecord struct {
	EventSeq  int64
	EventTime time.Time
	Type      string
	Severity  string
	Scope     string
	Summary   string
	Details   json.RawMessage
}

// EventFilter selects a page. All fields optional; Limit is clamped to [1,500].
type EventFilter struct {
	Types  []string  // OR-ed; empty = all
	Scope  string    // exact match; empty = all
	From   time.Time // inclusive; zero = unbounded
	To     time.Time // exclusive; zero = unbounded
	Cursor string    // opaque keyset cursor from a previous page
	Limit  int
}

// EventPage is one page of ListEvents results.
type EventPage struct {
	Events     []EventRecord
	NextCursor string // "" when the page is the last one
}

// EventStore is the seam every consumer takes. events.Ingester takes only
// InsertEvent (as EventSink); httpapi takes only ListEvents.
type EventStore interface {
	InsertEvent(ctx context.Context, rec EventRecord) (inserted bool, err error)
	ListEvents(ctx context.Context, f EventFilter) (EventPage, error)
}

// eventStore is the pgx/sqlc-backed EventStore implementation.
type eventStore struct {
	q *gen.Queries
	m *metrics.Metrics
}

// NewEventStore returns an EventStore backed by db's connection pool. m must
// not be nil: every query is metered, with no way to opt out.
func NewEventStore(db *DB, m *metrics.Metrics) EventStore {
	return &eventStore{q: gen.New(db.pool), m: m}
}

// observe records the metrics timer common to every query: duration by query
// name, and a query x result counter. Called for every call this store makes,
// success or failure, which is what makes StoreQueries a true call count.
func (s *eventStore) observe(query string, start time.Time, result string) {
	s.m.StoreQueryDuration.WithLabelValues(query).Observe(time.Since(start).Seconds())
	s.m.StoreQueries.WithLabelValues(query, result).Inc()
}

// InsertEvent persists rec. inserted is false, with a nil error, exactly when
// ON CONFLICT DO NOTHING fired because another replica already wrote this
// (EventSeq, EventTime) pair -- the normal multi-replica case, which callers
// must not log as an error.
func (s *eventStore) InsertEvent(ctx context.Context, rec EventRecord) (bool, error) { //nolint:gocritic // hugeParam: EventStore is the pinned public interface (task-3-brief.md), value semantics intentional
	start := time.Now()
	rows, err := s.q.InsertTopologyEvent(ctx, gen.InsertTopologyEventParams{
		EventSeq:  rec.EventSeq,
		EventTime: rec.EventTime,
		Type:      rec.Type,
		Severity:  rec.Severity,
		Scope:     rec.Scope,
		Summary:   rec.Summary,
		Details:   rec.Details,
	})
	if err != nil {
		s.observe(queryInsertTopologyEvent, start, resultError)
		return false, fmt.Errorf("store: insert event: %w", err)
	}

	inserted := rows > 0
	result := resultOK
	if !inserted {
		result = resultConflict
	}
	// EventsPersisted is owned by the ingester's sink path (the one caller
	// that knows an insert was a persistence attempt, not a query); counting
	// it here too would double-increment once the sink is wired.
	s.observe(queryInsertTopologyEvent, start, result)
	return inserted, nil
}

// ListEvents returns one page matching f, newest first. NextCursor is set
// only when the page came back exactly as full as requested: a short page
// proves there is nothing older left to return, so encoding a cursor for it
// would only earn the caller one guaranteed-empty extra round trip.
func (s *eventStore) ListEvents(ctx context.Context, f EventFilter) (EventPage, error) { //nolint:gocritic // hugeParam: EventStore is the pinned public interface (task-3-brief.md), value semantics intentional
	limit := clampLimit(f.Limit)

	var curTime pgtype.Timestamptz
	var curID pgtype.Int8
	if f.Cursor != "" {
		ts, id, ok, err := DecodeCursor(f.Cursor)
		if err != nil {
			return EventPage{}, fmt.Errorf("store: list events: %w", err)
		}
		if ok {
			curTime = pgtype.Timestamptz{Time: ts, Valid: true}
			curID = pgtype.Int8{Int64: id, Valid: true}
		}
	}

	// A nil (not merely empty) slice is what makes sqlc.narg('types') bind
	// SQL NULL instead of an empty array literal, which is what the query's
	// "types IS NULL OR ..." clause needs to mean "no type filter".
	var types []string
	if len(f.Types) > 0 {
		types = f.Types
	}

	var scope pgtype.Text
	if f.Scope != "" {
		scope = pgtype.Text{String: f.Scope, Valid: true}
	}

	var fromTime, toTime pgtype.Timestamptz
	if !f.From.IsZero() {
		fromTime = pgtype.Timestamptz{Time: f.From, Valid: true}
	}
	if !f.To.IsZero() {
		toTime = pgtype.Timestamptz{Time: f.To, Valid: true}
	}

	start := time.Now()
	rows, err := s.q.ListTopologyEvents(ctx, gen.ListTopologyEventsParams{
		Types:    types,
		Scope:    scope,
		FromTime: fromTime,
		ToTime:   toTime,
		CurTime:  curTime,
		CurID:    curID,
		Lim:      int32(limit), //nolint:gosec // limit is clamped to [1,500] above
	})
	if err != nil {
		s.observe(queryListTopologyEvents, start, resultError)
		return EventPage{}, fmt.Errorf("store: list events: %w", err)
	}
	s.observe(queryListTopologyEvents, start, resultOK)

	events := make([]EventRecord, len(rows))
	for i := range rows {
		r := &rows[i]
		events[i] = EventRecord{
			EventSeq:  r.EventSeq,
			EventTime: r.EventTime,
			Type:      r.Type,
			Severity:  r.Severity,
			Scope:     r.Scope,
			Summary:   r.Summary,
			Details:   r.Details,
		}
	}

	var nextCursor string
	if len(rows) == limit {
		last := rows[len(rows)-1]
		nextCursor = EncodeCursor(last.EventTime, last.ID)
	}

	return EventPage{Events: events, NextCursor: nextCursor}, nil
}

// clampLimit applies EventFilter.Limit's documented contract: 0 defaults to
// defaultLimit, everything else is clamped into [minLimit, maxLimit].
func clampLimit(limit int) int {
	if limit == 0 {
		return defaultLimit
	}
	if limit < minLimit {
		return minLimit
	}
	if limit > maxLimit {
		return maxLimit
	}
	return limit
}
