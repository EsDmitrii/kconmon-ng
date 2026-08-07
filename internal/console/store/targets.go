package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/EsDmitrii/kconmon-ng/internal/console/store/gen"
)

// ErrInUse is returned when a delete is refused because another row still
// references the one being deleted -- today only DeleteTarget, blocked by
// check_definitions.destination_target_id's ON DELETE RESTRICT (migration
// 00004). It is deliberately NOT ErrWrongState: nothing about the target is
// wrong, and nothing about it can be fixed by retrying. The caller's remedy
// is to delete (or re-point) the definitions first, which
// ListDefinitions(DefinitionFilter{TargetID: id}) enumerates.
var ErrInUse = errors.New("store: in use")

// foreignKeyViolationCode is PostgreSQL's SQLSTATE for a
// foreign_key_violation, the counterpart to uniqueViolationCode (auth.go).
const foreignKeyViolationCode = "23503"

// wrapForeignKeyViolation turns a foreign-key PgError into sentinel, leaving
// every other error (including a nil one) unchanged. The sentinel is a
// parameter because the same SQLSTATE means opposite things in the two
// directions: a DELETE refused by ON DELETE RESTRICT means the row is still
// referenced (ErrInUse), while an INSERT or UPDATE naming a
// destination_target_id with no targets row means the reference itself is
// missing (ErrNotFound).
func wrapForeignKeyViolation(err, sentinel error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == foreignKeyViolationCode {
		return sentinel
	}
	return err
}

// nameMaxLen mirrors the CHECK (length(name) BETWEEN 1 AND 63) both targets
// and check_definitions carry. Validation rejects an over-long name here so a
// caller gets a plain "name too long" instead of a raw constraint violation
// from the driver.
const nameMaxLen = 63

// nameRE bounds what may become a Prometheus label value: ASCII alphanumerics
// plus '-', '_' and '.', never leading or trailing. Postgres only bounds the
// length; this bounds the charset, because targets.name is the one field that
// leaves this system as a label value (migration 00004's comment) and a name
// carrying quotes, whitespace or newlines would land in an exposition-format
// line.
var nameRE = regexp.MustCompile(`^[a-zA-Z0-9]([a-zA-Z0-9_.-]*[a-zA-Z0-9])?$`)

// ---------------------------------------------------------------------------
// Targets
// ---------------------------------------------------------------------------

// Target is one external probe destination (TARGETS.md §7.2). Name is the
// only field that ever becomes a Prometheus label value.
type Target struct {
	ID        string
	Name      string
	Kind      string // host | url
	Address   string
	Labels    json.RawMessage
	CreatedAt time.Time
	UpdatedAt time.Time
}

// TargetInput is CreateTarget/UpdateTarget's write payload. Both are full
// replaces: every field is written on every call, so a zero-value field means
// "empty", never "leave as-is".
type TargetInput struct {
	Name    string
	Kind    string // host | url
	Address string
	// Labels is a JSON object. nil, an empty slice and the literal JSON null
	// are all written as {} (see orEmptyJSON); anything else must parse, or
	// Validate rejects it. These are the target's own descriptive labels
	// (DATA.md), NOT Prometheus label values -- only Name ever becomes one.
	Labels json.RawMessage
}

// TargetFilter selects a page of targets. All fields optional; Limit is
// clamped to [1,500] the same way EventFilter.Limit is.
type TargetFilter struct {
	Kind   string // exact match; empty = all
	Cursor string // opaque keyset cursor from a previous page
	Limit  int
}

// TargetPage is one page of ListTargets results, same shape as EventPage.
type TargetPage struct {
	Targets    []Target
	NextCursor string // "" when the page is the last one
}

// TargetStore is the write seam: httpapi mutates targets, nothing else does.
type TargetStore interface {
	CreateTarget(ctx context.Context, in TargetInput) (Target, error)
	UpdateTarget(ctx context.Context, id string, in TargetInput) (Target, error)
	DeleteTarget(ctx context.Context, id string) error
}

var _ TargetStore = (*DB)(nil)

// TargetReader is the read seam: httpapi lists, the assignment planner and
// the runner resolve one by id. ErrNotFound for an unknown id;
// ErrAlreadyExists for a duplicate name; ErrInUse when a DELETE is refused
// by check_definitions' ON DELETE RESTRICT.
type TargetReader interface {
	GetTarget(ctx context.Context, id string) (Target, error)
	ListTargets(ctx context.Context, f TargetFilter) (TargetPage, error)
}

var _ TargetReader = (*DB)(nil)

