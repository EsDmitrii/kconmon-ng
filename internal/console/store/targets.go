package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/EsDmitrii/kconmon-ng/internal/console/store/gen"
)

// ErrInUse is returned when a delete is refused because another row still references the one being
// deleted.
var ErrInUse = errors.New("store: in use")

// foreignKeyViolationCode is PostgreSQL's SQLSTATE for a
// foreign_key_violation, the counterpart to uniqueViolationCode (auth.go). PostgreSQL raises it for
// BOTH directions of a referential constraint -- the INSERT/UPDATE that names a missing parent and
// the ON DELETE RESTRICT that still has children -- which the targets integration suite pins.
const foreignKeyViolationCode = "23503"

// wrapForeignKeyViolation turns a foreign-key PgError into sentinel, leaving every other error
// (including a nil one) unchanged.
func wrapForeignKeyViolation(err, sentinel error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == foreignKeyViolationCode {
		return sentinel
	}
	return err
}

// nameMaxLen mirrors the CHECK (length(name) BETWEEN 1 AND 63) both targets and check_definitions
// carry; validation rejects an over-long name here so a caller gets a plain "name too long" instead
// of a raw constraint violation from the driver.
const nameMaxLen = 63

// nameRE bounds what may become a Prometheus label value: ASCII alphanumerics plus '-', '_' and
// '.', never leading or trailing.
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
	// Labels is a JSON object. nil, an empty slice and the literal JSON null are all written as {}
	// (see orEmptyJSON).
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

// TargetReader is the read seam: httpapi lists, the assignment planner and the runner resolve one
// by id.
type TargetReader interface {
	GetTarget(ctx context.Context, id string) (Target, error)
	ListTargets(ctx context.Context, f TargetFilter) (TargetPage, error)
}

var _ TargetReader = (*DB)(nil)

// targetKinds is the closed set migration 00004's "-- host | url" comment names; enforced here
// rather than by a CHECK constraint so widening it in a later milestone is code.
var targetKinds = map[string]bool{"host": true, "url": true}

// Validate reports whether in is a well-formed target; it runs before the INSERT/UPDATE so a caller
// gets a precise.
func (in *TargetInput) Validate() error {
	if err := validateName(in.Name); err != nil {
		return fmt.Errorf("store: target: %w", err)
	}
	if !targetKinds[in.Kind] {
		return fmt.Errorf("store: target: kind %q must be one of host, url", in.Kind)
	}
	in.Address = strings.TrimSpace(in.Address)
	if in.Address == "" {
		return errors.New("store: target: address must not be empty")
	}
	if err := validateTargetAddress(in.Kind, in.Address); err != nil {
		return fmt.Errorf("store: target: %w", err)
	}
	if err := validateStringMap("labels", in.Labels); err != nil {
		return fmt.Errorf("store: target: %w", err)
	}
	return nil
}

// validateTargetAddress rejects an address the agent could never dial FOR THE KIND IT WAS FILED
// UNDER.
func validateTargetAddress(kind, address string) error {
	if kind == "url" {
		return validateHTTPAddress("address", address)
	}
	return validateHostAddress("address", address)
}

// validateHTTPAddress is checker.validateExternalHTTP's rule: an http(s) URL with a host.
func validateHTTPAddress(field, address string) error {
	u, err := url.Parse(strings.TrimSpace(address))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
		return fmt.Errorf("%s %q must be an http(s) URL with a host", field, address)
	}
	return nil
}

// validateHostAddress is the allowlist half of the same derivation: an IP literal or a resolvable
// name.
func validateHostAddress(field, address string) error {
	value := strings.TrimSpace(address)
	host := value
	if h, p, err := net.SplitHostPort(value); err == nil {
		// SplitHostPort accepted it, which is exactly the branch both senders
		// take -- so the port has to survive their own parse too, or the whole
		// string travels to DNS as a name and cannot resolve.
		n, convErr := strconv.ParseUint(p, 10, 16)
		if convErr != nil || n == 0 {
			return fmt.Errorf("%s %q has no usable port; a port must be 1-65535", field, address)
		}
		host = h
	}
	if isIPLiteral(host) || isHostname(host) {
		return nil
	}
	return fmt.Errorf("%s %q must be a host, an IP, or host:port", field, address)
}

