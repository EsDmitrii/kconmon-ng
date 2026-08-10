package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// validAnnotationInput is the baseline every AnnotationInput case below
// mutates one field of, so a failure names exactly the field under test.
func validAnnotationInput() AnnotationInput {
	return AnnotationInput{
		StartAt:   time.Now().UTC(),
		Scope:     "node-a",
		Text:      "rolled the CNI upgrade",
		CreatedBy: "user:admin",
	}
}

func TestAnnotationInputValidateAcceptsWellFormed(t *testing.T) {
	start := time.Now().UTC()
	end := start.Add(time.Hour)

	cases := []struct {
		name string
		in   AnnotationInput
	}{
		{"instant mark", validAnnotationInput()},
		{"range", func() AnnotationInput { in := validAnnotationInput(); in.EndAt = &end; return in }()},
		{"zero-length range", func() AnnotationInput {
			in := validAnnotationInput()
			in.StartAt = start
			in.EndAt = &start
			return in
		}()},
		{"global scope", func() AnnotationInput { in := validAnnotationInput(); in.Scope = ""; return in }()},
		{"max scope", func() AnnotationInput {
			in := validAnnotationInput()
			in.Scope = strings.Repeat("s", annotationScopeMaxLen)
			return in
		}()},
		{"one byte of text", func() AnnotationInput { in := validAnnotationInput(); in.Text = "x"; return in }()},
		{"max text", func() AnnotationInput {
			in := validAnnotationInput()
			in.Text = strings.Repeat("t", annotationTextMaxLen)
			return in
		}()},
		{"no author", func() AnnotationInput { in := validAnnotationInput(); in.CreatedBy = ""; return in }()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := tc.in
			if err := in.Validate(); err != nil {
				t.Errorf("Validate(%+v) = %v, want nil", tc.in, err)
			}
		})
	}
}

func TestAnnotationInputValidateRejects(t *testing.T) {
	before := time.Now().UTC().Add(-time.Hour)

	cases := []struct {
		name   string
		mutate func(*AnnotationInput)
	}{
		{"zero start", func(in *AnnotationInput) { in.StartAt = time.Time{} }},
		{"empty text", func(in *AnnotationInput) { in.Text = "" }},
		{"text over 1024 bytes", func(in *AnnotationInput) {
			in.Text = strings.Repeat("t", annotationTextMaxLen+1)
		}},
		{"scope over 255 bytes", func(in *AnnotationInput) {
			in.Scope = strings.Repeat("s", annotationScopeMaxLen+1)
		}},
		{"end before start", func(in *AnnotationInput) { in.EndAt = &before }},
		{"created by over 255 bytes", func(in *AnnotationInput) {
			in.CreatedBy = strings.Repeat("u", annotationScopeMaxLen+1)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := validAnnotationInput()
			tc.mutate(&in)
			if err := in.Validate(); err == nil {
				t.Errorf("Validate(%+v) = nil, want an error", in)
			}
		})
	}
}

// TestAnnotationTextLengthIsBytesNotRunes pins which unit the 1024 bound is in: the column stores
// bytes.
func TestAnnotationTextLengthIsBytesNotRunes(t *testing.T) {
	in := validAnnotationInput()
	in.Text = strings.Repeat("ы", annotationTextMaxLen) // 2 bytes per rune
	if err := in.Validate(); err == nil {
		t.Error("Validate accepted 1024 two-byte runes, want the byte bound to reject it")
	}
}

// TestDeleteAnnotationMalformedIDIsNotFoundWithoutTouchingPgx mirrors the run
// readers' pre-check (checks_test.go): the *DB has a NIL pool, so a clean
// return is itself proof no round trip was attempted.
func TestDeleteAnnotationMalformedIDIsNotFoundWithoutTouchingPgx(t *testing.T) {
	db := &DB{}
	ctx := context.Background()

	for _, id := range []string{"", "not-a-uuid", "../../etc/passwd", "1234", "%00"} {
		t.Run(id, func(t *testing.T) {
			if err := db.DeleteAnnotation(ctx, id); !errors.Is(err, ErrNotFound) {
				t.Fatalf("DeleteAnnotation(%q) err = %v, want ErrNotFound", id, err)
			}
		})
	}
}
