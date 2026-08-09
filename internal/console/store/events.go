package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
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
	queryInsertTopologyEvent       = "InsertTopologyEvent"
	queryListTopologyEvents        = "ListTopologyEvents"
	queryOldestTopologyEventTime   = "OldestTopologyEventTime"
	queryListTopologyEventsForFold = "ListTopologyEventsForFold"

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
	queryGetTokenByID             = "GetTokenByID"
	queryCreateToken              = "CreateToken"
	queryListTokens               = "ListTokens"
	queryRevokeToken              = "RevokeToken"
	queryTouchTokenLastUsed       = "TouchTokenLastUsed"
	queryInsertAuditEntry         = "InsertAuditEntry"
	queryListAuditEntries         = "ListAuditEntries"
	queryDeleteAuditEntriesBefore = "DeleteAuditEntriesBefore"

	// The MTR path-history and annotation queries (mtr.go, annotations.go).
	// The Delete*Before sweeps are deliberately absent: the pruner reports
	// its own work through RetentionDeleted{table}, and metering the same
	// DELETEs twice under two different metrics would double-count them.
	queryUpsertPathSnapshot  = "UpsertPathSnapshot"
	queryListMTRDestinations = "ListMTRDestinations"
	queryListPathSnapshots   = "ListPathSnapshots"
	queryGetPathSnapshot     = "GetPathSnapshot"
	queryGetEnrichment       = "GetEnrichment"
	queryPutEnrichment       = "PutEnrichment"
	queryCreateAnnotation    = "CreateAnnotation"
	queryGetAnnotation       = "GetAnnotation"
	queryListAnnotations     = "ListAnnotations"
	queryDeleteAnnotation    = "DeleteAnnotation"

	// The M6 investigation queries (k8sevents.go, incidents.go,
	// maintenance.go, webhooks.go). The Delete*Before sweeps are absent for
	// the same reason M5's are: the pruner reports its own work through
	// RetentionDeleted{table}.
	queryInsertK8sEvent          = "InsertK8sEvent"
	queryListK8sEvents           = "ListK8sEvents"
	queryCreateIncident          = "CreateIncident"
	queryGetIncident             = "GetIncident"
	queryListIncidents           = "ListIncidents"
	queryUpdateIncidentStatus    = "UpdateIncidentStatus"
	queryUpdateIncidentNotes     = "UpdateIncidentNotes"
	queryUpdateIncidentPinned    = "UpdateIncidentPinned"
	queryDeleteIncident          = "DeleteIncident"
	queryCreateMaintenanceWindow = "CreateMaintenanceWindow"
	queryListMaintenanceWindows  = "ListMaintenanceWindows"
	queryDeleteMaintenanceWindow = "DeleteMaintenanceWindow"
	queryCreateWebhook           = "CreateWebhook"
	queryGetWebhook              = "GetWebhook"
	queryListWebhooks            = "ListWebhooks"
	queryUpdateWebhook           = "UpdateWebhook"
	queryUpdateWebhookDelivery   = "UpdateWebhookDelivery"
	queryDeleteWebhook           = "DeleteWebhook"

	// The M7 alerting queries (alertrules.go). alert_rules is configuration
	// and has no sweep at all, so unlike the M5/M6 blocks above there is not
	// even a Delete*Before to leave out.
	queryCreateAlertRule           = "CreateAlertRule"
	queryGetAlertRule              = "GetAlertRule"
	queryListAlertRules            = "ListAlertRules"
	queryUpdateAlertRule           = "UpdateAlertRule"
	queryUpdateAlertRuleSyncStatus = "UpdateAlertRuleSyncStatus"
	queryDeleteAlertRule           = "DeleteAlertRule"

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
//
// Scope and ScopeNode are two different questions about the same column and
// httpapi refuses the pair with a 422 (they would only ever narrow each other).
// Setting both here is therefore not a documented shape, but it is not
// undefined either: the query ANDs them, which is the honest reading of "apply
// every filter you were given".
type EventFilter struct {
	Types []string // OR-ed; empty = all
	Scope string   // exact match; empty = all
	// ScopeNode matches a NAME on either side of the scope: the bare scope
	// itself, or a pair scope ("<src>→<dst>", events.pairScope) naming it as
	// source or destination. This is what an object card asks -- a node's own
	// events plus every check that ran to or from it -- and what Scope, being
	// an equality filter, structurally cannot answer. Empty = no filter. The
	// name is matched LITERALLY: its LIKE metacharacters are escaped in the
	// query, so a target name carrying '_' cannot widen into a wildcard.
	ScopeNode string
	From      time.Time // inclusive; zero = unbounded
	To        time.Time // exclusive; zero = unbounded
	Cursor    string    // opaque keyset cursor from a previous page
	Limit     int
}

