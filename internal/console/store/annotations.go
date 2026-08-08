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

// annotationTextMaxLen and annotationScopeMaxLen are M5 Decision 10's bounds.
// Both are counted in BYTES, not runes: the columns store bytes, and a caller
// that squeezed 1024 multi-byte runes past a rune-counting check would still
// be a caller whose note is four kilobytes.
//
// annotationScopeMaxLen also bounds created_by, which is a subject reference
// ("user:<name>", "token:<name>") and has no reason to be longer than a scope.
const (
	annotationTextMaxLen  = 1024
	annotationScopeMaxLen = 255
)

// Annotation is one operator note pinned to a moment or a span (M5 Decision
// 10). EndAt is nil for an instant mark -- which means "a point in time", not
// "still open": an annotation is a mark, not a document, which is also why M5
// has create/list/delete and no edit.
type Annotation struct {
	ID      string
	StartAt time.Time
	EndAt   *time.Time // nil = instant mark
	// Scope is "" for a global annotation; any other value names a node, a
	// pair or a target and is matched exactly. It is a filter key, NEVER a
	// Prometheus label value -- neither it nor Text is ever exported as one.
	Scope     string
	Text      string
	CreatedBy string
	CreatedAt time.Time
}

// AnnotationInput is CreateAnnotation's write payload.
type AnnotationInput struct {
	StartAt   time.Time
	EndAt     *time.Time // nil = instant mark
	Scope     string     // "" = global
	Text      string
	CreatedBy string
}

// AnnotationFilter selects a page of annotations. All fields optional; Limit
// is clamped to [1,500] the same way EventFilter.Limit is.
type AnnotationFilter struct {
	// Scope is a POINTER, unlike every other exact-match filter in this
	// package, because "" is a real value here: it is the global scope. nil
	// means "every scope"; a pointer to "" means "the global ones only".
	Scope *string
	// From and To bound the window an annotation must OVERLAP to be returned,
	// not the window its start must fall in: a span that began before From and
	// is still running at From is exactly the annotation a chart needs to
	// draw. An instant mark counts as the zero-length span [StartAt, StartAt].
	// Zero on either side is unbounded.
	From   time.Time // inclusive
	To     time.Time // exclusive
	Cursor string    // opaque keyset cursor from a previous page
	Limit  int
}

// AnnotationPage is one page of ListAnnotations results, same shape as
// TargetPage.
type AnnotationPage struct {
	Annotations []Annotation
	NextCursor  string // "" when the page is the last one
}

// AnnotationStore is the write seam: httpapi creates and deletes annotations,
// nothing else does. There is no update -- see Annotation's doc comment.
type AnnotationStore interface {
	CreateAnnotation(ctx context.Context, in AnnotationInput) (Annotation, error)
	// DeleteAnnotation returns ErrNotFound when id does not name an
	// annotation, including when it is not a UUID at all.
	DeleteAnnotation(ctx context.Context, id string) error
}

var _ AnnotationStore = (*DB)(nil)

// AnnotationReader is the read seam: httpapi lists for the chart markers and
// the Live scrollback.
type AnnotationReader interface {
	GetAnnotation(ctx context.Context, id string) (Annotation, error)
	ListAnnotations(ctx context.Context, f AnnotationFilter) (AnnotationPage, error)
}

var _ AnnotationReader = (*DB)(nil)

// Validate reports whether in is a well-formed annotation. The three rules are
// M5 Decision 10's: text 1..1024 bytes, end_at at or after start_at, scope at
// most 255 bytes. A zero StartAt is rejected on top of those -- the column is
// NOT NULL, so it would otherwise store year 1 and land the mark at the far
// left of every chart forever.
func (in *AnnotationInput) Validate() error {
	if in.StartAt.IsZero() {
		return errors.New("store: annotation: start at must not be zero")
	}
	if in.Text == "" {
		return errors.New("store: annotation: text must not be empty")
	}
	if len(in.Text) > annotationTextMaxLen {
		return fmt.Errorf("store: annotation: text is %d bytes, limit is %d", len(in.Text), annotationTextMaxLen)
	}
	if len(in.Scope) > annotationScopeMaxLen {
		return fmt.Errorf("store: annotation: scope is %d bytes, limit is %d", len(in.Scope), annotationScopeMaxLen)
	}
	if len(in.CreatedBy) > annotationScopeMaxLen {
		return fmt.Errorf("store: annotation: created by is %d bytes, limit is %d", len(in.CreatedBy), annotationScopeMaxLen)
	}
	if in.EndAt != nil && in.EndAt.Before(in.StartAt) {
		return fmt.Errorf("store: annotation: end at %s is before start at %s",
			in.EndAt.Format(time.RFC3339Nano), in.StartAt.Format(time.RFC3339Nano))
	}
	return nil
}