// targetKinds is the closed set migration 00004's "-- host | url" comment
// names. Enforced here rather than by a CHECK constraint so widening it in a
// later milestone is code, not a migration (the same reasoning Decision 9
// applies to check_schedules.kind).
var targetKinds = map[string]bool{"host": true, "url": true}

// Validate reports whether in is a well-formed target. It runs before the
// INSERT/UPDATE so a caller gets a precise, actionable error instead of a
// raw constraint violation, and so the charset rule -- which Postgres does
// not enforce at all -- is applied at the only layer that can. Malformed
// Labels are rejected here for the same reason: the driver's own complaint
// about a jsonb literal does not say which field carried it.
func (in *TargetInput) Validate() error {
	if err := validateName(in.Name); err != nil {
		return fmt.Errorf("store: target: %w", err)
	}
	if !targetKinds[in.Kind] {
		return fmt.Errorf("store: target: kind %q must be one of host, url", in.Kind)
	}
	if in.Address == "" {
		return errors.New("store: target: address must not be empty")
	}
	if err := validateJSON("labels", in.Labels); err != nil {
		return fmt.Errorf("store: target: %w", err)
	}
	return nil
}

// validateName applies the shared name rule targets and check_definitions
// both carry.
func validateName(name string) error {
	if name == "" {
		return errors.New("name must not be empty")
	}
	if len(name) > nameMaxLen {
		return fmt.Errorf("name is %d bytes, limit is %d", len(name), nameMaxLen)
	}
	if !nameRE.MatchString(name) {
		return fmt.Errorf("name %q must be alphanumerics, '-', '_' or '.', starting and ending alphanumeric", name)
	}
	return nil
}

func targetFromRow(t *gen.Target) Target {
	return Target{
		ID:        formatUUID(t.ID),
		Name:      t.Name,
		Kind:      t.Kind,
		Address:   t.Address,
		Labels:    t.Labels,
		CreatedAt: t.CreatedAt,
		UpdatedAt: t.UpdatedAt,
	}
}

func (db *DB) CreateTarget(ctx context.Context, in TargetInput) (Target, error) { //nolint:gocritic // hugeParam: TargetInput mirrors the other write-payload structs in this package
	if err := in.Validate(); err != nil {
		return Target{}, err
	}
	// The id is minted here rather than by a column DEFAULT so the whole
	// package keeps one id story: every UUID primary key it writes
	// (check_runs, targets, check_definitions, check_schedules) is
	// caller-side, and a create is therefore always retry-identifiable.
	tid, err := parseUUID(uuid.NewString())
	if err != nil {
		return Target{}, fmt.Errorf("store: create target: %w", err)
	}
	t, err := gen.New(db.pool).CreateTarget(ctx, gen.CreateTargetParams{
		ID:      tid,
		Name:    in.Name,
		Kind:    in.Kind,
		Address: in.Address,
		Labels:  orEmptyJSON(in.Labels),
	})
	if err != nil {
		return Target{}, fmt.Errorf("store: create target: %w", wrapUniqueViolation(err))
	}
	return targetFromRow(&t), nil
}

func (db *DB) UpdateTarget(ctx context.Context, id string, in TargetInput) (Target, error) { //nolint:gocritic // hugeParam: TargetInput mirrors the other write-payload structs in this package
	if err := in.Validate(); err != nil {
		return Target{}, err
	}
	tid, err := parseUUID(id)
	if err != nil {
		return Target{}, fmt.Errorf("store: update target: %w", err)
	}
	t, err := gen.New(db.pool).UpdateTarget(ctx, gen.UpdateTargetParams{
		ID:      tid,
		Name:    in.Name,
		Kind:    in.Kind,
		Address: in.Address,
		Labels:  orEmptyJSON(in.Labels),
	})
	if err != nil {
		return Target{}, fmt.Errorf("store: update target: %w", wrapUniqueViolation(wrapNoRows(err)))
	}
	return targetFromRow(&t), nil
}

func (db *DB) DeleteTarget(ctx context.Context, id string) error {
	tid, err := parseUUID(id)
	if err != nil {
		return fmt.Errorf("store: delete target: %w", err)
	}
	rows, err := gen.New(db.pool).DeleteTarget(ctx, tid)
	if err != nil {
		return fmt.Errorf("store: delete target: %w", wrapForeignKeyViolation(err, ErrInUse))
	}
	if rows == 0 {
		return fmt.Errorf("store: delete target: %w", ErrNotFound)
	}
	return nil
}

func (db *DB) GetTarget(ctx context.Context, id string) (Target, error) {
	tid, err := parseUUID(id)
	if err != nil {
		return Target{}, fmt.Errorf("store: get target: %w", err)
	}
	t, err := gen.New(db.pool).GetTarget(ctx, tid)
	if err != nil {
		return Target{}, fmt.Errorf("store: get target: %w", wrapNoRows(err))
	}
	return targetFromRow(&t), nil
}