// EventPage is one page of ListEvents results.
type EventPage struct {
	Events     []EventRecord
	NextCursor string // "" when the page is the last one
}

// EventStore is the seam every consumer takes. events.Ingester takes only
// InsertEvent (as EventSink); httpapi takes ListEvents for GET /api/v1/events
// and TopologyAt for GET /api/v1/topology?at=.
type EventStore interface {
	InsertEvent(ctx context.Context, rec EventRecord) (inserted bool, err error)
	ListEvents(ctx context.Context, f EventFilter) (EventPage, error)
	TopologyAt(ctx context.Context, at time.Time) (TopologySnapshot, error)
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

	// Passed RAW: the escaping the LIKE branches need happens inside the query
	// (see topology_events.sql), because the same param also feeds the equality
	// branch, where an escaped name would match nothing.
	var scopeNode pgtype.Text
	if f.ScopeNode != "" {
		scopeNode = pgtype.Text{String: f.ScopeNode, Valid: true}
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
		Types:     types,
		Scope:     scope,
		ScopeNode: scopeNode,
		FromTime:  fromTime,
		ToTime:    toTime,
		CurTime:   curTime,
		CurID:     curID,
		Lim:       int32(limit), //nolint:gosec // limit is clamped to [1,500] above
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

// --- topology-at-t (M5 Task 9) -------------------------------------------
//
// WHAT A topology_changed EVENT ACTUALLY CARRIES, and therefore what this
// fold can and cannot reconstruct. The persisted details JSON is exactly
// events.topologyChangedDetails (internal/console/events/live_event.go):
//
//	{"reason": "...", "nodeName": "...", "agentId": "...", "zone": "..."}
//
// reason is the controller registry's own label -- agent_registered,
// zone_updated, agent_deregistered, agent_evicted (api/proto/kconmon.proto,
// internal/controller/registry.go). That is the WHOLE payload:
//
//   - Node/agent IDENTITY is reconstructible ONLY when the event names it.
//     Since M7 the controller attributes every change at its emission site --
//     one event per affected agent, carrying agent id, node and zone -- so
//     current history folds into a real node set. History written BEFORE that
//     (still inside retention on an upgraded console) carries a reason and
//     nothing else: those rows name nobody, and the fold's honest answer for
//     them is to change nothing and count them in UnfoldableEvents. A large
//     UnfoldableEvents next to an empty node set means the window is mostly
//     pre-M7 history, not that the cluster was empty.
//   - ZONE is recorded since M7, including by zone_updated (whose entire
//     subject is the new zone), so folded nodes and agents carry the zone the
//     events last stated. It stays empty for pre-M7 rows, and an event that
//     omits it never ERASES a zone already folded -- see the zone rule in
//     foldTopology.
//   - POD IP is never recorded, at any version. Folded agents always carry an
//     empty PodIP.
//   - READINESS is not recorded. Ready is true for every node the fold has
//     seen registered and not since removed -- "present per the event log",
//     which is the only readiness this history contains.
//
// Removals are a REASON, not a separate event type: agent_deregistered and
// agent_evicted are the two that take a subject out of the set.
const (
	topologyReasonRegistered   = "agent_registered"
	topologyReasonZoneUpdated  = "zone_updated"
	topologyReasonDeregistered = "agent_deregistered"
	topologyReasonEvicted      = "agent_evicted"
)

// eventTypeTopologyChanged mirrors events.TypeTopologyChanged. Duplicated as a
// private const rather than imported: the dependency runs the other way round
// (events.Ingester writes THROUGH this store), and store importing the console's
// event-projection package would invert it.
const eventTypeTopologyChanged = "topology_changed"

// topologyFoldLimit bounds the fold's single query. A fold is only correct
// when it sees EVERY event from the beginning of retention, so this is a
// blast-radius guard, never a page size: hitting it means the answer is
// missing the NEWEST events and TopologySnapshot.Truncated says so. At the
// default 90-day retention, topology churn is orders of magnitude below this
// (an event per agent registration/eviction), so a truncated fold means either
// a pathological flapping cluster or a misconfigured retention -- both worth
// the WARN it logs.
const topologyFoldLimit = 100_000

// TopologyNode is one node in a reconstructed topology. Field-for-field the
// durable twin of controllerclient.Node, so httpapi can serve the same JSON
// keys for a folded snapshot as for the live passthrough. Zone comes from the
// events and is empty for pre-M7 history; Ready is presence-derived -- see the
// block comment above.
type TopologyNode struct {
	Name  string
	Zone  string
	Ready bool
}

// TopologyAgent is one agent in a reconstructed topology, the durable twin of
// controllerclient.Agent. PodIP is ALWAYS empty -- no event ever recorded it.
// Zone comes from the events and is empty for pre-M7 history.
type TopologyAgent struct {
	ID       string
	NodeName string
	Zone     string
	PodIP    string
}

// TopologySnapshot is the result of folding topology_changed events up to an
// instant. Nodes and Agents are never nil (empty slices, sorted by Name/ID) so
// the JSON always carries arrays.
type TopologySnapshot struct {
	Nodes  []TopologyNode
	Agents []TopologyAgent

	// LastChange is the event_time of the newest event folded -- i.e. when the
	// topology last actually changed at or before the requested instant. Zero
	// when no event was folded at all.
	LastChange time.Time

	// OldestRetained is the event_time of the OLDEST row still in
	// topology_events, whatever its type: the retention floor. Zero means the
	// table is empty. A caller asking about an instant before this cannot be
	// answered honestly -- the events that would have built that set are gone.
	OldestRetained time.Time

	// EventsFolded counts every row the fold consumed, including the ones that
	// could not move the set.
	EventsFolded int

	// UnfoldableEvents counts rows that could NOT move the set: they named
	// neither a node nor an agent (every pre-M7 row, none written since),
	// carried unparseable details, or used a reason this build does not know.
	// A large value next to an empty node set is the signal that the history
	// is thin, not that the cluster was empty.
	UnfoldableEvents int

	// Truncated reports that topologyFoldLimit cut the history off, so the
	// snapshot is missing its newest events and must not be trusted.
	Truncated bool
}

// topologyChangeDetails is the details JSON of a topology_changed row, mirrored
// by hand from events.topologyChangedDetails (which is unexported there, and
// store must not import that package anyway).
type topologyChangeDetails struct {
	Reason   string `json:"reason"`
	NodeName string `json:"nodeName"`
	AgentID  string `json:"agentId"`
	Zone     string `json:"zone"`
}

// TopologyAt reconstructs the node/agent set as of at by replaying every
// topology_changed event with event_time <= at in (event_time, id) order.
//
// The returned snapshot ALWAYS carries OldestRetained, even when the fold
// itself is empty: that is what lets the caller tell "nothing had happened
// yet" apart from "the events for that instant have been pruned", which is a
// 422 rather than an empty 200. Both facts come back from this one call so the
// caller needs no second round trip to decide.
//
// See the block comment above for the fold's contract -- what the events do
// and do not record.
func (s *eventStore) TopologyAt(ctx context.Context, at time.Time) (TopologySnapshot, error) {
	oldestStart := time.Now()
	oldest, err := s.q.OldestTopologyEventTime(ctx)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// An empty table is not an error: it is a console whose event history
		// has not started (or has been pruned away entirely). The zero
		// OldestRetained below is what tells the caller that.
		s.observe(queryOldestTopologyEventTime, oldestStart, resultOK)
		oldest = time.Time{}
	case err != nil:
		s.observe(queryOldestTopologyEventTime, oldestStart, resultError)
		return TopologySnapshot{}, fmt.Errorf("store: topology at: oldest event: %w", err)
	default:
		s.observe(queryOldestTopologyEventTime, oldestStart, resultOK)
	}

	foldStart := time.Now()
	rows, err := s.q.ListTopologyEventsForFold(ctx, gen.ListTopologyEventsForFoldParams{
		Type: eventTypeTopologyChanged,
		At:   at,
		Lim:  topologyFoldLimit,
	})
	if err != nil {
		s.observe(queryListTopologyEventsForFold, foldStart, resultError)
		return TopologySnapshot{}, fmt.Errorf("store: topology at: fold: %w", err)
	}
	s.observe(queryListTopologyEventsForFold, foldStart, resultOK)

	// The rows are already (event_time, id) ascending -- the fold is a pure
	// function of that order, so it is reused verbatim by the unit tests over
	// synthetic records.
	recs := make([]EventRecord, len(rows))
	for i := range rows {
		recs[i] = EventRecord{
			EventTime: rows[i].EventTime,
			Type:      eventTypeTopologyChanged,
			Details:   rows[i].Details,
		}
	}

	snap := foldTopology(recs)
	snap.OldestRetained = oldest
	if len(rows) == topologyFoldLimit {
		snap.Truncated = true
		slog.Warn("topology fold hit its row limit; the reconstructed topology is missing its newest events",
			"limit", topologyFoldLimit, "at", at)
	}
	return snap, nil
}

// foldTopology replays recs -- assumed already ordered (event_time, id)
// ascending -- into the node/agent set they describe. Pure: no clock, no I/O,
// no package state, so the whole contract is unit-testable over synthetic
// records. OldestRetained is NOT set here; only the caller knows it.
func foldTopology(recs []EventRecord) TopologySnapshot {
	// Membership plus the last placement each subject was seen with. Zone is
	// carried rather than recomputed because only the event that mentions it
	// knows it -- see the zone rule inside the loop.
	type placement struct {
		nodeName string
		zone     string
	}
	nodes := map[string]string{}     // node name -> zone last stated for it
	agents := map[string]placement{} // agent id -> where it was last seen

	snap := TopologySnapshot{EventsFolded: len(recs)}
	for i := range recs {
		rec := &recs[i]
		snap.LastChange = rec.EventTime

		var d topologyChangeDetails
		if err := json.Unmarshal(rec.Details, &d); err != nil {
			// Corrupt or foreign details: counted, never guessed at. No log --
			// one bad row would otherwise log once per request forever.
			snap.UnfoldableEvents++
			continue
		}
		if d.NodeName == "" && d.AgentID == "" {
			snap.UnfoldableEvents++
			continue
		}

		switch d.Reason {
		case topologyReasonRegistered, topologyReasonZoneUpdated:
			// The zone rule: an event that states one WINS (zone_updated
			// exists precisely to restate it), an event that omits one leaves
			// what is already known ALONE. Overwriting with "" would blank
			// zones at random across the mixed history an upgraded console
			// holds -- pre-M7 rows carry no zone key at all -- while ignoring
			// a stated zone would make a relabel invisible.
			if d.NodeName != "" {
				if _, seen := nodes[d.NodeName]; d.Zone != "" || !seen {
					nodes[d.NodeName] = d.Zone
				}
			}
			if d.AgentID != "" {
				zone := d.Zone
				if zone == "" {
					zone = agents[d.AgentID].zone
				}
				agents[d.AgentID] = placement{nodeName: d.NodeName, zone: zone}
			}
		case topologyReasonDeregistered, topologyReasonEvicted:
			// Removing something absent is a no-op, not an error: retention
			// can cut a subject's registration away and keep its removal.
			delete(nodes, d.NodeName)
			delete(agents, d.AgentID)
		default:
			// A reason a newer controller invented. Folding it in either
			// direction would be a guess, so membership is left untouched.
			snap.UnfoldableEvents++
		}
	}

	snap.Nodes = make([]TopologyNode, 0, len(nodes))
	for name, zone := range nodes {
		// Ready is presence-derived, and Zone is whatever the events said --
		// empty for history a pre-M7 controller wrote. See the block comment
		// on this section.
		snap.Nodes = append(snap.Nodes, TopologyNode{Name: name, Zone: zone, Ready: true})
	}
	sort.Slice(snap.Nodes, func(i, j int) bool { return snap.Nodes[i].Name < snap.Nodes[j].Name })

	snap.Agents = make([]TopologyAgent, 0, len(agents))
	for id, p := range agents {
		snap.Agents = append(snap.Agents, TopologyAgent{ID: id, NodeName: p.nodeName, Zone: p.zone})
	}
	sort.Slice(snap.Agents, func(i, j int) bool { return snap.Agents[i].ID < snap.Agents[j].ID })

	return snap
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