// validateName applies the shared name rule targets and check_definitions
// both carry.
/*
validateNoControlChars refuses a control character in a free-text field.

PostgreSQL cannot store a NUL in a text column (SQLSTATE 22021) and the driver refuses the whole
statement, so one byte in a request body came back as a driver error — and the handlers map any
store error that is not a validation error to 502 "<subsystem> unavailable". A client's own input
therefore told the operator that maintenance windows, incidents or webhooks were DOWN. Every text
FILTER in httpapi is already guarded this way (rejectControlChars); this is the write side, and it
belongs here so every caller of a Validate gets it, not only the HTTP one.

Nothing legitimate carries one: these are names, titles, reasons and URLs.
*/
func validateNoControlChars(field, v string) error {
	if idx := strings.IndexFunc(v, unicode.IsControl); idx >= 0 {
		return fmt.Errorf("%s contains a control character at byte %d", field, idx)
	}
	return nil
}

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
	// The id is minted here rather than by a column DEFAULT so the whole package keeps one id story.
	tid, err := parseUUID(uuid.NewString())
	if err != nil {
		return Target{}, fmt.Errorf("store: create target: %w", err)
	}
	start := time.Now()
	t, err := gen.New(db.pool).CreateTarget(ctx, gen.CreateTargetParams{
		ID:      tid,
		Name:    in.Name,
		Kind:    in.Kind,
		Address: in.Address,
		Labels:  orEmptyJSON(in.Labels),
	})
	db.observe(queryCreateTarget, start, queryResult(err))
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
	start := time.Now()
	t, err := gen.New(db.pool).UpdateTarget(ctx, gen.UpdateTargetParams{
		ID:      tid,
		Name:    in.Name,
		Kind:    in.Kind,
		Address: in.Address,
		Labels:  orEmptyJSON(in.Labels),
	})
	db.observe(queryUpdateTarget, start, queryResult(err))
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
	start := time.Now()
	rows, err := gen.New(db.pool).DeleteTarget(ctx, tid)
	db.observe(queryDeleteTarget, start, queryResult(err))
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
	start := time.Now()
	t, err := gen.New(db.pool).GetTarget(ctx, tid)
	db.observe(queryGetTarget, start, queryResult(err))
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

	start := time.Now()
	rows, err := gen.New(db.pool).ListTargets(ctx, gen.ListTargetsParams{
		Kind:    kind,
		CurTime: curTime,
		CurID:   curID,
		Lim:     int32(limit), //nolint:gosec // limit is clamped to [1,500] above
	})
	db.observe(queryListTargets, start, queryResult(err))
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

// DefinitionReader is the read seam: httpapi lists, the scheduler resolves one by id when a
// schedule fires.
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

// Validate reports whether in is a well-formed definition; beyond the enumerations it enforces the
// one cross-field rule the schema states but cannot express.
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
	if in.DestinationKind == "adhoc" {
		if in.DestinationAddress == "" {
			return errors.New("store: definition: destination kind adhoc requires a destination address")
		}
		if err := ValidateAdhocAddress(in.DestinationAddress); err != nil {
			return fmt.Errorf("store: definition: %w", err)
		}
	}
	if err := validateJSON("params", in.Params); err != nil {
		return fmt.Errorf("store: definition: %w", err)
	}
	return nil
}

// hostLabelRE bounds ONE label of a DNS name. Underscores are permitted
// because Go's own resolver permits them (net.isDomainName): this rule must
// not refuse a name the agent would happily resolve.
var hostLabelRE = regexp.MustCompile(`^[A-Za-z0-9_]([A-Za-z0-9_-]*[A-Za-z0-9_])?$`)

const (
	hostnameMaxLen  = 253
	hostLabelMaxLen = 63
)

// ValidateAdhocAddress rejects a destination_address the agent could never dial. Exported because a
// SAVED definition and a one-off run typed into the diagnostics form must be judged by one rule --
// httpapi's POST /api/v1/runs calls it for destinationKind=adhoc.
func ValidateAdhocAddress(address string) error {
	value := strings.TrimSpace(address)
	if value == "" {
		return errors.New("destination address must not be blank")
	}

	lower := strings.ToLower(value)
	if strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://") {
		// The prefix already settled the scheme, so only the host is left to
		// check -- and the message stays the URL-specific one round 4 pinned.
		if err := validateHTTPAddress("destination address", value); err != nil {
			return fmt.Errorf("destination address %q is not a valid http(s) URL", address)
		}
		return nil
	}

	if err := validateHostAddress("destination address", value); err != nil {
		if strings.Contains(err.Error(), "usable port") {
			return fmt.Errorf("destination address %q has no usable port; a port must be 1-65535", address)
		}
		return fmt.Errorf(
			"destination address %q must be a host, an IP, host:port, or an http(s) URL", address)
	}
	return nil
}

