package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/EsDmitrii/kconmon-ng/internal/console/store/gen"
)

// Every one is counted in BYTES, not runes, for annotationTextMaxLen's reason: the columns store
// bytes.
const (
	incidentTitleMaxLen = 255
	incidentNotesMaxLen = 16384
	incidentScopeMaxLen = 255

	// pinnedMaxEntries bounds the pinned array.
	pinnedMaxEntries = 64
	pinnedIDMaxLen   = 128
	pinnedNoteMaxLen = 512
)

// The set is closed here rather than by a CHECK constraint so widening it in a later milestone is
// code, not a migration.
const (
	IncidentStatusOpen     = "open"
	IncidentStatusResolved = "resolved"
)

var incidentStatuses = map[string]bool{
	IncidentStatusOpen:     true,
	IncidentStatusResolved: true,
}

// pinnedKinds is the closed vocabulary of PinnedRef.Kind; an unknown kind is REJECTED rather than
// stored.
var pinnedKinds = map[string]bool{
	"event":      true, // a topology_events row
	"audit":      true, // an audit_log row
	"annotation": true, // an annotations row
	"snapshot":   true, // an mtr_path_snapshots row
	"run":        true, // a check_runs row
	"k8s":        true, // a k8s_events row
}

// PinnedRef is one finding pinned to an incident; ID is a STRING for every kind, including the
// bigint-keyed ones (event, k8s).
type PinnedRef struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
	Note string `json:"note,omitempty"`
}

// Incident is one saved investigation: annotations-class.
type Incident struct {
	ID    string
	Title string
	// Scope is "" for a global incident; any other value names a node, a pair
	// or a target and is matched exactly. It is a filter key, NEVER a
	// Prometheus label value -- neither it nor Title is ever exported as one.
	Scope  string
	FromAt time.Time
	// ToAt is nil for an OPEN-ENDED range ("from then until further notice").
	// That is the opposite of Annotation.EndAt's nil, which means an instant
	// mark: the two look alike and mean different things.
	ToAt   *time.Time
	Status string
	Notes  string
	// Pinned is the stored JSONB ARRAY of PinnedRef; handed back raw for PathSnapshot.Hops' reason:
	// the API layer re-serializes.
	Pinned    json.RawMessage
	CreatedBy string
	CreatedAt time.Time
	// ResolvedAt is status' witness: non-nil exactly when Status is
	// "resolved". Retention reads THIS column, which is what makes an open
	// incident unprunable -- see prune.go.
	ResolvedAt *time.Time
}

// IncidentInput is CreateIncident's write payload.
type IncidentInput struct {
	Title  string
	Scope  string // "" = global
	FromAt time.Time
	ToAt   *time.Time // nil = open-ended range
	// Status is "" for the usual case, which Validate reads as "open". A
	// caller may name a status explicitly; the ResolvedAt invariant below then
	// applies exactly as it does on update.
	Status     string
	Notes      string
	Pinned     json.RawMessage // nil / empty / JSON null all store []
	CreatedBy  string
	ResolvedAt *time.Time
}

// IncidentFilter selects a page of incidents. All fields optional; Limit is
// clamped to [1,500] the same way EventFilter.Limit is.
type IncidentFilter struct {
	Status string // exact match; "" = every status
	// Scope is a POINTER, the same exception AnnotationFilter.Scope is and for
	// the same reason: "" is a real value here (the global scope), so nil
	// means "every scope" and a pointer to "" means "the global ones only".
	Scope *string
	// From and To bound the window an incident's OWN RANGE must OVERLAP, not the window it was created
	// in.
	From   time.Time // inclusive
	To     time.Time // exclusive
	Cursor string    // opaque keyset cursor from a previous page
	Limit  int
}

// IncidentPage is one page of ListIncidents results, same shape as
// AnnotationPage.
type IncidentPage struct {
	Incidents  []Incident
	NextCursor string // "" when the page is the last one
}

