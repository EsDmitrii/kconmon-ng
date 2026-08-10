package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// validMaintenanceInput is the baseline every MaintenanceInput case below
// mutates one field of, so a failure names exactly the field under test.
func validMaintenanceInput() MaintenanceInput {
	start := time.Now().UTC()
	return MaintenanceInput{
		Scope:     "node-a",
		StartAt:   start,
		EndAt:     start.Add(2 * time.Hour),
		Reason:    "kernel upgrade, rolling reboot",
		CreatedBy: "user:admin",
	}
}

func TestMaintenanceInputValidateAcceptsWellFormed(t *testing.T) {
	cases := []struct {
		name string
		in   MaintenanceInput
	}{
		{"scoped window", validMaintenanceInput()},
		{"global scope", func() MaintenanceInput { in := validMaintenanceInput(); in.Scope = ""; return in }()},
		{"one nanosecond long", func() MaintenanceInput {
			in := validMaintenanceInput()
			in.EndAt = in.StartAt.Add(time.Nanosecond)
			return in
		}()},
		{"no author", func() MaintenanceInput { in := validMaintenanceInput(); in.CreatedBy = ""; return in }()},
		{"max reason", func() MaintenanceInput {
			in := validMaintenanceInput()
			in.Reason = strings.Repeat("r", maintenanceReasonMaxLen)
			return in
		}()},
		{"max scope", func() MaintenanceInput {
			in := validMaintenanceInput()
			in.Scope = strings.Repeat("s", maintenanceScopeMaxLen)
			return in
		}()},
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

func TestMaintenanceInputValidateRejects(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*MaintenanceInput)
	}{
		{"zero start", func(in *MaintenanceInput) { in.StartAt = time.Time{} }},
		{"zero end", func(in *MaintenanceInput) { in.EndAt = time.Time{} }},
		{"end before start", func(in *MaintenanceInput) { in.EndAt = in.StartAt.Add(-time.Minute) }},
		{"empty reason", func(in *MaintenanceInput) { in.Reason = "" }},
		{"reason over 512 bytes", func(in *MaintenanceInput) {
			in.Reason = strings.Repeat("r", maintenanceReasonMaxLen+1)
		}},
		{"scope over 255 bytes", func(in *MaintenanceInput) {
			in.Scope = strings.Repeat("s", maintenanceScopeMaxLen+1)
		}},
		{"created by over 255 bytes", func(in *MaintenanceInput) {
			in.CreatedBy = strings.Repeat("u", maintenanceScopeMaxLen+1)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := validMaintenanceInput()
			tc.mutate(&in)
			if err := in.Validate(); err == nil {
				t.Errorf("Validate(%+v) = nil, want an error", in)
			}
		})
	}
}

// TestMaintenanceEndMustBeStrictlyAfterStart pins the one boundary the CHECK constraint and
// Validate must agree on exactly.
func TestMaintenanceEndMustBeStrictlyAfterStart(t *testing.T) {
	in := validMaintenanceInput()
	in.EndAt = in.StartAt
	if err := in.Validate(); err == nil {
		t.Error("Validate accepted end_at == start_at; the CHECK is end_at > start_at, and the two must agree")
	}

	in = validMaintenanceInput()
	in.EndAt = in.StartAt.Add(time.Nanosecond)
	if err := in.Validate(); err != nil {
		t.Errorf("Validate rejected the shortest valid window: %v", err)
	}
}

// TestDeleteMaintenanceWindowMalformedIDIsNotFoundWithoutTouchingPgx mirrors
// the annotation readers' pre-check: the *DB has a NIL pool, so a clean return
// is itself proof no round trip was attempted.
func TestDeleteMaintenanceWindowMalformedIDIsNotFoundWithoutTouchingPgx(t *testing.T) {
	db := &DB{}
	ctx := context.Background()

	for _, id := range []string{"", "not-a-uuid", "../../etc/passwd", "1234", "%00"} {
		t.Run(id, func(t *testing.T) {
			if err := db.DeleteMaintenanceWindow(ctx, id); !errors.Is(err, ErrNotFound) {
				t.Fatalf("DeleteMaintenanceWindow(%q) err = %v, want ErrNotFound", id, err)
			}
		})
	}
}

// TestListMaintenanceWindowsRejectsAMalformedCursorWithoutTouchingPgx asserts
// a corrupt cursor fails loudly rather than silently restarting pagination.
// NIL pool.
func TestListMaintenanceWindowsRejectsAMalformedCursorWithoutTouchingPgx(t *testing.T) {
	db := &DB{}
	ctx := context.Background()

	for _, cursor := range []string{"not-base64!!", "Zm9v", "MjAyNi0wMS0wMXxub3QtYS11dWlk"} {
		t.Run(cursor, func(t *testing.T) {
			if _, err := db.ListMaintenanceWindows(ctx, MaintenanceFilter{Cursor: cursor}); err == nil {
				t.Errorf("ListMaintenanceWindows(cursor=%q) = nil error, want a decode failure", cursor)
			}
		})
	}
}

// TestCreateMaintenanceWindowValidatesBeforeTouchingPgx asserts validation
// runs before the INSERT. NIL pool, so a clean return proves it.
func TestCreateMaintenanceWindowValidatesBeforeTouchingPgx(t *testing.T) {
	db := &DB{}
	if _, err := db.CreateMaintenanceWindow(context.Background(), MaintenanceInput{}); err == nil {
		t.Error("CreateMaintenanceWindow(zero input) = nil error, want a validation error")
	}
}