func annotationFromRow(a *gen.Annotation) Annotation {
	return Annotation{
		ID:        formatUUID(a.ID),
		StartAt:   a.StartAt,
		EndAt:     nullTime(a.EndAt),
		Scope:     a.Scope,
		Text:      a.Text,
		CreatedBy: a.CreatedBy,
		CreatedAt: a.CreatedAt,
	}
}

func (db *DB) CreateAnnotation(ctx context.Context, in AnnotationInput) (Annotation, error) { //nolint:gocritic // hugeParam: AnnotationInput mirrors the other write-payload structs in this package
	if err := in.Validate(); err != nil {
		return Annotation{}, err
	}
	aid, err := parseUUID(uuid.NewString())
	if err != nil {
		return Annotation{}, fmt.Errorf("store: create annotation: %w", err)
	}
	start := time.Now()
	a, err := gen.New(db.pool).CreateAnnotation(ctx, gen.CreateAnnotationParams{
		ID:        aid,
		StartAt:   in.StartAt,
		EndAt:     timestamptzFromPtr(in.EndAt),
		Scope:     in.Scope,
		Text:      in.Text,
		CreatedBy: in.CreatedBy,
	})
	db.observe(queryCreateAnnotation, start, queryResult(err))
	if err != nil {
		return Annotation{}, fmt.Errorf("store: create annotation: %w", err)
	}
	return annotationFromRow(&a), nil
}

// GetAnnotation applies GetRun's UUID pre-check: a malformed id is reported as
// ErrNotFound, not as a parse failure, so the edge answers 404 rather than
// "annotation store unavailable".
func (db *DB) GetAnnotation(ctx context.Context, id string) (Annotation, error) {
	aid, err := parseUUID(id)
	if err != nil {
		return Annotation{}, fmt.Errorf("store: get annotation: %w: %w", ErrNotFound, err)
	}
	start := time.Now()
	a, err := gen.New(db.pool).GetAnnotation(ctx, aid)
	db.observe(queryGetAnnotation, start, queryResult(wrapNoRows(err)))
	if err != nil {
		return Annotation{}, fmt.Errorf("store: get annotation: %w", wrapNoRows(err))
	}
	return annotationFromRow(&a), nil
}

// DeleteAnnotation removes one mark. Same pre-check as GetAnnotation, and the
// same answer for an id that names nothing: deleting a mark that is not there
// is ErrNotFound, not success -- the caller asked about a specific mark.
func (db *DB) DeleteAnnotation(ctx context.Context, id string) error {
	aid, err := parseUUID(id)
	if err != nil {
		return fmt.Errorf("store: delete annotation: %w: %w", ErrNotFound, err)
	}
	start := time.Now()
	rows, err := gen.New(db.pool).DeleteAnnotation(ctx, aid)
	db.observe(queryDeleteAnnotation, start, queryResult(err))
	if err != nil {
		return fmt.Errorf("store: delete annotation: %w", err)
	}
	if rows == 0 {
		return fmt.Errorf("store: delete annotation: %w", ErrNotFound)
	}
	return nil
}

func (db *DB) ListAnnotations(ctx context.Context, f AnnotationFilter) (AnnotationPage, error) { //nolint:gocritic // hugeParam: AnnotationFilter mirrors EventFilter's value semantics (events.go)
	limit := clampLimit(f.Limit)

	curTime, curID, err := decodeKeyset(f.Cursor)
	if err != nil {
		return AnnotationPage{}, fmt.Errorf("store: list annotations: %w", err)
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
	rows, err := gen.New(db.pool).ListAnnotations(ctx, gen.ListAnnotationsParams{
		Scope:    scope,
		FromTime: fromTime,
		ToTime:   toTime,
		CurTime:  curTime,
		CurID:    curID,
		Lim:      int32(limit), //nolint:gosec // limit is clamped to [1,500] above
	})
	db.observe(queryListAnnotations, start, queryResult(err))
	if err != nil {
		return AnnotationPage{}, fmt.Errorf("store: list annotations: %w", err)
	}

	anns := make([]Annotation, len(rows))
	for i := range rows {
		anns[i] = annotationFromRow(&rows[i])
	}

	var nextCursor string
	if len(rows) == limit {
		last := anns[len(anns)-1]
		nextCursor = EncodeUUIDCursor(last.StartAt, last.ID)
	}

	return AnnotationPage{Annotations: anns, NextCursor: nextCursor}, nil
}

// DeleteAnnotationsBefore deletes up to limit annotations starting before
// before, oldest first, and reports how many were removed. Used by Pruner's
// sweep; exposed for the same testability reason as DeleteRunsBefore.
func (db *DB) DeleteAnnotationsBefore(ctx context.Context, before time.Time, limit int32) (int64, error) {
	n, err := gen.New(db.pool).DeleteAnnotationsBefore(ctx, gen.DeleteAnnotationsBeforeParams{
		StartAt: before,
		Limit:   limit,
	})
	if err != nil {
		return 0, fmt.Errorf("store: delete annotations before: %w", err)
	}
	return n, nil
}