func (db *DB) ListTargets(ctx context.Context, f TargetFilter) (TargetPage, error) { //nolint:gocritic // hugeParam: TargetFilter mirrors EventFilter's value semantics (events.go)
	limit := clampLimit(f.Limit)

	curTime, curID, err := decodeKeyset(f.Cursor)
	if err != nil {
		return TargetPage{}, fmt.Errorf("store: list targets: %w", err)
	}

	var kind pgtype.Text
	if f.Kind != "" {
		kind = pgtype.Text{String: f.Kind, Valid: true}
	}

	rows, err := gen.New(db.pool).ListTargets(ctx, gen.ListTargetsParams{
		Kind:    kind,
		CurTime: curTime,
		CurID:   curID,
		Lim:     int32(limit), //nolint:gosec // limit is clamped to [1,500] above
	})
	if err != nil {
		return TargetPage{}, fmt.Errorf("store: list targets: %w", err)
	}

	targets := make([]Target, len(rows))
	for i := range rows {
		targets[i] = targetFromRow(&rows[i])
	}

	var nextCursor string
	if len(rows) == limit {
		last := targets[len(targets)-1]
		nextCursor = EncodeUUIDCursor(last.CreatedAt, last.ID)
	}

	return TargetPage{Targets: targets, NextCursor: nextCursor}, nil
}

// ---------------------------------------------------------------------------
// Check definitions
// ---------------------------------------------------------------------------

// Definition is one saved check spec: what to probe, from where, how
// (DATA.md §5.2).
type Definition struct {
	ID              string
	Name            string
	SourceSelection string // all | per-zone | one-per-zone
	DestinationKind string // node | target | adhoc
	// DestinationTargetID is "" for every DestinationKind but "target".
	DestinationTargetID string
	DestinationAddress  string
	CheckType           string // tcp | udp | icmp | dns | http | mtr
	Plane               string
	Params              json.RawMessage
	Enabled             bool
	CreatedAt           time.Time
	UpdatedAt           time.Time
}

// DefinitionInput is CreateDefinition/UpdateDefinition's write payload, a
// full replace with the same contract as TargetInput.
type DefinitionInput struct {
	Name                string
	SourceSelection     string
	DestinationKind     string
	DestinationTargetID string
	DestinationAddress  string
	CheckType           string
	Plane               string
	// Params is a JSON object, with the same nil / empty / null -> {}
	// normalisation and the same validity rule as TargetInput.Labels.
	Params  json.RawMessage
	Enabled bool
}

// DefinitionFilter selects a page of definitions. All fields optional; Limit
// is clamped to [1,500].
type DefinitionFilter struct {
	TargetID string // exact destination_target_id match; empty = all
	Enabled  *bool  // nil = both
	Cursor   string
	Limit    int
}

// DefinitionPage is one page of ListDefinitions results.
type DefinitionPage struct {
	Definitions []Definition
	NextCursor  string
}

// DefinitionStore is the write seam: httpapi mutates definitions, nothing
// else does.
type DefinitionStore interface {
	CreateDefinition(ctx context.Context, in DefinitionInput) (Definition, error)
	UpdateDefinition(ctx context.Context, id string, in DefinitionInput) (Definition, error)
	// DeleteDefinition cascades to the definition's schedules
	// (check_schedules.definition_id ON DELETE CASCADE).
	DeleteDefinition(ctx context.Context, id string) error
}

var _ DefinitionStore = (*DB)(nil)

// DefinitionReader is the read seam: httpapi lists, the scheduler resolves
// one by id when a schedule fires. ErrNotFound for an unknown id -- and for a
// DestinationTargetID naming no target on create/update, since the missing
// row is the target's, not the definition's; ErrAlreadyExists for a duplicate
// name.
type DefinitionReader interface {
	GetDefinition(ctx context.Context, id string) (Definition, error)
	ListDefinitions(ctx context.Context, f DefinitionFilter) (DefinitionPage, error)
}

var _ DefinitionReader = (*DB)(nil)

// The closed sets migration 00004's column comments name. Same reasoning as
// targetKinds: enforced in code, not by a CHECK constraint.
var (
	sourceSelections = map[string]bool{"all": true, "per-zone": true, "one-per-zone": true}
	destinationKinds = map[string]bool{"node": true, "target": true, "adhoc": true}
	checkTypes       = map[string]bool{"tcp": true, "udp": true, "icmp": true, "dns": true, "http": true, "mtr": true}
)

