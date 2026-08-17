package checks

import (
	"errors"
	"testing"
	"time"
)

/*
interval_internal_test.go covers the OPERATOR-REQUESTED sample interval.

Until it existed, EffectiveSampleInterval's own doc said the base cadence "is not something an
operator can dial", and three surfaces each reported a different number for the same run: the MTR
Runner's caption said 5s (the base cadence, unstretched), the run permalink's tile said 3m (the
planner's worst-case floor), and the run itself produced a probe about once a minute. These tests
pin the one rule the whole product now speaks: what was ASKED for, what will RUN, and why the two
differ when they do.
*/

// TestRequestedSampleIntervalIsHonoured is the plain case: a fast check type, a cadence well inside
// both the cap and the round floor, and the plan is the number that was typed.
func TestRequestedSampleIntervalIsHonoured(t *testing.T) {
	spec := &Spec{Type: "tcp", Duration: 5 * time.Minute, RequestedSampleInterval: time.Second}
	cad := PlanCadence(spec, allToAllPairs(4), clampTimeoutFor("tcp", 5*time.Second))

	if cad.Interval != time.Second {
		t.Errorf("interval = %s, want 1s", cad.Interval)
	}
	if cad.Base != time.Second {
		t.Errorf("base = %s, want 1s -- the dispatch loop is paced by the request too", cad.Base)
	}
	if cad.SamplesPerPair != 300 {
		t.Errorf("samplesPerPair = %d, want 300", cad.SamplesPerPair)
	}
	if cad.Adjusted != "" {
		t.Errorf("adjusted = %q, want empty: nothing was adjusted", cad.Adjusted)
	}
	if cad.Requested != time.Second {
		t.Errorf("requested = %s, want 1s reported back verbatim", cad.Requested)
	}
}

// TestRequestedSampleIntervalBeatsTheDerivedFloor pins that an explicit request goes BELOW
// MinSampleInterval. That 5s floor exists so a derived cadence cannot measure the console's own
// dispatch loop; an operator who typed 1s has asked for exactly that and gets it, bounded by the
// 500-sample cap rather than by a default nobody chose.
func TestRequestedSampleIntervalBeatsTheDerivedFloor(t *testing.T) {
	// 5m/500 is 600ms, so the cap leaves 1s alone and MinSampleInterval is the only thing that could
	// have moved it.
	spec := &Spec{Type: "tcp", Duration: 5 * time.Minute, RequestedSampleInterval: time.Second}
	if got := BaseSampleInterval(spec); got != time.Second {
		t.Errorf("BaseSampleInterval = %s, want 1s (MinSampleInterval must not floor a request)", got)
	}
	if MinSampleInterval <= time.Second {
		t.Fatal("this test is vacuous unless MinSampleInterval is above the 1s it must not apply to")
	}
	// And the derived path is untouched by the same call.
	if got := BaseSampleInterval(&Spec{Type: "tcp", Duration: 10 * time.Minute}); got != MinSampleInterval {
		t.Errorf("derived BaseSampleInterval = %s, want %s", got, MinSampleInterval)
	}
}

// TestRequestedSampleIntervalCapBinds is task 5's "report when it binds rather than truncating
// silently": 1s over 24h is 86 400 samples for one pair, the cap widens it to duration/500, and the
// plan SAYS SO instead of quietly handing back a different number.
func TestRequestedSampleIntervalCapBinds(t *testing.T) {
	spec := &Spec{Type: "tcp", Duration: 24 * time.Hour, RequestedSampleInterval: time.Second}
	cad := PlanCadence(spec, allToAllPairs(4), clampTimeoutFor("tcp", 5*time.Second))

	want := 24 * time.Hour / MaxSamplesPerPair
	if cad.Interval != want {
		t.Errorf("interval = %s, want %s (duration/%d)", cad.Interval, want, MaxSamplesPerPair)
	}
	if cad.SamplesPerPair != MaxSamplesPerPair {
		t.Errorf("samplesPerPair = %d, want the cap %d", cad.SamplesPerPair, MaxSamplesPerPair)
	}
	if cad.Adjusted != IntervalCapped {
		t.Errorf("adjusted = %q, want %q", cad.Adjusted, IntervalCapped)
	}
	if cad.Requested != time.Second {
		t.Errorf("requested = %s, want the 1s that was asked for, still reported", cad.Requested)
	}
}

