package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/EsDmitrii/kconmon-ng/internal/console/store/gen"
)

// The maintenance-window bounds.
const (
	maintenanceReasonMaxLen = 512
	maintenanceScopeMaxLen  = 255
)

// MaintenanceWindow is one planned-work interval; it renders as markArea on scoped charts and as a
// timeline row.
type MaintenanceWindow struct {
	ID string
	// Scope is "" for a global window; any other value names a node, a pair or
	// a target and is matched exactly -- the annotations vocabulary. A filter
	// key, NEVER a Prometheus label value.
	Scope string
	// StartAt and EndAt are both required and EndAt is strictly after StartAt,
	// enforced BOTH by Validate and by the table's own CHECK: a window with no
	// end is not a maintenance window, it is a state of the world.
	StartAt   time.Time
	EndAt     time.Time
	Reason    string
	CreatedBy string
	CreatedAt time.Time
}

// MaintenanceInput is CreateMaintenanceWindow's write payload.
type MaintenanceInput struct {
	Scope     string // "" = global
	StartAt   time.Time
	EndAt     time.Time
	Reason    string
	CreatedBy string
}

// MaintenanceFilter selects a page of windows. All fields optional; Limit is
// clamped to [1,500] the same way EventFilter.Limit is.
type MaintenanceFilter struct {
	// Scope is a POINTER for AnnotationFilter.Scope's reason: "" is the global
	// scope, a real value, so nil means "every scope" and a pointer to ""
	// means "the global ones only".
	Scope *string
	// From and To bound the window a maintenance window must OVERLAP, not the window it must be
	// contained in.
	From   time.Time // inclusive
	To     time.Time // exclusive
	Cursor string    // opaque keyset cursor from a previous page
	Limit  int
}

// MaintenancePage is one page of ListMaintenanceWindows results.
type MaintenancePage struct {
	Windows    []MaintenanceWindow
	NextCursor string // "" when the page is the last one
}

// MaintenanceStore is the write seam: httpapi creates and deletes windows, nothing else does.
type MaintenanceStore interface {
	CreateMaintenanceWindow(ctx context.Context, in MaintenanceInput) (MaintenanceWindow, error)
	// DeleteMaintenanceWindow returns ErrNotFound when id does not name a
	// window, including when it is not a UUID at all.
	DeleteMaintenanceWindow(ctx context.Context, id string) error
}

var _ MaintenanceStore = (*DB)(nil)

// MaintenanceReader is the read seam: the chart overlay, the Investigate
// timeline and the manage list all page through this one call.
type MaintenanceReader interface {
	ListMaintenanceWindows(ctx context.Context, f MaintenanceFilter) (MaintenancePage, error)
}

var _ MaintenanceReader = (*DB)(nil)

// Validate reports whether in is a well-formed window.
func (in *MaintenanceInput) Validate() error {
	// See validateNoControlChars: a NUL here came back as 502 "maintenance windows unavailable".
	for _, f := range [][2]string{{"scope", in.Scope}, {"reason", in.Reason}, {"created by", in.CreatedBy}} {
		if err := validateNoControlChars(f[0], f[1]); err != nil {
			return fmt.Errorf("store: maintenance window: %w", err)
		}
	}
	if in.StartAt.IsZero() {
		return errors.New("store: maintenance window: start at must not be zero")
	}
	if in.EndAt.IsZero() {
		return errors.New("store: maintenance window: end at must not be zero")
	}
	if !in.EndAt.After(in.StartAt) {
		return fmt.Errorf("store: maintenance window: end at %s must be after start at %s",
			in.EndAt.Format(time.RFC3339Nano), in.StartAt.Format(time.RFC3339Nano))
	}
	if in.Reason == "" {
		return errors.New("store: maintenance window: reason must not be empty")
	}
	if len(in.Reason) > maintenanceReasonMaxLen {
		return fmt.Errorf("store: maintenance window: reason is %d bytes, limit is %d",
			len(in.Reason), maintenanceReasonMaxLen)
	}
	if len(in.Scope) > maintenanceScopeMaxLen {
		return fmt.Errorf("store: maintenance window: scope is %d bytes, limit is %d",
			len(in.Scope), maintenanceScopeMaxLen)
	}
	if len(in.CreatedBy) > maintenanceScopeMaxLen {
		return fmt.Errorf("store: maintenance window: created by is %d bytes, limit is %d",
			len(in.CreatedBy), maintenanceScopeMaxLen)
	}
	return nil
}

