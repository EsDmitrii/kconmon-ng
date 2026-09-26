package webhooks

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/EsDmitrii/kconmon-ng/internal/console/store"
)

/*
A console restarted inside a window while the windows table cannot be read does not know yet which
baseline alerts the previous process held. One of them resolving in that state used to go out as a
lone alert.resolved for an alert.fired the receiver never got. Its resolve now waits for the first
read that can decide, and that read judges it by the windows open when the baseline was taken.
*/

func TestAlertWatcherHoldsAnUncheckedResolveUntilWindowsAreReadable(t *testing.T) {
	src := newFakeAlertSource(
		alertReply{body: promBody(t, pairAlertActiveAt(testNow.Add(-30*time.Minute)))}, // began inside the window
		alertReply{body: promBody(t)}, // resolved, window still open, reads still failing
	)
	mt := &fakeMaintenance{windows: []store.MaintenanceWindow{windowAround("n1")}, err: errors.New("db down")}
	w, n, m := newMaintenanceWatcherWith(t, src, mt, nil)
	w.poll(context.Background())
	w.poll(context.Background())
	if got := n.recorded(); len(got) != 0 {
		t.Fatalf("delivered %+v while the hold could not be decided, want nothing", got)
	}

	mt.err = nil
	w.poll(context.Background())
	if got := n.recorded(); len(got) != 0 {
		t.Fatalf("delivered %+v, want nothing for an alert the previous process held", got)
	}
	if got := suppressedCount(m, store.WebhookEventAlertResolved); got != 1 {
		t.Errorf("WebhookSuppressed(alert.resolved) = %v, want 1", got)
	}
}

// The window may be over by the time the table is readable again; the decision still uses the
// windows that were open at the baseline, which is what the previous process held by.
func TestAlertWatcherDecidesAPendingResolveByTheBaselinesWindows(t *testing.T) {
	src := newFakeAlertSource(
		alertReply{body: promBody(t, pairAlertActiveAt(testNow.Add(-30*time.Minute)))},
		alertReply{body: promBody(t)},
	)
	mt := &fakeMaintenance{windows: []store.MaintenanceWindow{windowAround("n1")}, err: errors.New("db down")}
	w, n, _ := newMaintenanceWatcherWith(t, src, mt, nil)
	w.poll(context.Background())
	w.poll(context.Background())

	w.now = func() time.Time { return testNow.Add(2 * time.Hour) } // the window has closed
	mt.err = nil
	w.poll(context.Background())
	if got := n.recorded(); len(got) != 0 {
		t.Fatalf("delivered %+v after the window closed, want nothing for an alert held inside it", got)
	}
}

// An alert that fired before the window was delivered by the previous process, so its resolve goes
// out once the read can tell, carrying the time it actually resolved.
func TestAlertWatcherDeliversAPendingResolveOfAPreWindowAlert(t *testing.T) {
	src := newFakeAlertSource(
		alertReply{body: promBody(t, pairAlertActiveAt(testNow.Add(-3*time.Hour)))},
		alertReply{body: promBody(t)},
	)
	mt := &fakeMaintenance{windows: []store.MaintenanceWindow{windowAround("n1")}, err: errors.New("db down")}
	w, n, _ := newMaintenanceWatcherWith(t, src, mt, nil)
	w.poll(context.Background())
	w.poll(context.Background())
	if got := n.recorded(); len(got) != 0 {
		t.Fatalf("delivered %+v before the hold could be decided", got)
	}

	w.now = func() time.Time { return testNow.Add(time.Minute) }
	mt.err = nil
	w.poll(context.Background())
	got := n.recorded()
	if len(got) != 1 || got[0].event != store.WebhookEventAlertResolved {
		t.Fatalf("delivered %+v, want the alert.resolved of an alert that fired before the window", got)
	}
	if got[0].alert.ResolvedAt == nil || !got[0].alert.ResolvedAt.Equal(testNow.UTC()) {
		t.Errorf("resolvedAt = %v, want the poll that saw it resolve (%v)", got[0].alert.ResolvedAt, testNow.UTC())
	}
}

