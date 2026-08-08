package store

import (
	"context"
	"strings"
	"testing"
	"time"
)

// validK8sEventInput is the baseline every K8sEventInput case below mutates
// one field of, so a failure names exactly the field under test.
func validK8sEventInput() K8sEventInput {
	return K8sEventInput{
		UID:             "5f8d1e2a-0000-4000-8000-000000000001",
		ResourceVersion: "10241",
		EventTime:       time.Now().UTC(),
		Kind:            "Node",
		Name:            "node-a",
		Reason:          "NodeNotReady",
		Type:            "Warning",
		Message:         "Node node-a status is now: NodeNotReady",
		Count:           1,
	}
}

func TestK8sEventInputValidateAcceptsWellFormed(t *testing.T) {
	cases := []struct {
		name string
		in   K8sEventInput
	}{
		{"node event", validK8sEventInput()},
		{"pod event", func() K8sEventInput {
			in := validK8sEventInput()
			in.Kind = "Pod"
			in.Namespace = "kconmon"
			in.Type = "Normal"
			return in
		}()},
		{"empty namespace", validK8sEventInput()},
		{"empty reason", func() K8sEventInput { in := validK8sEventInput(); in.Reason = ""; return in }()},
		{"empty message", func() K8sEventInput { in := validK8sEventInput(); in.Message = ""; return in }()},
		{"zero count", func() K8sEventInput { in := validK8sEventInput(); in.Count = 0; return in }()},
		{"max name", func() K8sEventInput {
			in := validK8sEventInput()
			in.Name = strings.Repeat("n", k8sObjectNameMaxLen)
			return in
		}()},
		{"max message", func() K8sEventInput {
			in := validK8sEventInput()
			in.Message = strings.Repeat("m", k8sMessageMaxLen)
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

func TestK8sEventInputValidateRejects(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*K8sEventInput)
	}{
		{"empty uid", func(in *K8sEventInput) { in.UID = "" }},
		{"uid over 255 bytes", func(in *K8sEventInput) { in.UID = strings.Repeat("u", k8sUIDMaxLen+1) }},
		{"empty resource version", func(in *K8sEventInput) { in.ResourceVersion = "" }},
		{"resource version over 255 bytes", func(in *K8sEventInput) {
			in.ResourceVersion = strings.Repeat("9", k8sResourceVerLen+1)
		}},
		{"zero event time", func(in *K8sEventInput) { in.EventTime = time.Time{} }},
		{"empty kind", func(in *K8sEventInput) { in.Kind = "" }},
		{"unknown kind", func(in *K8sEventInput) { in.Kind = "Deployment" }},
		{"lowercase kind", func(in *K8sEventInput) { in.Kind = "node" }},
		{"empty name", func(in *K8sEventInput) { in.Name = "" }},
		{"name over 253 bytes", func(in *K8sEventInput) {
			in.Name = strings.Repeat("n", k8sObjectNameMaxLen+1)
		}},
		{"namespace over 253 bytes", func(in *K8sEventInput) {
			in.Namespace = strings.Repeat("s", k8sObjectNameMaxLen+1)
		}},
		{"reason over 255 bytes", func(in *K8sEventInput) {
			in.Reason = strings.Repeat("r", k8sReasonMaxLen+1)
		}},
		{"empty type", func(in *K8sEventInput) { in.Type = "" }},
		{"type over 64 bytes", func(in *K8sEventInput) { in.Type = strings.Repeat("t", k8sTypeMaxLen+1) }},
		{"message over 8192 bytes", func(in *K8sEventInput) {
			in.Message = strings.Repeat("m", k8sMessageMaxLen+1)
		}},
		{"negative count", func(in *K8sEventInput) { in.Count = -1 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := validK8sEventInput()
			tc.mutate(&in)
			if err := in.Validate(); err == nil {
				t.Errorf("Validate(%+v) = nil, want an error", in)
			}
		})
	}
}

// TestK8sEventKindIsAClosedSetButTypeIsNot pins the asymmetry the package
// documents and would otherwise be easy to "tidy up" into symmetry: kind is
// M6 Decision 3's own capture contract (nodes and pods, nothing else), so an
// unknown kind is a filter bug worth rejecting; type is core/v1's, which says
// new event types may be added, so pinning it to {Normal, Warning} would
// eventually drop real events for the crime of being new.
func TestK8sEventKindIsAClosedSetButTypeIsNot(t *testing.T) {
	in := validK8sEventInput()
	in.Kind = "ReplicaSet"
	if err := in.Validate(); err == nil {
		t.Error("Validate accepted kind=ReplicaSet, want the closed Node|Pod set to reject it")
	}

	in = validK8sEventInput()
	in.Type = "Informational"
	if err := in.Validate(); err != nil {
		t.Errorf("Validate rejected an unfamiliar type %q: %v; core/v1 may add types and the "+
			"capture must not silently drop them", in.Type, err)
	}
}

// TestInsertK8sEventValidatesBeforeTouchingPgx mirrors the run readers'
// pre-check (checks_test.go): the *DB has a NIL pool, so a clean (non-panicking)
// return is itself proof no round trip was attempted.
func TestInsertK8sEventValidatesBeforeTouchingPgx(t *testing.T) {
	db := &DB{}
	ctx := context.Background()

	inserted, err := db.InsertK8sEvent(ctx, K8sEventInput{})
	if err == nil {
		t.Fatal("InsertK8sEvent(zero input) = nil error, want a validation error")
	}
	if inserted {
		t.Error("InsertK8sEvent reported inserted=true for a rejected input")
	}
}

// TestListK8sEventsRejectsAMalformedCursorWithoutTouchingPgx asserts a corrupt
// cursor fails loudly rather than silently restarting pagination from the top,
// which for a caller polling a growing table loops forever. NIL pool again.
func TestListK8sEventsRejectsAMalformedCursorWithoutTouchingPgx(t *testing.T) {
	db := &DB{}
	ctx := context.Background()

	for _, cursor := range []string{"not-base64!!", "Zm9v", "MjAyNi0wMS0wMXxub3QtYS1udW1iZXI"} {
		t.Run(cursor, func(t *testing.T) {
			if _, err := db.ListK8sEvents(ctx, K8sEventFilter{Cursor: cursor}); err == nil {
				t.Errorf("ListK8sEvents(cursor=%q) = nil error, want a decode failure", cursor)
			}
		})
	}
}

// TestK8sEventCursorRoundTrips pins that ListK8sEvents pages on the BIGINT
// cursor family (EncodeCursor/DecodeCursor -- the topology_events one), not on
// the UUID family every M4/M5 listing uses: k8s_events' primary key is a
// BIGSERIAL, and a UUID cursor here would reject every cursor it minted.
func TestK8sEventCursorRoundTrips(t *testing.T) {
	at := time.Now().UTC().Truncate(time.Microsecond)
	cursor := EncodeCursor(at, 4242)

	gotAt, gotID, ok, err := DecodeCursor(cursor)
	if err != nil || !ok {
		t.Fatalf("DecodeCursor(%q) = (_, _, %v, %v), want ok with a nil error", cursor, ok, err)
	}
	if !gotAt.Equal(at) || gotID != 4242 {
		t.Errorf("DecodeCursor round trip = (%v, %d), want (%v, 4242)", gotAt, gotID, at)
	}
	if _, _, _, err := DecodeUUIDCursor(cursor); err == nil {
		t.Error("DecodeUUIDCursor accepted a bigint cursor: the two families must stay distinguishable")
	}
}

// sweepFunc is the shape prune.go drives every retention target through. The
// three M6 helpers are assigned to it below, so a signature drift is a compile
// error at the assignment rather than a sweep silently going missing from
// PruneOnce's list. There is deliberately no webhooks entry: that table has no
// sweep (see prune.go's table-label comment and
// TestWebhooksAreNotARetentionTable).
type sweepFunc = func(context.Context, time.Time, int32) (int64, error)

var (
	_ sweepFunc = (&DB{}).DeleteK8sEventsBefore
	_ sweepFunc = (&DB{}).DeleteIncidentsBefore
	_ sweepFunc = (&DB{}).DeleteMaintenanceWindowsBefore
)
