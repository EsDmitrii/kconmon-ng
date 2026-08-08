package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/EsDmitrii/kconmon-ng/internal/console/store/gen"
)

// Bounds for a captured Kubernetes event. They exist so a malformed or hostile
// payload cannot turn one row into a megabyte, NOT to re-implement the
// apiserver's own validation -- every one of them is set well past what a real
// event carries, so the capture never silently drops something the cluster
// genuinely emitted.
//
// The name/namespace bounds are the DNS-subdomain maximum a Kubernetes object
// name can actually reach (253); the rest are generous round numbers.
const (
	k8sUIDMaxLen        = 255
	k8sResourceVerLen   = 255
	k8sObjectNameMaxLen = 253
	k8sReasonMaxLen     = 255
	k8sTypeMaxLen       = 64
	k8sMessageMaxLen    = 8192
)

// k8sEventKinds is the closed set of involvedObject kinds this table accepts,
// and it is closed because M6 Decision 3 makes it so: the reader captures node
// events for nodes in the fleet topology and pod events from the release
// namespace, and nothing else. A third kind arriving here is a bug in the
// filter, not a new data source, so it is rejected loudly rather than stored.
//
// K8sEventInput.Type is deliberately NOT closed the same way. core/v1's own
// documentation says new event types may be added in the future, so pinning it
// to {Normal, Warning} would eventually drop real events for being new; the
// length bound is the whole of its validation.
var k8sEventKinds = map[string]bool{"Node": true, "Pod": true}

// K8sEvent is one captured Kubernetes event row. It is a CAPTURE, never an
// authority -- the cluster's own event log is (migration 00006).
type K8sEvent struct {
	ID int64
	// UID and ResourceVersion are the dedupe key TOGETHER. A Kubernetes Event
	// is mutable: a recurring reason keeps its uid and comes back with a
	// bumped Count and a new ResourceVersion, so each revision is its own row
	// and the timeline can show the recurrence.
	UID             string
	ResourceVersion string
	EventTime       time.Time
	// Kind is Node or Pod; Name is the node or pod name -- the same
	// metric-safe label the rest of the system uses, never an address.
	Kind      string
	Name      string
	Namespace string // "" for the cluster-scoped Node events
	Reason    string
	Type      string
	Message   string
	Count     int32
}

// K8sEventInput is InsertK8sEvent's write payload: a K8sEvent minus the
// database-assigned ID.
type K8sEventInput struct {
	UID             string
	ResourceVersion string
	EventTime       time.Time
	Kind            string
	Name            string
	Namespace       string
	Reason          string
	Type            string
	Message         string
	Count           int32
}

// K8sEventFilter selects a page of captured events. All fields optional; Limit
// is clamped to [1,500] the same way EventFilter.Limit is.
type K8sEventFilter struct {
	Name string // node or pod name; exact match, empty = all
	Kind string // Node | Pod; exact match, empty = all
	Type string // Normal | Warning; exact match, empty = all
	// From and To bound event_time as a half-open window, [From, To). Zero on
	// either side is unbounded.
	From   time.Time
	To     time.Time
	Cursor string // opaque keyset cursor from a previous page
	Limit  int
}

// K8sEventPage is one page of ListK8sEvents results.
type K8sEventPage struct {
	Events     []K8sEvent
	NextCursor string // "" when the page is the last one
}

// K8sEventStore is the write seam: internal/console/kubectx (M6 Task 2) is its
// only caller.
type K8sEventStore interface {
	// InsertK8sEvent records one captured event. inserted is false, with a nil
	// error, exactly when the (uid, resourceVersion) revision was already
	// stored -- the normal outcome of the reader's relist-on-watch-expiry
	// loop, which callers must not log as an error. Same contract as
	// EventStore.InsertEvent.
	InsertK8sEvent(ctx context.Context, in K8sEventInput) (inserted bool, err error)
}

var _ K8sEventStore = (*DB)(nil)

