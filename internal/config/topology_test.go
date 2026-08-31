package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefaultConfigTopologyIsFullMesh(t *testing.T) {
	cfg := DefaultConfig()

	if cfg.Topology.Mode != TopologyModeFull {
		t.Errorf("default topology.mode = %q, want %q", cfg.Topology.Mode, TopologyModeFull)
	}
	if cfg.Topology.Sparse.RingDegree != 2 {
		t.Errorf("default topology.sparse.ringDegree = %d, want 2", cfg.Topology.Sparse.RingDegree)
	}
	if cfg.Topology.Sparse.ZoneChords != 2 {
		t.Errorf("default topology.sparse.zoneChords = %d, want 2", cfg.Topology.Sparse.ZoneChords)
	}
	if cfg.Topology.Sparse.AutoThreshold != 0 {
		t.Errorf("default topology.sparse.autoThreshold = %d, want 0", cfg.Topology.Sparse.AutoThreshold)
	}
}

func TestLoadTopologyFromFile(t *testing.T) {
	content := `
topology:
  mode: sparse
  sparse:
    ringDegree: 3
    zoneChords: 1
    autoThreshold: 20
`
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	loader := NewLoader(path)
	if err := loader.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	cfg := loader.Get()

	if cfg.Topology.Mode != TopologyModeSparse {
		t.Errorf("topology.mode = %q, want %q", cfg.Topology.Mode, TopologyModeSparse)
	}
	if cfg.Topology.Sparse.RingDegree != 3 {
		t.Errorf("topology.sparse.ringDegree = %d, want 3", cfg.Topology.Sparse.RingDegree)
	}
	if cfg.Topology.Sparse.ZoneChords != 1 {
		t.Errorf("topology.sparse.zoneChords = %d, want 1", cfg.Topology.Sparse.ZoneChords)
	}
	if cfg.Topology.Sparse.AutoThreshold != 20 {
		t.Errorf("topology.sparse.autoThreshold = %d, want 20", cfg.Topology.Sparse.AutoThreshold)
	}
}

// TestLoadTopologyPartialSparseKeepsDefaults pins that switching the mode alone keeps the
// documented sparse defaults (ringDegree 2, zoneChords 2), so `mode: sparse` is usable on its own.
func TestLoadTopologyPartialSparseKeepsDefaults(t *testing.T) {
	content := "topology:\n  mode: sparse\n"
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	loader := NewLoader(path)
	if err := loader.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	cfg := loader.Get()

	if cfg.Topology.Sparse.RingDegree != 2 || cfg.Topology.Sparse.ZoneChords != 2 {
		t.Errorf("sparse defaults not preserved: %+v", cfg.Topology.Sparse)
	}
}

func TestTopologyValidation(t *testing.T) {
	tests := []struct {
		name    string
		modify  func(*Config)
		wantErr bool
	}{
		{
			name:    "empty mode is accepted as full",
			modify:  func(c *Config) { c.Topology.Mode = "" },
			wantErr: false,
		},
		{
			name:    "mode full",
			modify:  func(c *Config) { c.Topology.Mode = "full" },
			wantErr: false,
		},
		{
			name:    "mode sparse with defaults",
			modify:  func(c *Config) { c.Topology.Mode = "sparse" },
			wantErr: false,
		},
		{
			name:    "mode is case-insensitive",
			modify:  func(c *Config) { c.Topology.Mode = "Sparse" },
			wantErr: false,
		},
		{
			name:    "unknown mode is refused",
			modify:  func(c *Config) { c.Topology.Mode = "mesh" },
			wantErr: true,
		},
		{
			name: "sparse ringDegree zero is refused (breaks connectivity)",
			modify: func(c *Config) {
				c.Topology.Mode = "sparse"
				c.Topology.Sparse.RingDegree = 0
			},
			wantErr: true,
		},
		{
			name: "sparse negative zoneChords is refused",
			modify: func(c *Config) {
				c.Topology.Mode = "sparse"
				c.Topology.Sparse.ZoneChords = -1
			},
			wantErr: true,
		},
		{
			name: "sparse negative autoThreshold is refused",
			modify: func(c *Config) {
				c.Topology.Mode = "sparse"
				c.Topology.Sparse.AutoThreshold = -5
			},
			wantErr: true,
		},
		{
			name: "full mode does not validate the unused sparse block",
			modify: func(c *Config) {
				c.Topology.Mode = "full"
				c.Topology.Sparse.RingDegree = 0
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := DefaultConfig()
			tt.modify(cfg)

			loader := &Loader{}
			err := loader.validate(cfg)
			if tt.wantErr && err == nil {
				t.Error("expected a validation error, got nil")
			}
			if !tt.wantErr && err != nil {
				t.Errorf("unexpected validation error: %v", err)
			}
		})
	}
}
