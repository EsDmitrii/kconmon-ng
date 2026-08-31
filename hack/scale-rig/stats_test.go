package main

import (
	"testing"
	"time"
)

func TestPercentile(t *testing.T) {
	if got := percentile(nil, 95); got != 0 {
		t.Fatalf("empty: %v, want 0", got)
	}
	one := []time.Duration{7 * time.Millisecond}
	if got := percentile(one, 50); got != 7*time.Millisecond {
		t.Fatalf("single p50: %v", got)
	}
	if got := percentile(one, 100); got != 7*time.Millisecond {
		t.Fatalf("single p100: %v", got)
	}

	ds := make([]time.Duration, 100)
	for i := range ds {
		ds[i] = time.Duration(i+1) * time.Millisecond
	}
	if got := percentile(ds, 50); got != 50*time.Millisecond {
		t.Fatalf("p50=%v, want 50ms", got)
	}
	if got := percentile(ds, 95); got != 95*time.Millisecond {
		t.Fatalf("p95=%v, want 95ms", got)
	}
	if got := percentile(ds, 100); got != 100*time.Millisecond {
		t.Fatalf("p100=%v, want 100ms", got)
	}
}

func TestPercentileSortsInput(t *testing.T) {
	ds := []time.Duration{30, 10, 20}
	if got := percentile(ds, 100); got != 30 {
		t.Fatalf("max=%v, want 30", got)
	}
	if got := percentile(ds, 50); got != 20 {
		t.Fatalf("p50=%v, want 20", got)
	}
}