// The update surface is THREE NARROW UPDATES rather than one full replace: an incident evolves
// while several people look at it.
type IncidentStore interface {
	CreateIncident(ctx context.Context, in IncidentInput) (Incident, error)
	/* UpdateIncidentStatus moves an incident between open and resolved.
	   The bool is whether THIS call performed the transition: false means the incident was already in
	   that status (a racing writer got there first), and the returned Incident is the current row. It
	   is what keeps one resolution from being announced N times. */
	UpdateIncidentStatus(ctx context.Context, id, status string, resolvedAt *time.Time) (Incident, bool, error)
	UpdateIncidentNotes(ctx context.Context, id, notes string) (Incident, error)
	UpdateIncidentPinned(ctx context.Context, id string, pinned json.RawMessage) (Incident, error)
	// DeleteIncident returns ErrNotFound when id does not name an incident,
	// including when it is not a UUID at all.
	DeleteIncident(ctx context.Context, id string) error
}

var _ IncidentStore = (*DB)(nil)

// IncidentReader is the read seam: httpapi's incidents routes, the Overview
// "open incidents" card, and the Investigate page's ?incident= hydration.
type IncidentReader interface {
	GetIncident(ctx context.Context, id string) (Incident, error)
	ListIncidents(ctx context.Context, f IncidentFilter) (IncidentPage, error)
}

var _ IncidentReader = (*DB)(nil)

// Validate reports whether in is a well-formed incident.
func (in *IncidentInput) Validate() error {
	// See validateNoControlChars: a NUL here came back as 502 "incidents unavailable".
	for _, f := range [][2]string{{"title", in.Title}, {"scope", in.Scope}, {"notes", in.Notes}} {
		if err := validateNoControlChars(f[0], f[1]); err != nil {
			return fmt.Errorf("store: incident: %w", err)
		}
	}
	if in.Title == "" {
		return errors.New("store: incident: title must not be empty")
	}
	if len(in.Title) > incidentTitleMaxLen {
		return fmt.Errorf("store: incident: title is %d bytes, limit is %d", len(in.Title), incidentTitleMaxLen)
	}
	if len(in.Scope) > incidentScopeMaxLen {
		return fmt.Errorf("store: incident: scope is %d bytes, limit is %d", len(in.Scope), incidentScopeMaxLen)
	}
	if len(in.CreatedBy) > incidentScopeMaxLen {
		return fmt.Errorf("store: incident: created by is %d bytes, limit is %d",
			len(in.CreatedBy), incidentScopeMaxLen)
	}
	if in.FromAt.IsZero() {
		return errors.New("store: incident: from at must not be zero")
	}
	if in.ToAt != nil && in.ToAt.Before(in.FromAt) {
		return fmt.Errorf("store: incident: to at %s is before from at %s",
			in.ToAt.Format(time.RFC3339Nano), in.FromAt.Format(time.RFC3339Nano))
	}
	if len(in.Notes) > incidentNotesMaxLen {
		return fmt.Errorf("store: incident: notes are %d bytes, limit is %d", len(in.Notes), incidentNotesMaxLen)
	}
	if err := validateIncidentStatus(in.effectiveStatus(), in.ResolvedAt); err != nil {
		return err
	}
	return ValidatePinned(in.Pinned)
}

// effectiveStatus reads an empty Status as "open", the column's own DEFAULT.
func (in *IncidentInput) effectiveStatus() string {
	if in.Status == "" {
		return IncidentStatusOpen
	}
	return in.Status
}

// validateIncidentStatus applies the status/resolved_at invariant that both CreateIncident and
// UpdateIncidentStatus enforce; checked in Go rather than by a CHECK constraint so the status
// vocabulary can widen without a migration.
func validateIncidentStatus(status string, resolvedAt *time.Time) error {
	if !incidentStatuses[status] {
		return fmt.Errorf("store: incident: status %q must be one of open, resolved", status)
	}
	switch {
	case status == IncidentStatusResolved && resolvedAt == nil:
		return errors.New("store: incident: a resolved incident must carry a resolved at")
	case status == IncidentStatusOpen && resolvedAt != nil:
		return errors.New("store: incident: an open incident must not carry a resolved at")
	case resolvedAt != nil && resolvedAt.IsZero():
		return errors.New("store: incident: resolved at must not be zero")
	}
	return nil
}

