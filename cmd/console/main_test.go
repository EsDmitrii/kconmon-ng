package main

import "testing"

/*
 * Shutdown finishes the RUNS before it tears down the pipeline they publish onto.
 *
 * This was the other way round once, and the hub was gone by the time the runs finished: every
 * cancelled run's execute() waited on a relay counter for a topic closeAllClients had already
 * reaped, so TopicSeq answered 0, the target was unreachable by construction, and each run spun the
 * full relay timeout before logging a WARN blaming the bus for dropping frames it had in fact
 * delivered. Every rolling update produced that false alarm and burned a fifth of the finish budget
 * busy-waiting. The order is the whole fix, so the order is what this pins.
 */
func TestDrainConsoleFinishesRunsBeforeStoppingTheRealtimePipeline(t *testing.T) {
	var order []string
	drainConsole(
		func() { order = append(order, "runs") },
		func() { order = append(order, "realtime") },
	)

	if len(order) != 2 {
		t.Fatalf("drainConsole ran %v, want both halves exactly once", order)
	}
	if order[0] != "runs" || order[1] != "realtime" {
		t.Errorf("drainConsole ran %v, want runs before realtime: a run draining after the hub is "+
			"gone waits out its relay timeout and blames the bus for frames it delivered", order)
	}
}