// Validate reports whether in is a well-formed definition. Beyond the
// enumerations it enforces the one cross-field rule the schema states but
// cannot express: destination_target_id belongs to destination_kind='target'
// and to nothing else, and 'adhoc' is the only kind that carries a literal
// destination_address. Malformed Params are rejected here, same as
// TargetInput.Labels.
func (in *DefinitionInput) Validate() error {
	if err := validateName(in.Name); err != nil {
		return fmt.Errorf("store: definition: %w", err)
	}
	if !sourceSelections[in.SourceSelection] {
		return fmt.Errorf("store: definition: source selection %q must be one of all, per-zone, one-per-zone", in.SourceSelection)
	}
	if !destinationKinds[in.DestinationKind] {
		return fmt.Errorf("store: definition: destination kind %q must be one of node, target, adhoc", in.DestinationKind)
	}
	if !checkTypes[in.CheckType] {
		return fmt.Errorf("store: definition: check type %q must be one of tcp, udp, icmp, dns, http, mtr", in.CheckType)
	}
	if in.Plane == "" {
		return errors.New("store: definition: plane must not be empty")
	}
	if in.DestinationKind == "target" && in.DestinationTargetID == "" {
		return errors.New("store: definition: destination kind target requires a destination target id")
	}
	if in.DestinationKind != "target" && in.DestinationTargetID != "" {
		return fmt.Errorf("store: definition: destination target id is only valid for destination kind target, not %q", in.DestinationKind)
	}
	if in.DestinationKind == "adhoc" && in.DestinationAddress == "" {
		return errors.New("store: definition: destination kind adhoc requires a destination address")
	}
	if err := validateJSON("params", in.Params); err != nil {
		return fmt.Errorf("store: definition: %w", err)
	}
	return nil
}

func definitionFromRow(d *gen.CheckDefinition) Definition {
	return Definition{
		ID:                  formatUUID(d.ID),
		Name:                d.Name,
		SourceSelection:     d.SourceSelection,
		DestinationKind:     d.DestinationKind,
		DestinationTargetID: optionalUUIDString(d.DestinationTargetID),
		DestinationAddress:  d.DestinationAddress,
		CheckType:           d.CheckType,
		Plane:               d.Plane,
		Params:              d.Params,
		Enabled:             d.Enabled,
		CreatedAt:           d.CreatedAt,
		UpdatedAt:           d.UpdatedAt,
	}
}

func (db *DB) CreateDefinition(ctx context.Context, in DefinitionInput) (Definition, error) { //nolint:gocritic // hugeParam: DefinitionInput mirrors the other write-payload structs in this package
	if err := in.Validate(); err != nil {
		return Definition{}, err
	}
	did, err := parseUUID(uuid.NewString())
	if err != nil {
		return Definition{}, fmt.Errorf("store: create definition: %w", err)
	}
	targetID, err := optionalUUID(in.DestinationTargetID)
	if err != nil {
		return Definition{}, fmt.Errorf("store: create definition: %w", err)
	}
	d, err := gen.New(db.pool).CreateDefinition(ctx, gen.CreateDefinitionParams{
		ID:                  did,
		Name:                in.Name,
		SourceSelection:     in.SourceSelection,
		DestinationKind:     in.DestinationKind,
		DestinationTargetID: targetID,
		DestinationAddress:  in.DestinationAddress,
		CheckType:           in.CheckType,
		Plane:               in.Plane,
		Params:              orEmptyJSON(in.Params),
		Enabled:             in.Enabled,
	})
	if err != nil {
		return Definition{}, fmt.Errorf("store: create definition: %w",
			wrapUniqueViolation(wrapForeignKeyViolation(err, ErrNotFound)))
	}
	return definitionFromRow(&d), nil
}

func (db *DB) UpdateDefinition(ctx context.Context, id string, in DefinitionInput) (Definition, error) { //nolint:gocritic // hugeParam: DefinitionInput mirrors the other write-payload structs in this package
	if err := in.Validate(); err != nil {
		return Definition{}, err
	}
	did, err := parseUUID(id)
	if err != nil {
		return Definition{}, fmt.Errorf("store: update definition: %w", err)
	}
	targetID, err := optionalUUID(in.DestinationTargetID)
	if err != nil {
		return Definition{}, fmt.Errorf("store: update definition: %w", err)
	}
	d, err := gen.New(db.pool).UpdateDefinition(ctx, gen.UpdateDefinitionParams{
		ID:                  did,
		Name:                in.Name,
		SourceSelection:     in.SourceSelection,
		DestinationKind:     in.DestinationKind,
		DestinationTargetID: targetID,
		DestinationAddress:  in.DestinationAddress,
		CheckType:           in.CheckType,
		Plane:               in.Plane,
		Params:              orEmptyJSON(in.Params),
		Enabled:             in.Enabled,
	})
	if err != nil {
		return Definition{}, fmt.Errorf("store: update definition: %w",
			wrapUniqueViolation(wrapForeignKeyViolation(wrapNoRows(err), ErrNotFound)))
	}
	return definitionFromRow(&d), nil
}

