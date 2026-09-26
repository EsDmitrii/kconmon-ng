package scheduler

import (
	"context"
	"strings"
	"testing"

	"github.com/EsDmitrii/kconmon-ng/internal/console/checks"
	"github.com/EsDmitrii/kconmon-ng/internal/console/store"
)

// A run of udp, dns or http toward a target or ad-hoc destination is refused by the agent for every
// pair, so a schedule of such a definition (a row written before the API refused it, or one whose
// definition was edited since) must not start a run, and says why on the row. No retry can change
// that, so a once schedule is retired with the reason instead of being re-queued every minute.
func TestScheduleOfATypeTheAgentRefusesTowardAnExternalDestinationStartsNoRun(t *testing.T) {
	for _, kind := range []string{kindInterval, kindOnce} {
		for _, tc := range []struct{ checkType, destKind string }{
			{"udp", "adhoc"}, {"dns", "target"}, {"http", "adhoc"},
		} {
			t.Run(kind+"-"+tc.checkType+"-"+tc.destKind, func(t *testing.T) {
				h := newHarness(t, true)
				def := store.Definition{
					ID: defID, Name: "edge", SourceSelection: "all", DestinationKind: tc.destKind,
					CheckType: tc.checkType, Plane: "pod", Enabled: true,
				}
				if tc.destKind == "target" {
					def.DestinationTargetID = targetID
					h.store.targets[targetID] = store.Target{ID: targetID, Name: "corp", Kind: "host", Address: "10.9.9.9:53"}
				} else {
					def.DestinationAddress = "10.0.0.1:53"
				}
				h.store.definitions[defID] = def
				h.seedSchedule(kind, int64(oneMinute))

				h.s.Tick(context.Background())

				if n := len(h.runner.started); n != 0 {
					t.Fatalf("started %d runs, want 0: the agent refuses every pair of this run", n)
				}
				assertRecordedAsUnrunnable(t, h, kind, func(lastErr string) bool {
					return strings.Contains(lastErr, tc.checkType) && strings.Contains(lastErr, "tcp, icmp and mtr")
				})
			})
		}
	}
}

// A definition whose plane the runner refuses (saved before the rule, or imported) is just as
// permanent: the reason lands on the row, and a once schedule is retired rather than retried.
func TestScheduleOfADefinitionWithAPlaneTheRunnerRefusesSaysWhy(t *testing.T) {
	for _, kind := range []string{kindInterval, kindOnce} {
		t.Run(kind, func(t *testing.T) {
			h := newHarness(t, true)
			h.seedDefinition()
			h.seedSchedule(kind, int64(oneMinute))
			h.runner.startErr = checks.ErrInvalidPlane

			h.s.Tick(context.Background())

			assertRecordedAsUnrunnable(t, h, kind, func(lastErr string) bool {
				return strings.Contains(lastErr, `plane must be "pod"`)
			})
		})
	}
}

// assertRecordedAsUnrunnable checks the one write a refusal no retry can clear makes: a fire record
// carrying the reason, the next interval occurrence for an interval schedule and none for a once.
func assertRecordedAsUnrunnable(t *testing.T, h *harness, kind string, reasonOK func(string) bool) {
	t.Helper()
	if len(h.store.skips) != 0 {
		t.Errorf("MarkScheduleSkipped called %+v, want never: a skip keeps no reason and re-queues the occurrence", h.store.skips)
	}
	if len(h.store.marks) != 1 {
		t.Fatalf("MarkScheduleFired called %d times, want 1", len(h.store.marks))
	}
	mark := h.store.marks[0]
	if !reasonOK(mark.lastErr) {
		t.Errorf("lastError = %q, want it to say why no run can start", mark.lastErr)
	}
	switch {
	case kind == kindOnce && mark.next != nil:
		t.Errorf("once schedule re-queued for %v, want it retired", mark.next)
	case kind == kindInterval && mark.next == nil:
		t.Error("interval schedule retired, want its next occurrence kept")
	}
}

// mtr is the one type beyond tcp and icmp that the agent runs toward an external destination.
func TestScheduleOfMTRTowardAnExternalDestinationStillFires(t *testing.T) {
	h := newHarness(t, true)
	h.seedDefinition()
	def := h.store.definitions[defID]
	def.CheckType = "mtr"
	h.store.definitions[defID] = def
	h.seedSchedule(kindInterval, int64(oneMinute))

	h.s.Tick(context.Background())

	if n := len(h.runner.started); n != 1 {
		t.Fatalf("started %d runs, want 1", n)
	}
}
