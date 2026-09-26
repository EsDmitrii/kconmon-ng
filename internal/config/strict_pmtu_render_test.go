package config

import (
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// Every pmtu key the chart can write must decode into the agent's own settings: the chart renders
// them conditionally, so the default render above never exercises them.
func TestHelmRenderedTunedPMTULoads(t *testing.T) {
	helm := helmBinary(t)
	chartPath := filepath.Join("..", "..", "charts", "kconmon-ng")
	out, err := exec.CommandContext(t.Context(), helm, "template", chartPath,
		"--set", "config.checkers.pmtu.enabled=false",
		"--set", "config.checkers.pmtu.interval=30s",
		"--set", "config.checkers.pmtu.timeout=200ms",
		"--set", "config.checkers.pmtu.size=1400",
	).CombinedOutput()
	if err != nil {
		t.Fatalf("helm template failed: %v\n%s", err, out)
	}
	cfgYAML := extractConfigMapConfig(t, string(out))
	loader := NewLoader(writeConfig(t, cfgYAML))
	if err := loader.Load(); err != nil {
		t.Fatalf("tuned pmtu render failed strict validation: %v\nconfig:\n%s", err, cfgYAML)
	}
	want := PMTUCheckerConfig{Enabled: false, Interval: 30 * time.Second, Timeout: 200 * time.Millisecond, Size: 1400}
	if got := loader.Get().Checkers.PMTU; got != want {
		t.Fatalf("pmtu = %+v, want %+v", got, want)
	}
}