func (db *DB) DeleteDefinition(ctx context.Context, id string) error {
	did, err := parseUUID(id)
	if err != nil {
		return fmt.Errorf("store: delete definition: %w", err)
	}
	rows, err := gen.New(db.pool).DeleteDefinition(ctx, did)
	if err != nil {
		return fmt.Errorf("store: delete definition: %w", err)
	}
	if rows == 0 {
		return fmt.Errorf("store: delete definition: %w", ErrNotFound)
	}
	return nil
}

func (db *DB) GetDefinition(ctx context.Context, id string) (Definition, error) {
	did, err := parseUUID(id)
	if err != nil {
		return Definition{}, fmt.Errorf("store: get definition: %w", err)
	}
	d, err := gen.New(db.pool).GetDefinition(ctx, did)
	if err != nil {
		return Definition{}, fmt.Errorf("store: get definition: %w", wrapNoRows(err))
	}
	return definitionFromRow(&d), nil
}

func (db *DB) ListDefinitions(ctx context.Context, f DefinitionFilter) (DefinitionPage, error) { //nolint:gocritic // hugeParam: DefinitionFilter mirrors EventFilter's value semantics (events.go)
	limit := clampLimit(f.Limit)

	curTime, curID, err := decodeKeyset(f.Cursor)
	if err != nil {
		return DefinitionPage{}, fmt.Errorf("store: list definitions: %w", err)
	}

	targetID, err := optionalUUID(f.TargetID)
	if err != nil {
		return DefinitionPage{}, fmt.Errorf("store: list definitions: %w", err)
	}

	var enabled pgtype.Bool
	if f.Enabled != nil {
		enabled = pgtype.Bool{Bool: *f.Enabled, Valid: true}
	}

	rows, err := gen.New(db.pool).ListDefinitions(ctx, gen.ListDefinitionsParams{
		TargetID: targetID,
		Enabled:  enabled,
		CurTime:  curTime,
		CurID:    curID,
		Lim:      int32(limit), //nolint:gosec // limit is clamped to [1,500] above
	})
	if err != nil {
		return DefinitionPage{}, fmt.Errorf("store: list definitions: %w", err)
	}

	defs := make([]Definition, len(rows))
	for i := range rows {
		defs[i] = definitionFromRow(&rows[i])
	}

	var nextCursor string
	if len(rows) == limit {
		last := defs[len(defs)-1]
		nextCursor = EncodeUUIDCursor(last.CreatedAt, last.ID)
	}

	return DefinitionPage{Definitions: defs, NextCursor: nextCursor}, nil
}

// ---------------------------------------------------------------------------
// Check schedules
// ---------------------------------------------------------------------------