// isIPLiteral mirrors checker.parseLiteral: an address the allowlist checks as
// typed and never sends to DNS, bracketed IPv6 included.
func isIPLiteral(host string) bool {
	s := host
	if len(s) > 1 && s[0] == '[' && s[len(s)-1] == ']' {
		s = s[1 : len(s)-1]
	}
	_, err := netip.ParseAddr(s)
	return err == nil
}

// isHostname is "could LookupNetIP be asked this". One trailing dot is the
// fully-qualified spelling and is legal; any other empty label is a doubled
// dot and is not.
func isHostname(host string) bool {
	if host == "" || len(host) > hostnameMaxLen {
		return false
	}
	trimmed := strings.TrimSuffix(host, ".")
	for _, label := range strings.Split(trimmed, ".") {
		if label == "" || len(label) > hostLabelMaxLen || !hostLabelRE.MatchString(label) {
			return false
		}
	}
	return true
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
	start := time.Now()
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
	db.observe(queryCreateDefinition, start, queryResult(err))
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
	start := time.Now()
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
	db.observe(queryUpdateDefinition, start, queryResult(err))
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
	start := time.Now()
	rows, err := gen.New(db.pool).DeleteDefinition(ctx, did)
	db.observe(queryDeleteDefinition, start, queryResult(err))
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
	start := time.Now()
	d, err := gen.New(db.pool).GetDefinition(ctx, did)
	db.observe(queryGetDefinition, start, queryResult(err))
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

	start := time.Now()
	rows, err := gen.New(db.pool).ListDefinitions(ctx, gen.ListDefinitionsParams{
		TargetID: targetID,
		Enabled:  enabled,
		CurTime:  curTime,
		CurID:    curID,
		Lim:      int32(limit), //nolint:gosec // limit is clamped to [1,500] above
	})
	db.observe(queryListDefinitions, start, queryResult(err))
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
	// LastError is why the LAST fire produced no run, "" when it produced one.
	LastError   string
	LastErrorAt *time.Time
	CreatedAt   time.Time
	UpdatedAt   time.Time
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
	// MarkScheduleFired records a fire and the next one in a single UPDATE. nextFireAt == nil retires
	// the schedule from the due index without disabling it.
	MarkScheduleFired(ctx context.Context, id string, firedAt time.Time, nextFireAt *time.Time, lastError string) error
}

var _ ScheduleStore = (*DB)(nil)

// ScheduleReader is the read seam: httpapi lists, the scheduler polls
// ListDueSchedules. ErrNotFound for an unknown id -- and for a DefinitionID
// naming no definition on create, since the missing row is the definition's.
type ScheduleReader interface {
	GetSchedule(ctx context.Context, id string) (Schedule, error)
	ListSchedules(ctx context.Context, f ScheduleFilter) (SchedulePage, error)
	// ListDueSchedules returns up to limit enabled schedules whose next_fire_at has passed.
	ListDueSchedules(ctx context.Context, due time.Time, limit int) ([]Schedule, error)
}

var _ ScheduleReader = (*DB)(nil)

// scheduleKinds is the closed set migration 00004's "-- once | interval | continuous (cron: later
// milestone)" comment names.
var scheduleKinds = map[string]bool{"once": true, "interval": true, "continuous": true}

// Validate reports whether in is a well-formed schedule; it enforces the cross-field rules the
// schema states in a comment but cannot express.
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
		LastError:    s.LastError,
		LastErrorAt:  nullTime(s.LastErrorAt),
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
	start := time.Now()
	s, err := gen.New(db.pool).CreateSchedule(ctx, gen.CreateScheduleParams{
		ID:           sid,
		DefinitionID: defID,
		Kind:         in.Kind,
		IntervalNs:   in.IntervalNs,
		RunAt:        timestamptzFromPtr(in.RunAt),
		Enabled:      in.Enabled,
		NextFireAt:   timestamptzFromPtr(in.NextFireAt),
	})
	db.observe(queryCreateSchedule, start, queryResult(err))
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
	start := time.Now()
	s, err := gen.New(db.pool).UpdateSchedule(ctx, gen.UpdateScheduleParams{
		ID:         sid,
		Kind:       in.Kind,
		IntervalNs: in.IntervalNs,
		RunAt:      timestamptzFromPtr(in.RunAt),
		Enabled:    in.Enabled,
		NextFireAt: timestamptzFromPtr(in.NextFireAt),
	})
	db.observe(queryUpdateSchedule, start, queryResult(err))
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
	start := time.Now()
	rows, err := gen.New(db.pool).DeleteSchedule(ctx, sid)
	db.observe(queryDeleteSchedule, start, queryResult(err))
	if err != nil {
		return fmt.Errorf("store: delete schedule: %w", err)
	}
	if rows == 0 {
		return fmt.Errorf("store: delete schedule: %w", ErrNotFound)
	}
	return nil
}

