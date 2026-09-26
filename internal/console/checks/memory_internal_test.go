package checks

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/EsDmitrii/kconmon-ng/internal/console/store"
)

// Without a database a long, wide interval run (400 pairs x 500 samples) must not hold every result:
// the store keeps what a read can return, the newest store.RunResultsCap, and still says it truncated.
func TestMemoryStoreRetainsAtMostTheReadCapPerRun(t *testing.T) {
	m := NewMemoryStore()
	ctx := context.Background()
	if _, err := m.CreateRun(ctx, "r1", "udp", "pod", json.RawMessage(`{}`), "user", "u1", 1, time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	const extra = 500
	total := store.RunResultsCap + extra
	for i := range total {
		in := store.RunResultInput{RunID: "r1", SourceNode: "a", DestinationNode: "b", SampleSeq: int32(i), Success: true}
		if _, err := m.UpsertRunResult(ctx, in); err != nil {
			t.Fatalf("UpsertRunResult %d: %v", i, err)
		}
	}

	m.mu.Lock()
	held := len(m.runs["r1"].results)
	m.mu.Unlock()
	if held > store.RunResultsCap {
		t.Errorf("memory store holds %d results for one run, want at most %d", held, store.RunResultsCap)
	}

	rows, truncated, err := m.GetRunResults(ctx, "r1")
	if err != nil {
		t.Fatalf("GetRunResults: %v", err)
	}
	if !truncated || len(rows) != store.RunResultsCap {
		t.Fatalf("GetRunResults = %d rows, truncated=%v; want %d and true", len(rows), truncated, store.RunResultsCap)
	}
	if rows[0].SampleSeq != extra || rows[len(rows)-1].SampleSeq != int32(total-1) {
		t.Errorf("rows span seq %d..%d, want the newest %d..%d",
			rows[0].SampleSeq, rows[len(rows)-1].SampleSeq, extra, total-1)
	}

	// A retried sample that is still retained is overwritten in place, not appended.
	retry := store.RunResultInput{RunID: "r1", SourceNode: "a", DestinationNode: "b", SampleSeq: int32(total - 1), Error: "retried"}
	if _, err := m.UpsertRunResult(ctx, retry); err != nil {
		t.Fatalf("UpsertRunResult retry: %v", err)
	}
	rows, _, _ = m.GetRunResults(ctx, "r1")
	if len(rows) != store.RunResultsCap || rows[len(rows)-1].Error != "retried" || rows[len(rows)-1].SampleSeq != int32(total-1) {
		t.Errorf("retry: %d rows, last = %+v; want the last row overwritten", len(rows), rows[len(rows)-1])
	}
}