// TestRequestedSampleIntervalBelowRoundFloorIsAdjustedNotRefused is the chosen below-floor
// semantics, in one test: 1s for an MTR run over ten pairs is faster than one round of traces can
// possibly finish, and the run is ACCEPTED with the two numbers reported apart -- requested 1s,
// effective one round's floor -- rather than refused with a 422.
func TestRequestedSampleIntervalBelowRoundFloorIsAdjustedNotRefused(t *testing.T) {
	pairs := starPairs(10, 5)
	spec := &Spec{Type: "mtr", Duration: 5 * time.Minute, RequestedSampleInterval: time.Second}
	cad := PlanCadence(spec, pairs, clampTimeoutFor("mtr", 0))

	if cad.Adjusted != IntervalStretched {
		t.Fatalf("adjusted = %q, want %q", cad.Adjusted, IntervalStretched)
	}
	if cad.Requested != time.Second {
		t.Errorf("requested = %s, want 1s", cad.Requested)
	}
	if cad.Interval <= time.Second {
		t.Errorf("interval = %s, want a stretched cadence above the 1s request", cad.Interval)
	}
	// The floor is one round of the busiest agent's share, exactly as the unrequested path computes it.
	if want := roundFloor(pairs, mtrMinPerPairTimeout); cad.Interval != want {
		t.Errorf("interval = %s, want one round's floor %s", cad.Interval, want)
	}
	if cad.SamplesPerPair < 1 {
		t.Errorf("samplesPerPair = %d, want at least one honest pass", cad.SamplesPerPair)
	}
}

// TestAbsentSampleIntervalIsUnchanged is the byte-identity guard: with no request, every number the
// planner produces is the one it produced before this field existed, and the three snapshot fields
// that describe a request stay zero so `omitempty` drops them from the stored spec.
func TestAbsentSampleIntervalIsUnchanged(t *testing.T) {
	for _, checkType := range []string{"tcp", "udp", "icmp", "dns", "http", "mtr"} {
		for _, d := range []time.Duration{0, 10 * time.Second, time.Minute, 15 * time.Minute, 24 * time.Hour} {
			spec := &Spec{Type: checkType, Duration: d}
			pairs := allToAllPairs(4)
			timeout := clampTimeoutFor(checkType, 0)
			cad := PlanCadence(spec, pairs, timeout)

			if want := EffectiveSampleInterval(spec, pairs, timeout); cad.Interval != want {
				t.Errorf("%s/%s: interval = %s, want EffectiveSampleInterval's %s", checkType, d, cad.Interval, want)
			}
			if want := PlannedSamplesPerPair(d, cad.Interval); cad.SamplesPerPair != want {
				t.Errorf("%s/%s: samplesPerPair = %d, want %d", checkType, d, cad.SamplesPerPair, want)
			}
			if want := SampleInterval(d); cad.Base != want {
				t.Errorf("%s/%s: base = %s, want the derived %s", checkType, d, cad.Base, want)
			}
			if cad.Requested != 0 {
				t.Errorf("%s/%s: requested = %s, want zero", checkType, d, cad.Requested)
			}
		}
	}
}

// TestValidateSampleInterval pins the one line this package draws: a value OUT OF RANGE is refused,
// a value that is merely infeasible is adjusted. The range mirrors ValidateDuration's contract --
// a well-formed field carrying a value policy refuses, never silently clamped.
func TestValidateSampleInterval(t *testing.T) {
	for _, tc := range []struct {
		name      string
		requested time.Duration
		duration  time.Duration
		wantErr   bool
	}{
		{"absent is an absent request", 0, 5 * time.Minute, false},
		{"absent on an instant run", 0, 0, false},
		{"the documented floor", MinRequestedSampleInterval, 5 * time.Minute, false},
		{"equal to the duration is one sample, not an error", 5 * time.Minute, 5 * time.Minute, false},
		{"below the floor", 500 * time.Millisecond, 5 * time.Minute, true},
		{"negative", -time.Second, 5 * time.Minute, true},
		{"longer than the run", 15 * time.Minute, 5 * time.Minute, true},
		{"an instant run has no cadence to dial", time.Second, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateSampleInterval(tc.requested, tc.duration)
			if tc.wantErr {
				if !errors.Is(err, ErrSampleIntervalOutOfRange) {
					t.Fatalf("err = %v, want ErrSampleIntervalOutOfRange", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("err = %v, want nil", err)
			}
		})
	}
}

// starPairs builds n pairs spread over `sources` agents -- the shape roundFloor's per-source gate
// actually cares about, which allToAllPairs cannot express.
func starPairs(n, sources int) []Pair {
	pairs := make([]Pair, 0, n)
	for i := range n {
		pairs = append(pairs, Pair{
			Source:      string(rune('a' + i%sources)),
			Destination: NodeDestination("dst-" + string(rune('a'+i))),
		})
	}
	return pairs
}
