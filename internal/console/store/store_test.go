package store

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TestOpenEmptyDSNReturnsError asserts the guard rail.
func TestOpenEmptyDSNReturnsError(t *testing.T) {
	_, err := Open(context.Background(), "", 10, time.Second, false)
	if err == nil {
		t.Fatal("Open with empty dsn: want error, got nil")
	}
}

// TestOpenUnroutableHostRespectsConnectTimeout asserts Open returns in roughly connectTimeout
// rather than hanging.
func TestOpenUnroutableHostRespectsConnectTimeout(t *testing.T) {
	const connectTimeout = 500 * time.Millisecond
	// 192.0.2.1 is TEST-NET-1 (RFC 5737), reserved for documentation and never routed anywhere: no
	// host there ever responds.
	const dsn = "postgres://user:pass@192.0.2.1:5432/db?sslmode=disable"

	start := time.Now()
	_, err := Open(context.Background(), dsn, 5, connectTimeout, false)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("Open against unroutable host: want error, got nil")
	}
	// Generous slack over connectTimeout: this only needs to prove Open does
	// not hang indefinitely, not pin down exact scheduling behavior.
	const slack = 5 * time.Second
	if elapsed > connectTimeout+slack {
		t.Fatalf("Open blocked for %s, want roughly connectTimeout (%s)", elapsed, connectTimeout)
	}
}

// TestEmbeddedMigrationsWellFormed is a cheap regression test that catches a
// malformed migration (missing an Up or Down marker) before it reaches a
// cluster, and confirms 00001_topology_events.sql is actually embedded.
func TestEmbeddedMigrationsWellFormed(t *testing.T) {
	entries, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		t.Fatalf("read embedded migrations dir: %v", err)
	}

	var found00001, found00002, found00003 bool
	var sqlFiles int
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		sqlFiles++
		switch e.Name() {
		case "00001_topology_events.sql":
			found00001 = true
		case "00002_auth.sql":
			found00002 = true
		case "00003_checks.sql":
			found00003 = true
		}

		data, err := migrationsFS.ReadFile("migrations/" + e.Name())
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		content := string(data)
		if !strings.Contains(content, "-- +goose Up") {
			t.Errorf("%s: missing '-- +goose Up' marker", e.Name())
		}
		if !strings.Contains(content, "-- +goose Down") {
			t.Errorf("%s: missing '-- +goose Down' marker", e.Name())
		}
	}

	if sqlFiles == 0 {
		t.Fatal("no .sql files found in embedded migrations FS")
	}
	if !found00001 {
		t.Error("embedded migrations FS does not contain 00001_topology_events.sql")
	}
	if !found00002 {
		t.Error("embedded migrations FS does not contain 00002_auth.sql")
	}
	if !found00003 {
		t.Error("embedded migrations FS does not contain 00003_checks.sql")
	}
}