// Schedule binds a definition to a cadence (DATA.md §5.2). IntervalNs is
// nanoseconds, the repo-wide duration convention.
type Schedule struct {
	ID           string
	DefinitionID string
	Kind         string // once | interval | continuous
	IntervalNs   int64
	RunAt        *time.Time // kind="once" only
	Enabled      bool
	LastFiredAt  *time.Time
	NextFireAt   *time.Time
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// ScheduleInput is CreateSchedule/UpdateSchedule's write payload. DefinitionID
// is read only by CreateSchedule -- UpdateSchedule never moves a schedule to
// another definition (see UpdateSchedule's query comment).
type ScheduleInput struct {
	DefinitionID string
	Kind         string
	IntervalNs   int64
	RunAt        *time.Time
	Enabled      bool
	// NextFireAt is when ListDueSchedules should next hand this schedule out.
	// nil keeps it out of the due index entirely -- the state a kind="once"
	// schedule ends in, and the state every schedule starts in until its
	// owner computes a first fire time.
	NextFireAt *time.Time
}

// ScheduleFilter selects a page of schedules. All fields optional; Limit is
// clamped to [1,500].
type ScheduleFilter struct {
	DefinitionID string // exact match; empty = all
	Cursor       string
	Limit        int
}

// SchedulePage is one page of ListSchedules results.
type SchedulePage struct {
	Schedules  []Schedule
	NextCursor string
}

// ScheduleStore is the write seam: httpapi mutates schedules, and the
// scheduler stamps each fire through MarkScheduleFired.
type ScheduleStore interface {
	CreateSchedule(ctx context.Context, in ScheduleInput) (Schedule, error)
	UpdateSchedule(ctx context.Context, id string, in ScheduleInput) (Schedule, error)
	DeleteSchedule(ctx context.Context, id string) error
	// MarkScheduleFired records a fire and the next one in a single UPDATE.
	// nextFireAt == nil retires the schedule from the due index without
	// disabling it. Returns ErrNotFound when id does not name a schedule.
	MarkScheduleFired(ctx context.Context, id string, firedAt time.Time, nextFireAt *time.Time) error
}

var _ ScheduleStore = (*DB)(nil)

// ScheduleReader is the read seam: httpapi lists, the scheduler polls
// ListDueSchedules. ErrNotFound for an unknown id -- and for a DefinitionID
// naming no definition on create, since the missing row is the definition's.
type ScheduleReader interface {
	GetSchedule(ctx context.Context, id string) (Schedule, error)
	ListSchedules(ctx context.Context, f ScheduleFilter) (SchedulePage, error)
	// ListDueSchedules returns up to limit enabled schedules whose
	// next_fire_at has passed, soonest first. limit follows the same
	// clampLimit rule as every other listing here: 0 means "unset" and
	// yields the default of 100, anything else is clamped into [1,500].
	ListDueSchedules(ctx context.Context, due time.Time, limit int) ([]Schedule, error)
}

var _ ScheduleReader = (*DB)(nil)

// scheduleKinds is the closed set migration 00004's "-- once | interval |
// continuous (cron: later milestone)" comment names. Adding 'cron' is a
// one-line change here and no migration at all -- which is the entire point
// of the column being plain TEXT (Decision 9).
var scheduleKinds = map[string]bool{"once": true, "interval": true, "continuous": true}

// Validate reports whether in is a well-formed schedule. It enforces the
// cross-field rules the schema states in a comment but cannot express: run_at
// belongs to kind='once' and to nothing else, an interval cadence needs a
// positive interval, and neither 'once' nor 'continuous' may carry one --
// 'once' because it fires exactly at run_at, 'continuous' because it has no
// cadence at all (the check runs for as long as it is enabled, so there is no
// gap between fires for an interval to describe). A non-zero IntervalNs on
// either is a caller that meant kind='interval', and silently storing a
// number nothing will ever read is the worse of the two outcomes.
func (in *ScheduleInput) Validate() error {
	if _, err := uuid.Parse(in.DefinitionID); err != nil {
		return fmt.Errorf("store: schedule: definition id: %w", err)
	}
	if !scheduleKinds[in.Kind] {
		return fmt.Errorf("store: schedule: kind %q must be one of once, interval, continuous", in.Kind)
	}
	switch in.Kind {
	case "once":
		if in.RunAt == nil {
			return errors.New("store: schedule: kind once requires a run at time")
		}
		if in.IntervalNs != 0 {
			return errors.New("store: schedule: kind once must not carry an interval")
		}
	case "interval":
		if in.IntervalNs <= 0 {
			return errors.New("store: schedule: kind interval requires a positive interval")
		}
		if in.RunAt != nil {
			return errors.New("store: schedule: run at is only valid for kind once")
		}
	default: // continuous
		if in.RunAt != nil {
			return errors.New("store: schedule: run at is only valid for kind once")
		}
		if in.IntervalNs != 0 {
			return errors.New("store: schedule: kind continuous must not carry an interval")
		}
	}
	return nil
}

func scheduleFromRow(s *gen.CheckSchedule) Schedule {
	return Schedule{
		ID:           formatUUID(s.ID),
		DefinitionID: formatUUID(s.DefinitionID),
		Kind:         s.Kind,
		IntervalNs:   s.IntervalNs,
		RunAt:        nullTime(s.RunAt),
		Enabled:      s.Enabled,
		LastFiredAt:  nullTime(s.LastFiredAt),
		NextFireAt:   nullTime(s.NextFireAt),
		CreatedAt:    s.CreatedAt,
		UpdatedAt:    s.UpdatedAt,
	}
}

func (db *DB) CreateSchedule(ctx context.Context, in ScheduleInput) (Schedule, error) { //nolint:gocritic // hugeParam: ScheduleInput mirrors the other write-payload structs in this package
	if err := in.Validate(); err != nil {
		return Schedule{}, err
	}
	sid, err := parseUUID(uuid.NewString())
	if err != nil {
		return Schedule{}, fmt.Errorf("store: create schedule: %w", err)
	}
	defID, err := parseUUID(in.DefinitionID)
	if err != nil {
		return Schedule{}, fmt.Errorf("store: create schedule: %w", err)
	}
	s, err := gen.New(db.pool).CreateSchedule(ctx, gen.CreateScheduleParams{
		ID:           sid,
		DefinitionID: defID,
		Kind:         in.Kind,
		IntervalNs:   in.IntervalNs,
		RunAt:        timestamptzFromPtr(in.RunAt),
		Enabled:      in.Enabled,
		NextFireAt:   timestamptzFromPtr(in.NextFireAt),
	})
	if err != nil {
		return Schedule{}, fmt.Errorf("store: create schedule: %w", wrapForeignKeyViolation(err, ErrNotFound))
	}
	return scheduleFromRow(&s), nil
}

func (db *DB) UpdateSchedule(ctx context.Context, id string, in ScheduleInput) (Schedule, error) { //nolint:gocritic // hugeParam: ScheduleInput mirrors the other write-payload structs in this package
	if err := in.Validate(); err != nil {
		return Schedule{}, err
	}
	sid, err := parseUUID(id)
	if err != nil {
		return Schedule{}, fmt.Errorf("store: update schedule: %w", err)
	}
	s, err := gen.New(db.pool).UpdateSchedule(ctx, gen.UpdateScheduleParams{
		ID:         sid,
		Kind:       in.Kind,
		IntervalNs: in.IntervalNs,
		RunAt:      timestamptzFromPtr(in.RunAt),
		Enabled:    in.Enabled,
		NextFireAt: timestamptzFromPtr(in.NextFireAt),
	})
	if err != nil {
		return Schedule{}, fmt.Errorf("store: update schedule: %w", wrapNoRows(err))
	}
	return scheduleFromRow(&s), nil
}

func (db *DB) DeleteSchedule(ctx context.Context, id string) error {
	sid, err := parseUUID(id)
	if err != nil {
		return fmt.Errorf("store: delete schedule: %w", err)
	}
	rows, err := gen.New(db.pool).DeleteSchedule(ctx, sid)
	if err != nil {
		return fmt.Errorf("store: delete schedule: %w", err)
	}
	if rows == 0 {
		return fmt.Errorf("store: delete schedule: %w", ErrNotFound)
	}
	return nil
}

func (db *DB) MarkScheduleFired(ctx context.Context, id string, firedAt time.Time, nextFireAt *time.Time) error {
	sid, err := parseUUID(id)
	if err != nil {
		return fmt.Errorf("store: mark schedule fired: %w", err)
	}
	rows, err := gen.New(db.pool).MarkScheduleFired(ctx, gen.MarkScheduleFiredParams{
		ID:          sid,
		LastFiredAt: pgtype.Timestamptz{Time: firedAt, Valid: true},
		NextFireAt:  timestamptzFromPtr(nextFireAt),
	})
	if err != nil {
		return fmt.Errorf("store: mark schedule fired: %w", err)
	}
	if rows == 0 {
		return fmt.Errorf("store: mark schedule fired: %w", ErrNotFound)
	}
	return nil
}

func (db *DB) GetSchedule(ctx context.Context, id string) (Schedule, error) {
	sid, err := parseUUID(id)
	if err != nil {
		return Schedule{}, fmt.Errorf("store: get schedule: %w", err)
	}
	s, err := gen.New(db.pool).GetSchedule(ctx, sid)
	if err != nil {
		return Schedule{}, fmt.Errorf("store: get schedule: %w", wrapNoRows(err))
	}
	return scheduleFromRow(&s), nil
}

func (db *DB) ListSchedules(ctx context.Context, f ScheduleFilter) (SchedulePage, error) { //nolint:gocritic // hugeParam: ScheduleFilter mirrors EventFilter's value semantics (events.go)
	limit := clampLimit(f.Limit)

	curTime, curID, err := decodeKeyset(f.Cursor)
	if err != nil {
		return SchedulePage{}, fmt.Errorf("store: list schedules: %w", err)
	}

	defID, err := optionalUUID(f.DefinitionID)
	if err != nil {
		return SchedulePage{}, fmt.Errorf("store: list schedules: %w", err)
	}

	rows, err := gen.New(db.pool).ListSchedules(ctx, gen.ListSchedulesParams{
		DefinitionID: defID,
		CurTime:      curTime,
		CurID:        curID,
		Lim:          int32(limit), //nolint:gosec // limit is clamped to [1,500] above
	})
	if err != nil {
		return SchedulePage{}, fmt.Errorf("store: list schedules: %w", err)
	}

	schedules := make([]Schedule, len(rows))
	for i := range rows {
		schedules[i] = scheduleFromRow(&rows[i])
	}

	var nextCursor string
	if len(rows) == limit {
		last := schedules[len(schedules)-1]
		nextCursor = EncodeUUIDCursor(last.CreatedAt, last.ID)
	}

	return SchedulePage{Schedules: schedules, NextCursor: nextCursor}, nil
}

// ListDueSchedules is the scheduler's due poll. limit == 0 means the default
// of 100 rows; any other value is clamped into [1,500].
func (db *DB) ListDueSchedules(ctx context.Context, due time.Time, limit int) ([]Schedule, error) {
	rows, err := gen.New(db.pool).ListDueSchedules(ctx, gen.ListDueSchedulesParams{
		Due: due,
		Lim: int32(clampLimit(limit)), //nolint:gosec // clampLimit bounds this to [1,500]
	})
	if err != nil {
		return nil, fmt.Errorf("store: list due schedules: %w", err)
	}
	schedules := make([]Schedule, len(rows))
	for i := range rows {
		schedules[i] = scheduleFromRow(&rows[i])
	}
	return schedules, nil
}

// ---------------------------------------------------------------------------
// Shared helpers
// ---------------------------------------------------------------------------

// decodeKeyset turns an opaque (created_at, UUID) cursor into the
// cur_time/cur_id params every listing in this file binds. An empty cursor
// yields two invalid (SQL NULL) values, which is what the queries'
// "cur_time IS NULL OR ..." clause needs to mean "first page".
func decodeKeyset(cursor string) (pgtype.Timestamptz, pgtype.UUID, error) {
	if cursor == "" {
		return pgtype.Timestamptz{}, pgtype.UUID{}, nil
	}
	ts, id, ok, err := DecodeUUIDCursor(cursor)
	if err != nil {
		return pgtype.Timestamptz{}, pgtype.UUID{}, err
	}
	if !ok {
		return pgtype.Timestamptz{}, pgtype.UUID{}, nil
	}
	cid, err := parseUUID(id)
	if err != nil {
		return pgtype.Timestamptz{}, pgtype.UUID{}, err
	}
	return pgtype.Timestamptz{Time: ts, Valid: true}, cid, nil
}

// optionalUUID parses an optional id: "" is SQL NULL (a filter that matches
// everything, or a column left unset), anything else must be a canonical
// UUID.
func optionalUUID(s string) (pgtype.UUID, error) {
	if s == "" {
		return pgtype.UUID{}, nil
	}
	return parseUUID(s)
}

// optionalUUIDString is optionalUUID's inverse: a SQL NULL reads back as "".
func optionalUUIDString(id pgtype.UUID) string {
	if !id.Valid {
		return ""
	}
	return formatUUID(id)
}

// orEmptyJSON normalises a JSONB payload to something the NOT NULL
// labels/params columns will accept, substituting {} -- the same value the
// columns' own DEFAULT '{}'::jsonb would supply, spelled explicitly so an
// UPDATE (which has no DEFAULT to fall back on) behaves identically to an
// INSERT. A fresh slice per call, never one shared package-level backing array
// a caller could scribble on.
//
// Three shapes collapse to {}:
//
//   - nil -- binds as SQL NULL, which the NOT NULL column rejects.
//   - an empty (len 0) but non-nil slice -- the shape json.Marshal of an
//     absent field or a hand-built json.RawMessage{} takes. It reaches the
//     driver as a zero-length jsonb literal, which Postgres rejects with a raw
//     "invalid input syntax for type json" the caller cannot act on.
//   - the literal JSON null. jsonb happily stores a JSON null, but "no labels"
//     and "labels explicitly set to null" are the same state to every reader
//     in this system, and only one of them can be the stored one. {} is
//     chosen because it is what the column defaults to, so a row written
//     without labels and a row written with null read back identically.
//
// Anything else is passed through untouched. Well-formedness is the input's
// Validate's job, not this function's -- by the time a payload gets here it
// has already been through json.Valid.
func orEmptyJSON(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), jsonNull) {
		return json.RawMessage(`{}`)
	}
	return raw
}

// jsonNull is the literal orEmptyJSON folds into {}. A package-level var, not
// a conversion per call, and never returned to a caller.
var jsonNull = []byte("null")

// validateJSON rejects a JSONB payload Postgres would refuse, naming the
// field so the caller learns which one it was. An empty or nil payload is
// fine: orEmptyJSON turns it into {} before it reaches the driver.
func validateJSON(field string, raw json.RawMessage) error {
	if len(raw) == 0 {
		return nil
	}
	if !json.Valid(raw) {
		return fmt.Errorf("%s must be valid JSON", field)
	}
	return nil
}
