package controllerclient_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/EsDmitrii/kconmon-ng/internal/console/controllerclient"
)

func topoJSON() string {
	return `{"nodes":[{"name":"node-1","zone":"us-east-1a","ready":true},
	{"name":"node-2","zone":"us-east-1b","ready":false}],
	"agents":[{"id":"node-1-agent-x","nodeName":"node-1","podIP":"10.0.0.1","zone":"us-east-1a"}],
	"timestamp":"2026-01-01T00:00:00Z"}`
}

func TestTopology(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/topology" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(topoJSON()))
	}))
	defer srv.Close()

	c := controllerclient.New(srv.URL, 5*time.Second)
	topo, err := c.Topology(context.Background())
	if err != nil {
		t.Fatalf("Topology: %v", err)
	}
	if len(topo.Nodes) != 2 || len(topo.Agents) != 1 {
		t.Fatalf("unexpected topology: %+v", topo)
	}
	if topo.Nodes[0].Name != "node-1" || !topo.Nodes[0].Ready || topo.Agents[0].PodIP != "10.0.0.1" {
		t.Errorf("field mapping wrong: %+v", topo)
	}
}

func TestTopologyRetriesNonLeader503(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) < 3 {
			http.Error(w, "not leader", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte(topoJSON()))
	}))
	defer srv.Close()

	c := controllerclient.New(srv.URL, 5*time.Second)
	if _, err := c.Topology(context.Background()); err != nil {
		t.Fatalf("expected success after retries, got %v", err)
	}
	if calls.Load() != 3 {
		t.Errorf("expected 3 calls, got %d", calls.Load())
	}
}

func TestTopologyExhaustsRetries(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "not leader", http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	c := controllerclient.New(srv.URL, 5*time.Second)
	_, err := c.Topology(context.Background())
	if !errors.Is(err, controllerclient.ErrUnavailable) {
		t.Fatalf("expected ErrUnavailable, got %v", err)
	}
}

func TestVersion(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/version" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"version": "1.4.0", "commit": "abc"})
	}))
	defer srv.Close()

	v, err := controllerclient.New(srv.URL, 5*time.Second).Version(context.Background())
	if err != nil || v.Version != "1.4.0" || v.Commit != "abc" {
		t.Fatalf("Version: %+v, %v", v, err)
	}
}

func TestVersionCapabilities(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"version": "1.6.0", "commit": "abc", "capabilities": []string{"events"},
		})
	}))
	defer srv.Close()

	v, err := controllerclient.New(srv.URL, 5*time.Second).Version(context.Background())
	if err != nil {
		t.Fatalf("Version: %v", err)
	}
	if !v.HasCapability("events") {
		t.Errorf("expected HasCapability(events) true, got capabilities=%v", v.Capabilities)
	}
	if v.HasCapability("nonexistent") {
		t.Error("expected HasCapability(nonexistent) false")
	}
}

func TestVersionNoCapabilitiesField(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"version": "1.5.0", "commit": "abc"})
	}))
	defer srv.Close()

	v, err := controllerclient.New(srv.URL, 5*time.Second).Version(context.Background())
	if err != nil {
		t.Fatalf("Version: %v", err)
	}
	if v.HasCapability("events") {
		t.Error("expected HasCapability(events) false when the controller predates capability flags")
	}
}
