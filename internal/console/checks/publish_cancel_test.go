package checks_test

import (
	"context"
	"testing"
	"time"

	"github.com/EsDmitrii/kconmon-ng/internal/console/cache"
	"github.com/EsDmitrii/kconmon-ng/internal/console/checks"
	"github.com/EsDmitrii/kconmon-ng/internal/console/ws"
)

// ctxBus refuses a publish on a done context, as the Valkey bus does (rueidis checks ctx.Err()
// before it writes anything).
type ctxBus struct{ *recordingBus }

func (b ctxBus) Publish(ctx context.Context, topic string, msg cache.Message) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return b.recordingBus.Publish(ctx, topic, msg)
}

// The pairs in flight when a run is cancelled finish AFTER runCtx is done, and their terminal
// frames must still reach a bus that honours ctx; otherwise the live grid keeps them "dispatched".
func TestCancelledRunStillPublishesInFlightPairsTerminalFrames(t *testing.T) {
	ctrl := newGatedCtrl()
	bus := ctxBus{newRecordingBus()}
	hub := ws.NewHub(bus, testMetrics(t))
	mem := checks.NewMemoryStore()
	runner := checks.NewRunner(ctrl, hub, bus, mem, testMetrics(t))

	spec := checks.Spec{Sources: []string{"n1"}, Destinations: []string{"n2", "n3"}, Type: "tcp", Plane: "pod", Timeout: 5 * time.Second}
	id, err := runner.Start(context.Background(), spec, testInitiator())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	ctrl.awaitBlocked(t, 2)
	if cerr := runner.Cancel(context.Background(), id); cerr != nil {
		t.Fatalf("Cancel: %v", cerr)
	}
	if run := waitForTerminal(t, mem, id); run.Status != "cancelled" {
		t.Fatalf("run.Status = %q, want cancelled", run.Status)
	}

	terminal := map[string]string{}
	for _, m := range bus.onTopic(ws.RunTopic(id)) {
		fv := decodeFrame(t, m.msg)
		if fv.State != "" && fv.State != "dispatched" {
			terminal[fv.Destination] = fv.State
		}
	}
	for _, dst := range []string{"n2", "n3"} {
		if _, ok := terminal[dst]; !ok {
			t.Errorf("no terminal frame published for in-flight pair n1->%s after the cancel (got %v)", dst, terminal)
		}
	}
}
