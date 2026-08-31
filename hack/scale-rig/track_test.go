package main

import (
	"math/rand/v2"
	"sync"
	"testing"
	"time"
)

func TestPropTrackerSingleProbeCompletesOnLastWatcher(t *testing.T) {
	tr := &propTracker{}
	start := time.Now()
	p := tr.addProbe(5, 3, start)

	cursors := make([]int, 3)

	// A list shorter than the threshold covers nothing.
	tr.cover(&cursors[0], 4, start.Add(time.Millisecond))
	if p.remaining.Load() != 3 {
		t.Fatalf("short list must not cover: remaining=%d", p.remaining.Load())
	}
	if cursors[0] != 0 {
		t.Fatalf("cursor must not advance on a short list: %d", cursors[0])
	}

	tr.cover(&cursors[0], 5, start.Add(2*time.Millisecond))
	tr.cover(&cursors[1], 5, start.Add(3*time.Millisecond))
	if p.doneNanos.Load() != 0 {
		t.Fatalf("probe done before every watcher covered it")
	}
	doneAt := start.Add(7 * time.Millisecond)
	tr.cover(&cursors[2], 9, doneAt)
	if p.doneNanos.Load() != doneAt.UnixNano() {
		t.Fatalf("doneNanos=%d, want %d", p.doneNanos.Load(), doneAt.UnixNano())
	}
	for i, c := range cursors {
		if c != 1 {
			t.Fatalf("cursor[%d]=%d, want 1", i, c)
		}
	}
}

func TestPropTrackerCursorPreventsDoubleCounting(t *testing.T) {
	tr := &propTracker{}
	start := time.Now()
	p := tr.addProbe(5, 2, start)

	cursor := 0
	tr.cover(&cursor, 5, start)
	tr.cover(&cursor, 6, start) // same watcher again: already past the probe
	if got := p.remaining.Load(); got != 1 {
		t.Fatalf("one watcher covered twice: remaining=%d, want 1", got)
	}
}

func TestPropTrackerMonotoneThresholdsAdvanceTogether(t *testing.T) {
	tr := &propTracker{}
	start := time.Now()
	p1 := tr.addProbe(5, 1, start)
	p2 := tr.addProbe(6, 1, start)

	cursor := 0
	tr.cover(&cursor, 6, start.Add(time.Millisecond))
	if p1.doneNanos.Load() == 0 || p2.doneNanos.Load() == 0 {
		t.Fatalf("a list at the larger threshold must cover both probes")
	}
	if cursor != 2 {
		t.Fatalf("cursor=%d, want 2", cursor)
	}
}

func TestPropTrackerResults(t *testing.T) {
	tr := &propTracker{}
	start := time.Now()
	p := tr.addProbe(3, 1, start)
	tr.addProbe(4, 1, start) // never covered

	cursor := 0
	tr.cover(&cursor, 3, start.Add(40*time.Millisecond))
	if p.doneNanos.Load() == 0 {
		t.Fatalf("first probe not done")
	}

	done, incomplete := tr.results()
	if len(done) != 1 || incomplete != 1 {
		t.Fatalf("results: done=%d incomplete=%d, want 1/1", len(done), incomplete)
	}
	if done[0] != 40*time.Millisecond {
		t.Fatalf("duration=%v, want 40ms", done[0])
	}
}

// Property: whatever order concurrent watchers observe the growing list in, every probe ends with
// remaining exactly 0 and a done timestamp — never negative, never double-counted.
func TestPropTrackerConcurrentProperty(t *testing.T) {
	const watchers, probes = 50, 20
	tr := &propTracker{}
	start := time.Now()
	for i := range probes {
		tr.addProbe(i+1, watchers, start)
	}

	var wg sync.WaitGroup
	for range watchers {
		wg.Go(func() {
			cursor := 0
			seen := 0
			for seen < probes {
				// Jump forward a random amount, as coalesced FULL_SYNCs do.
				seen += 1 + rand.IntN(4)
				if seen > probes {
					seen = probes
				}
				tr.cover(&cursor, seen, time.Now())
			}
		})
	}
	wg.Wait()

	done, incomplete := tr.results()
	if len(done) != probes || incomplete != 0 {
		t.Fatalf("done=%d incomplete=%d, want %d/0", len(done), incomplete, probes)
	}
	tr.mu.RLock()
	defer tr.mu.RUnlock()
	for i, p := range tr.probes {
		if got := p.remaining.Load(); got != 0 {
			t.Fatalf("probe %d remaining=%d, want 0", i, got)
		}
	}
}