// scheduleErrorMaxLen bounds what one fire may write into last_error.
const scheduleErrorMaxLen = 500

// truncateScheduleError caps lastError at scheduleErrorMaxLen BYTES INCLUDING the ellipsis it
// appends.
func truncateScheduleError(lastError string) string {
	if len(lastError) <= scheduleErrorMaxLen {
		return lastError
	}
	const ellipsis = "…"
	budget := scheduleErrorMaxLen - len(ellipsis)
	cut := 0
	for i := range lastError { // range over a string yields rune-start offsets
		if i > budget {
			break
		}
		cut = i
	}
	return lastError[:cut] + ellipsis
}

func (db *DB) MarkScheduleFired(
	ctx context.Context, id string, firedAt time.Time, nextFireAt *time.Time, lastError string,
) error {
	sid, err := parseUUID(id)
	if err != nil {
		return fmt.Errorf("store: mark schedule fired: %w", err)
	}
	start := time.Now()
	rows, err := gen.New(db.pool).MarkScheduleFired(ctx, gen.MarkScheduleFiredParams{
		ID:          sid,
		LastFiredAt: pgtype.Timestamptz{Time: firedAt, Valid: true},
		NextFireAt:  timestamptzFromPtr(nextFireAt),
		LastError:   truncateScheduleError(lastError),
	})
	db.observe(queryMarkScheduleFired, start, queryResult(err))
	if err != nil {
		return fmt.Errorf("store: mark schedule fired: %w", err)
	}
	if rows == 0 {
		return fmt.Errorf("store: mark schedule fired: %w", ErrNotFound)
	}
	return nil
}

// MarkScheduleSkipped advances a schedule's cadence for an occurrence that produced no run at
// all -- so it is not re-selected every tick -- while leaving last_fired_at untouched, because
// nothing fired.
func (db *DB) MarkScheduleSkipped(ctx context.Context, id string, nextFireAt *time.Time) error {
	sid, err := parseUUID(id)
	if err != nil {
		return fmt.Errorf("store: mark schedule skipped: %w", err)
	}
	start := time.Now()
	rows, err := gen.New(db.pool).MarkScheduleSkipped(ctx, gen.MarkScheduleSkippedParams{
		ID:         sid,
		NextFireAt: timestamptzFromPtr(nextFireAt),
	})
	db.observe(queryMarkScheduleSkipped, start, queryResult(err))
	if err != nil {
		return fmt.Errorf("store: mark schedule skipped: %w", err)
	}
	if rows == 0 {
		return fmt.Errorf("store: mark schedule skipped: %w", ErrNotFound)
	}
	return nil
}