// K8sEventReader is the read seam: httpapi's GET /api/v1/k8s-events, which the
// Investigate timeline calls as one of its sources.
type K8sEventReader interface {
	ListK8sEvents(ctx context.Context, f K8sEventFilter) (K8sEventPage, error)
}

var _ K8sEventReader = (*DB)(nil)

// Validate reports whether in is a well-formed captured event. It runs before
// the INSERT so the reader gets a precise error it can count as
// K8sEvents{result="error"} instead of a raw constraint violation.
func (in *K8sEventInput) Validate() error {
	if in.UID == "" {
		return errors.New("store: k8s event: uid must not be empty")
	}
	if len(in.UID) > k8sUIDMaxLen {
		return fmt.Errorf("store: k8s event: uid is %d bytes, limit is %d", len(in.UID), k8sUIDMaxLen)
	}
	if in.ResourceVersion == "" {
		return errors.New("store: k8s event: resource version must not be empty")
	}
	if len(in.ResourceVersion) > k8sResourceVerLen {
		return fmt.Errorf("store: k8s event: resource version is %d bytes, limit is %d",
			len(in.ResourceVersion), k8sResourceVerLen)
	}
	if in.EventTime.IsZero() {
		return errors.New("store: k8s event: event time must not be zero")
	}
	if !k8sEventKinds[in.Kind] {
		return fmt.Errorf("store: k8s event: kind %q must be one of Node, Pod", in.Kind)
	}
	if in.Name == "" {
		return errors.New("store: k8s event: name must not be empty")
	}
	if len(in.Name) > k8sObjectNameMaxLen {
		return fmt.Errorf("store: k8s event: name is %d bytes, limit is %d", len(in.Name), k8sObjectNameMaxLen)
	}
	if len(in.Namespace) > k8sObjectNameMaxLen {
		return fmt.Errorf("store: k8s event: namespace is %d bytes, limit is %d",
			len(in.Namespace), k8sObjectNameMaxLen)
	}
	if len(in.Reason) > k8sReasonMaxLen {
		return fmt.Errorf("store: k8s event: reason is %d bytes, limit is %d", len(in.Reason), k8sReasonMaxLen)
	}
	if in.Type == "" {
		return errors.New("store: k8s event: type must not be empty")
	}
	if len(in.Type) > k8sTypeMaxLen {
		return fmt.Errorf("store: k8s event: type is %d bytes, limit is %d", len(in.Type), k8sTypeMaxLen)
	}
	if len(in.Message) > k8sMessageMaxLen {
		return fmt.Errorf("store: k8s event: message is %d bytes, limit is %d", len(in.Message), k8sMessageMaxLen)
	}
	if in.Count < 0 {
		return fmt.Errorf("store: k8s event: count %d must not be negative", in.Count)
	}
	return nil
}

func k8sEventFromRow(e *gen.K8sEvent) K8sEvent {
	return K8sEvent{
		ID:              e.ID,
		UID:             e.Uid,
		ResourceVersion: e.ResourceVer,
		EventTime:       e.EventTime,
		Kind:            e.Kind,
		Name:            e.Name,
		Namespace:       e.Namespace,
		Reason:          e.Reason,
		Type:            e.Type,
		Message:         e.Message,
		Count:           e.Count,
	}
}