// ValidatePinned reports whether raw is a well-formed pinned array.
func ValidatePinned(raw json.RawMessage) error {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, jsonNull) {
		return nil
	}
	var refs []PinnedRef
	if err := json.Unmarshal(trimmed, &refs); err != nil {
		return fmt.Errorf("store: incident: pinned must be a JSON array of {kind,id,note}: %w", err)
	}
	if len(refs) > pinnedMaxEntries {
		return fmt.Errorf("store: incident: %d pinned entries, limit is %d", len(refs), pinnedMaxEntries)
	}
	for i := range refs {
		if !pinnedKinds[refs[i].Kind] {
			return fmt.Errorf("store: incident: pinned[%d]: kind %q must be one of "+
				"event, audit, annotation, snapshot, run, k8s", i, refs[i].Kind)
		}
		if refs[i].ID == "" {
			return fmt.Errorf("store: incident: pinned[%d]: id must not be empty", i)
		}
		if len(refs[i].ID) > pinnedIDMaxLen {
			return fmt.Errorf("store: incident: pinned[%d]: id is %d bytes, limit is %d",
				i, len(refs[i].ID), pinnedIDMaxLen)
		}
		if len(refs[i].Note) > pinnedNoteMaxLen {
			return fmt.Errorf("store: incident: pinned[%d]: note is %d bytes, limit is %d",
				i, len(refs[i].Note), pinnedNoteMaxLen)
		}
	}
	return nil
}

// DecodePinned parses an Incident.Pinned payload back into typed refs; the store itself never needs
// it -- it hands the JSONB straight through.
func DecodePinned(raw json.RawMessage) ([]PinnedRef, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, jsonNull) {
		return nil, nil
	}
	var refs []PinnedRef
	if err := json.Unmarshal(trimmed, &refs); err != nil {
		return nil, fmt.Errorf("store: decode pinned: %w", err)
	}
	return refs, nil
}

// orEmptyPinnedArray is orEmptyJSON for an ARRAY-shaped column; it cannot be orEmptyJSON itself:
// that one substitutes {}, the labels/params default.
func orEmptyPinnedArray(raw json.RawMessage) json.RawMessage {
	if len(bytes.TrimSpace(raw)) == 0 || bytes.Equal(bytes.TrimSpace(raw), jsonNull) {
		return json.RawMessage(`[]`)
	}
	return raw
}

func incidentFromRow(i *gen.Incident) Incident {
	return Incident{
		ID:         formatUUID(i.ID),
		Title:      i.Title,
		Scope:      i.Scope,
		FromAt:     i.FromAt,
		ToAt:       nullTime(i.ToAt),
		Status:     i.Status,
		Notes:      i.Notes,
		Pinned:     i.Pinned,
		CreatedBy:  i.CreatedBy,
		CreatedAt:  i.CreatedAt,
		ResolvedAt: nullTime(i.ResolvedAt),
	}
}

func (db *DB) CreateIncident(ctx context.Context, in IncidentInput) (Incident, error) { //nolint:gocritic // hugeParam: IncidentInput mirrors the other write-payload structs in this package
	if err := in.Validate(); err != nil {
		return Incident{}, err
	}
	iid, err := parseUUID(uuid.NewString())
	if err != nil {
		return Incident{}, fmt.Errorf("store: create incident: %w", err)
	}

	start := time.Now()
	row, err := gen.New(db.pool).CreateIncident(ctx, gen.CreateIncidentParams{
		ID:         iid,
		Title:      in.Title,
		Scope:      in.Scope,
		FromAt:     in.FromAt,
		ToAt:       timestamptzFromPtr(in.ToAt),
		Status:     in.effectiveStatus(),
		Notes:      in.Notes,
		Pinned:     orEmptyPinnedArray(in.Pinned),
		CreatedBy:  in.CreatedBy,
		ResolvedAt: timestamptzFromPtr(in.ResolvedAt),
	})
	db.observe(queryCreateIncident, start, queryResult(err))
	if err != nil {
		return Incident{}, fmt.Errorf("store: create incident: %w", err)
	}
	return incidentFromRow(&row), nil
}