func (db *DB) GetSchedule(ctx context.Context, id string) (Schedule, error) {
	sid, err := parseUUID(id)
	if err != nil {
		return Schedule{}, fmt.Errorf("store: get schedule: %w", err)
	}
	start := time.Now()
	s, err := gen.New(db.pool).GetSchedule(ctx, sid)
	db.observe(queryGetSchedule, start, queryResult(err))
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

	start := time.Now()
	rows, err := gen.New(db.pool).ListSchedules(ctx, gen.ListSchedulesParams{
		DefinitionID: defID,
		CurTime:      curTime,
		CurID:        curID,
		Lim:          int32(limit), //nolint:gosec // limit is clamped to [1,500] above
	})
	db.observe(queryListSchedules, start, queryResult(err))
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
	start := time.Now()
	rows, err := gen.New(db.pool).ListDueSchedules(ctx, gen.ListDueSchedulesParams{
		Due: due,
		Lim: int32(clampLimit(limit)), //nolint:gosec // clampLimit bounds this to [1,500]
	})
	db.observe(queryListDueSchedules, start, queryResult(err))
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

// decodeKeyset turns an opaque (created_at, UUID) cursor into the cur_time/cur_id params every
// listing in this file binds.
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

// orEmptyJSON normalises a JSONB payload to something the NOT NULL labels/params columns will
// accept, substituting {}.
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
/*
 * validateStringMap is validateJSON plus the VALUE type, for `labels`.
 *
 * The published schema says `additionalProperties: {type: string}` and the store accepted anything:
 * a label whose value was an object, an array or a number went into the column and came back out to
 * every consumer that reasonably expects a string — a metric label, a filter, a rendered chip. The
 * lengths are bounded for the same reason a target's name is: these are display text and,
 * downstream, potential label values.
 */
func validateStringMap(field string, raw json.RawMessage) error {
	if err := validateJSON(field, raw); err != nil {
		return err
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, jsonNull) {
		return nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &m); err != nil {
		return fmt.Errorf("%s must be a JSON object of strings", field)
	}
	for k, v := range m {
		if k == "" || len(k) > labelKeyMaxLen {
			return fmt.Errorf("%s: key %q is %d bytes, limit is 1..%d", field, k, len(k), labelKeyMaxLen)
		}
		var str string
		if err := json.Unmarshal(v, &str); err != nil {
			return fmt.Errorf("%s: value of %q must be a string", field, k)
		}
		if len(str) > labelValueMaxLen {
			return fmt.Errorf("%s: value of %q is %d bytes, limit is %d", field, k, len(str), labelValueMaxLen)
		}
	}
	return nil
}

// The bounds validateStringMap enforces; label text is display text and, downstream, a candidate
// metric label value.
const (
	labelKeyMaxLen   = 63
	labelValueMaxLen = 253

	// jsonFieldMaxBytes bounds one JSONB column's payload; see validateJSON.
	jsonFieldMaxBytes = 16 * 1024
)

/*
validateNoJSONNUL refuses a NUL anywhere inside a JSONB payload.

PostgreSQL rejects a NUL in a jsonb value (SQLSTATE 22P05) and the driver fails the statement, so
one escaped \u0000 in a target's labels, a definition's params, an alert rule's annotations or an
incident's notes came back as 502 "<subsystem> unavailable" — a client's own input reported as an
outage. Every one of those routes had size and shape validation already; this is the byte that shape
validation cannot see, because \u0000 IS valid JSON.

The escape is the only reachable form: a literal NUL byte cannot appear in valid JSON, and
json.Valid has already run above.
*/
func validateNoJSONNUL(field string, raw json.RawMessage) error {
	if bytes.Contains(bytes.ToLower(raw), []byte(`\u0000`)) {
		return fmt.Errorf("%s must not contain a NUL character", field)
	}
	return nil
}

func validateJSON(field string, raw json.RawMessage) error {
	if len(raw) == 0 {
		return nil
	}
	/* A SIZE BOUND, like every other field this package stores.

	   Nothing limited a JSONB column's payload. The only ceiling was the HTTP body limit, so one
	   write could store megabytes of params on a check definition — and that row is then re-read and
	   re-marshalled by every GET /api/v1/checks, every GET /api/v1/export, the scheduler's spec
	   resolution and the definitions page, forever. A params object is a handful of typed knobs
	   (a port, a method, an expected status); 16 KiB is orders of magnitude above any real one and
	   still small enough that a thousand definitions is megabytes rather than gigabytes. */
	if len(raw) > jsonFieldMaxBytes {
		return fmt.Errorf("%s is %d bytes, limit is %d", field, len(raw), jsonFieldMaxBytes)
	}
	if !json.Valid(raw) {
		return fmt.Errorf("%s must be valid JSON", field)
	}
	if err := validateNoJSONNUL(field, raw); err != nil {
		return err
	}
	/* An OBJECT, which is what the schema declares and what every consumer indexes into: an array
	   or a scalar is legal JSON, went into the JSONB column happily, and then read back as
	   something no caller can use. `null` stays legal -- orEmptyJSON folds it into {} before the
	   driver sees it. */
	trimmed := bytes.TrimSpace(raw)
	if bytes.Equal(trimmed, jsonNull) {
		return nil
	}
	if trimmed[0] != '{' {
		return fmt.Errorf("%s must be a JSON object, not an array or a scalar", field)
	}
	return nil
}