// InsertK8sEvent persists in. A zero Count is stored as 1 -- the column's own
// DEFAULT and the value core/v1 uses for a first occurrence -- so a caller
// that leaves the field unset does not write "this happened zero times".
func (db *DB) InsertK8sEvent(ctx context.Context, in K8sEventInput) (bool, error) { //nolint:gocritic // hugeParam: K8sEventInput mirrors the other write-payload structs in this package
	if err := in.Validate(); err != nil {
		return false, err
	}
	count := in.Count
	if count == 0 {
		count = 1
	}

	start := time.Now()
	rows, err := gen.New(db.pool).InsertK8sEvent(ctx, gen.InsertK8sEventParams{
		Uid:         in.UID,
		ResourceVer: in.ResourceVersion,
		EventTime:   in.EventTime,
		Kind:        in.Kind,
		Name:        in.Name,
		Namespace:   in.Namespace,
		Reason:      in.Reason,
		Type:        in.Type,
		Message:     in.Message,
		Count:       count,
	})
	if err != nil {
		db.observe(queryInsertK8sEvent, start, resultError)
		return false, fmt.Errorf("store: insert k8s event: %w", err)
	}

	inserted := rows > 0
	result := resultOK
	if !inserted {
		result = resultConflict
	}
	db.observe(queryInsertK8sEvent, start, result)
	return inserted, nil
}

// ListK8sEvents returns one page matching f, newest first. NextCursor is set
// only when the page came back exactly as full as requested -- ListEvents'
// reasoning, and the same (event_time, id) bigint cursor codec.
func (db *DB) ListK8sEvents(ctx context.Context, f K8sEventFilter) (K8sEventPage, error) { //nolint:gocritic // hugeParam: K8sEventFilter mirrors EventFilter's value semantics (events.go)
	limit := clampLimit(f.Limit)

	var curTime pgtype.Timestamptz
	var curID pgtype.Int8
	if f.Cursor != "" {
		ts, id, ok, err := DecodeCursor(f.Cursor)
		if err != nil {
			return K8sEventPage{}, fmt.Errorf("store: list k8s events: %w", err)
		}
		if ok {
			curTime = pgtype.Timestamptz{Time: ts, Valid: true}
			curID = pgtype.Int8{Int64: id, Valid: true}
		}
	}

	var name, kind, evType pgtype.Text
	if f.Name != "" {
		name = pgtype.Text{String: f.Name, Valid: true}
	}
	if f.Kind != "" {
		kind = pgtype.Text{String: f.Kind, Valid: true}
	}
	if f.Type != "" {
		evType = pgtype.Text{String: f.Type, Valid: true}
	}

	var fromTime, toTime pgtype.Timestamptz
	if !f.From.IsZero() {
		fromTime = pgtype.Timestamptz{Time: f.From, Valid: true}
	}
	if !f.To.IsZero() {
		toTime = pgtype.Timestamptz{Time: f.To, Valid: true}
	}

	start := time.Now()
	rows, err := gen.New(db.pool).ListK8sEvents(ctx, gen.ListK8sEventsParams{
		Name:     name,
		Kind:     kind,
		EvType:   evType,
		FromTime: fromTime,
		ToTime:   toTime,
		CurTime:  curTime,
		CurID:    curID,
		Lim:      int32(limit), //nolint:gosec // limit is clamped to [1,500] above
	})
	db.observe(queryListK8sEvents, start, queryResult(err))
	if err != nil {
		return K8sEventPage{}, fmt.Errorf("store: list k8s events: %w", err)
	}

	events := make([]K8sEvent, len(rows))
	for i := range rows {
		events[i] = k8sEventFromRow(&rows[i])
	}

	var nextCursor string
	if len(rows) == limit {
		last := events[len(events)-1]
		nextCursor = EncodeCursor(last.EventTime, last.ID)
	}

	return K8sEventPage{Events: events, NextCursor: nextCursor}, nil
}

// DeleteK8sEventsBefore deletes up to limit captured events older than before,
// oldest first, and reports how many were removed. Used by Pruner's sweep;
// exposed for the same testability reason as DeleteRunsBefore.
func (db *DB) DeleteK8sEventsBefore(ctx context.Context, before time.Time, limit int32) (int64, error) {
	n, err := gen.New(db.pool).DeleteK8sEventsBefore(ctx, gen.DeleteK8sEventsBeforeParams{
		EventTime: before,
		Limit:     limit,
	})
	if err != nil {
		return 0, fmt.Errorf("store: delete k8s events before: %w", err)
	}
	return n, nil
}