// Without maintenance windows nothing is ever unchecked, and a resolve goes out at once as before.
func TestAlertWatcherWithoutWindowsResolvesAtOnce(t *testing.T) {
	src := newFakeAlertSource(
		alertReply{body: promBody(t, pairAlertActiveAt(testNow.Add(-30*time.Minute)))},
		alertReply{body: promBody(t)},
	)
	w, n := newTestWatcher(t, src, nil)
	w.poll(context.Background())
	w.poll(context.Background())
	if got := n.recorded(); len(got) != 1 || got[0].event != store.WebhookEventAlertResolved {
		t.Fatalf("delivered %+v, want one alert.resolved", got)
	}
}

/*
The same restart, but the alert outlives the window and the table becomes readable only after the
window closed. The still-firing unchecked alert is judged by the windows open at the baseline, like a
pending resolve: the previous process held its alert.fired, so that goes out now, and the resolve that
follows completes the pair instead of arriving alone.
*/
func TestAlertWatcherDeliversTheHeldFiredOfAnUncheckedAlertAfterTheWindowClosed(t *testing.T) {
	activeAt := testNow.Add(-30 * time.Minute) // began inside the window
	firing := promBody(t, pairAlertActiveAt(activeAt))
	src := newFakeAlertSource(
		alertReply{body: firing}, // baseline, reads failing
		alertReply{body: firing}, // still failing
		alertReply{body: firing}, // readable, the window has closed, still firing
		alertReply{body: promBody(t)},
	)
	mt := &fakeMaintenance{windows: []store.MaintenanceWindow{windowAround("n1")}, err: errors.New("db down")}
	w, n, m := newMaintenanceWatcherWith(t, src, mt, nil)
	w.poll(context.Background())
	w.poll(context.Background())
	// One failed read per poll: the baseline judgement does not add a second one.
	if got := testutil.ToFloat64(m.WebhookMaintenanceReadErrors.WithLabelValues()); got != 2 {
		t.Errorf("WebhookMaintenanceReadErrors = %v after two polls, want 2", got)
	}

	w.now = func() time.Time { return testNow.Add(2 * time.Hour) }
	mt.err = nil
	w.poll(context.Background())
	assertOneFired(t, n.recorded(), activeAt)
	if got := suppressedCount(m, store.WebhookEventAlertFired); got != 1 {
		t.Errorf("WebhookSuppressed(alert.fired) = %v, want 1 for the edge the previous process held", got)
	}

	w.poll(context.Background())
	got := n.recorded()
	if len(got) != 2 || got[1].event != store.WebhookEventAlertResolved {
		t.Fatalf("delivered %+v, want alert.fired then alert.resolved", got)
	}
}

// An alert that fired before the window was delivered by the previous process: after the window
// closed it is known, not re-fired, and its resolve goes out.
func TestAlertWatcherDoesNotRefireAnUncheckedPreWindowAlertAfterTheWindowClosed(t *testing.T) {
	firing := promBody(t, pairAlertActiveAt(testNow.Add(-3*time.Hour)))
	src := newFakeAlertSource(
		alertReply{body: firing},
		alertReply{body: firing},
		alertReply{body: firing},
		alertReply{body: promBody(t)},
	)
	mt := &fakeMaintenance{windows: []store.MaintenanceWindow{windowAround("n1")}, err: errors.New("db down")}
	w, n, _ := newMaintenanceWatcherWith(t, src, mt, nil)
	w.poll(context.Background())
	w.poll(context.Background())

	w.now = func() time.Time { return testNow.Add(2 * time.Hour) }
	mt.err = nil
	w.poll(context.Background())
	if got := n.recorded(); len(got) != 0 {
		t.Fatalf("delivered %+v, want nothing for an alert the previous process already delivered", got)
	}
	w.poll(context.Background())
	if got := n.recorded(); len(got) != 1 || got[0].event != store.WebhookEventAlertResolved {
		t.Fatalf("delivered %+v, want one alert.resolved", got)
	}
}