// GetIncident applies GetRun's UUID pre-check: a malformed id is reported as
// ErrNotFound, not as a parse failure, so the edge answers 404 rather than
// "incident store unavailable".
func (db *DB) GetIncident(ctx context.Context, id string) (Incident, error) {
	iid, err := parseUUID(id)
	if err != nil {
		return Incident{}, fmt.Errorf("store: get incident: %w: %w", ErrNotFound, err)
	}
	start := time.Now()
	row, err := gen.New(db.pool).GetIncident(ctx, iid)
	db.observe(queryGetIncident, start, queryResult(wrapNoRows(err)))
	if err != nil {
		return Incident{}, fmt.Errorf("store: get incident: %w", wrapNoRows(err))
	}
	return incidentFromRow(&row), nil
}

func (db *DB) ListIncidents(ctx context.Context, f IncidentFilter) (IncidentPage, error) { //nolint:gocritic // hugeParam: IncidentFilter mirrors EventFilter's value semantics (events.go)
	limit := clampLimit(f.Limit)

	curTime, curID, err := decodeKeyset(f.Cursor)
	if err != nil {
		return IncidentPage{}, fmt.Errorf("store: list incidents: %w", err)
	}

	var status, scope pgtype.Text
	if f.Status != "" {
		status = pgtype.Text{String: f.Status, Valid: true}
	}
	if f.Scope != nil {
		scope = pgtype.Text{String: *f.Scope, Valid: true}
	}

	var fromTime, toTime pgtype.Timestamptz
	if !f.From.IsZero() {
		fromTime = pgtype.Timestamptz{Time: f.From, Valid: true}
	}
	if !f.To.IsZero() {
		toTime = pgtype.Timestamptz{Time: f.To, Valid: true}
	}

	start := time.Now()
	rows, err := gen.New(db.pool).ListIncidents(ctx, gen.ListIncidentsParams{
		Status:   status,
		Scope:    scope,
		FromTime: fromTime,
		ToTime:   toTime,
		CurTime:  curTime,
		CurID:    curID,
		Lim:      int32(limit), //nolint:gosec // limit is clamped to [1,500] above
	})
	db.observe(queryListIncidents, start, queryResult(err))
	if err != nil {
		return IncidentPage{}, fmt.Errorf("store: list incidents: %w", err)
	}

	incidents := make([]Incident, len(rows))
	for i := range rows {
		incidents[i] = incidentFromRow(&rows[i])
	}

	var nextCursor string
	if len(rows) == limit {
		last := incidents[len(incidents)-1]
		nextCursor = EncodeUUIDCursor(last.CreatedAt, last.ID)
	}

	return IncidentPage{Incidents: incidents, NextCursor: nextCursor}, nil
}

// UpdateIncidentStatus resolves or reopens an incident. See IncidentStore for
// why resolvedAt is not a separate call.
func (db *DB) UpdateIncidentStatus(ctx context.Context, id, status string, resolvedAt *time.Time) (Incident, bool, error) {
	if err := validateIncidentStatus(status, resolvedAt); err != nil {
		return Incident{}, false, err
	}
	iid, err := parseUUID(id)
	if err != nil {
		return Incident{}, false, fmt.Errorf("store: update incident status: %w: %w", ErrNotFound, err)
	}
	start := time.Now()
	row, err := gen.New(db.pool).UpdateIncidentStatus(ctx, gen.UpdateIncidentStatusParams{
		ID:         iid,
		Status:     status,
		ResolvedAt: timestamptzFromPtr(resolvedAt),
	})
	db.observe(queryUpdateIncidentStatus, start, queryResult(wrapNoRows(err)))
	if err == nil {
		return incidentFromRow(&row), true, nil
	}
	/* No rows is AMBIGUOUS on its own: either the id does not exist, or the incident is already in
	   this status. The read tells them apart, and an already-transitioned incident is a success with
	   nothing to announce. */
	if errors.Is(wrapNoRows(err), ErrNotFound) {
		current, getErr := db.GetIncident(ctx, id)
		if getErr == nil {
			return current, false, nil
		}
		return Incident{}, false, fmt.Errorf("store: update incident status: %w", getErr)
	}
	return Incident{}, false, fmt.Errorf("store: update incident status: %w", wrapNoRows(err))
}