func maintenanceFromRow(w *gen.MaintenanceWindow) MaintenanceWindow {
	return MaintenanceWindow{
		ID:        formatUUID(w.ID),
		Scope:     w.Scope,
		StartAt:   w.StartAt,
		EndAt:     w.EndAt,
		Reason:    w.Reason,
		CreatedBy: w.CreatedBy,
		CreatedAt: w.CreatedAt,
	}
}

func (db *DB) CreateMaintenanceWindow(ctx context.Context, in MaintenanceInput) (MaintenanceWindow, error) { //nolint:gocritic // hugeParam: MaintenanceInput mirrors the other write-payload structs in this package
	if err := in.Validate(); err != nil {
		return MaintenanceWindow{}, err
	}
	wid, err := parseUUID(uuid.NewString())
	if err != nil {
		return MaintenanceWindow{}, fmt.Errorf("store: create maintenance window: %w", err)
	}

	start := time.Now()
	row, err := gen.New(db.pool).CreateMaintenanceWindow(ctx, gen.CreateMaintenanceWindowParams{
		ID:        wid,
		Scope:     in.Scope,
		StartAt:   in.StartAt,
		EndAt:     in.EndAt,
		Reason:    in.Reason,
		CreatedBy: in.CreatedBy,
	})
	db.observe(queryCreateMaintenanceWindow, start, queryResult(err))
	if err != nil {
		return MaintenanceWindow{}, fmt.Errorf("store: create maintenance window: %w", err)
	}
	return maintenanceFromRow(&row), nil
}

func (db *DB) ListMaintenanceWindows(ctx context.Context, f MaintenanceFilter) (MaintenancePage, error) { //nolint:gocritic // hugeParam: MaintenanceFilter mirrors EventFilter's value semantics (events.go)
	limit := clampLimit(f.Limit)

	curTime, curID, err := decodeKeyset(f.Cursor)
	if err != nil {
		return MaintenancePage{}, fmt.Errorf("store: list maintenance windows: %w", err)
	}

	var scope pgtype.Text
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
	rows, err := gen.New(db.pool).ListMaintenanceWindows(ctx, gen.ListMaintenanceWindowsParams{
		Scope:    scope,
		FromTime: fromTime,
		ToTime:   toTime,
		CurTime:  curTime,
		CurID:    curID,
		Lim:      int32(limit), //nolint:gosec // limit is clamped to [1,500] above
	})
	db.observe(queryListMaintenanceWindows, start, queryResult(err))
	if err != nil {
		return MaintenancePage{}, fmt.Errorf("store: list maintenance windows: %w", err)
	}

	windows := make([]MaintenanceWindow, len(rows))
	for i := range rows {
		windows[i] = maintenanceFromRow(&rows[i])
	}

	var nextCursor string
	if len(rows) == limit {
		last := windows[len(windows)-1]
		nextCursor = EncodeUUIDCursor(last.StartAt, last.ID)
	}

	return MaintenancePage{Windows: windows, NextCursor: nextCursor}, nil
}

// DeleteMaintenanceWindow removes one window. Same pre-check and miss answer
// as DeleteAnnotation.
func (db *DB) DeleteMaintenanceWindow(ctx context.Context, id string) error {
	wid, err := parseUUID(id)
	if err != nil {
		return fmt.Errorf("store: delete maintenance window: %w: %w", ErrNotFound, err)
	}
	start := time.Now()
	rows, err := gen.New(db.pool).DeleteMaintenanceWindow(ctx, wid)
	db.observe(queryDeleteMaintenanceWindow, start, queryResult(err))
	if err != nil {
		return fmt.Errorf("store: delete maintenance window: %w", err)
	}
	if rows == 0 {
		return fmt.Errorf("store: delete maintenance window: %w", ErrNotFound)
	}
	return nil
}

// DeleteMaintenanceWindowsBefore deletes up to limit windows that ENDED before
// before, oldest end first, and reports how many were removed. Used by
// Pruner's sweep; exposed for the same testability reason as DeleteRunsBefore.
func (db *DB) DeleteMaintenanceWindowsBefore(ctx context.Context, before time.Time, limit int32) (int64, error) {
	n, err := gen.New(db.pool).DeleteMaintenanceWindowsBefore(ctx, gen.DeleteMaintenanceWindowsBeforeParams{
		EndAt: before,
		Limit: limit,
	})
	if err != nil {
		return 0, fmt.Errorf("store: delete maintenance windows before: %w", err)
	}
	return n, nil
}