// UpdateIncidentNotes replaces the incident's Markdown notes.
func (db *DB) UpdateIncidentNotes(ctx context.Context, id, notes string) (Incident, error) {
	if len(notes) > incidentNotesMaxLen {
		return Incident{}, fmt.Errorf("store: incident: notes are %d bytes, limit is %d",
			len(notes), incidentNotesMaxLen)
	}
	iid, err := parseUUID(id)
	if err != nil {
		return Incident{}, fmt.Errorf("store: update incident notes: %w: %w", ErrNotFound, err)
	}
	start := time.Now()
	row, err := gen.New(db.pool).UpdateIncidentNotes(ctx, gen.UpdateIncidentNotesParams{ID: iid, Notes: notes})
	db.observe(queryUpdateIncidentNotes, start, queryResult(wrapNoRows(err)))
	if err != nil {
		return Incident{}, fmt.Errorf("store: update incident notes: %w", wrapNoRows(err))
	}
	return incidentFromRow(&row), nil
}

// UpdateIncidentPinned replaces the pinned-findings array wholesale; a pin and an unpin are both
// "the list is now this", which is what the UI actually knows.
func (db *DB) UpdateIncidentPinned(ctx context.Context, id string, pinned json.RawMessage) (Incident, error) {
	if err := ValidatePinned(pinned); err != nil {
		return Incident{}, err
	}
	iid, err := parseUUID(id)
	if err != nil {
		return Incident{}, fmt.Errorf("store: update incident pinned: %w: %w", ErrNotFound, err)
	}
	start := time.Now()
	row, err := gen.New(db.pool).UpdateIncidentPinned(ctx, gen.UpdateIncidentPinnedParams{
		ID:     iid,
		Pinned: orEmptyPinnedArray(pinned),
	})
	db.observe(queryUpdateIncidentPinned, start, queryResult(wrapNoRows(err)))
	if err != nil {
		return Incident{}, fmt.Errorf("store: update incident pinned: %w", wrapNoRows(err))
	}
	return incidentFromRow(&row), nil
}

// DeleteIncident removes one incident. Same pre-check and same miss answer as
// DeleteAnnotation: deleting an incident that is not there is ErrNotFound, not
// success -- the caller asked about a specific one.
func (db *DB) DeleteIncident(ctx context.Context, id string) error {
	iid, err := parseUUID(id)
	if err != nil {
		return fmt.Errorf("store: delete incident: %w: %w", ErrNotFound, err)
	}
	start := time.Now()
	rows, err := gen.New(db.pool).DeleteIncident(ctx, iid)
	db.observe(queryDeleteIncident, start, queryResult(err))
	if err != nil {
		return fmt.Errorf("store: delete incident: %w", err)
	}
	if rows == 0 {
		return fmt.Errorf("store: delete incident: %w", ErrNotFound)
	}
	return nil
}

// DeleteIncidentsBefore deletes up to limit incidents RESOLVED before before; an open incident has
// a NULL resolved_at and can therefore never be selected.
func (db *DB) DeleteIncidentsBefore(ctx context.Context, before time.Time, limit int32) (int64, error) {
	n, err := gen.New(db.pool).DeleteIncidentsBefore(ctx, gen.DeleteIncidentsBeforeParams{
		ResolvedAt: pgtype.Timestamptz{Time: before, Valid: true},
		Limit:      limit,
	})
	if err != nil {
		return 0, fmt.Errorf("store: delete incidents before: %w", err)
	}
	return n, nil
}
