package config

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// The chart writes its own checker timing into the ConfigMap, so a default changed only in
// defaults.go never reaches a chart install.
func TestChartCheckerTimingMatchesBinaryDefaults(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "charts", "kconmon-ng", "values.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var values struct {
		Config struct {
			Checkers map[string]struct {
				Interval string `yaml:"interval"`
				Timeout  string `yaml:"timeout"`
			} `yaml:"checkers"`
		} `yaml:"config"`
	}
	if err := yaml.Unmarshal(raw, &values); err != nil {
		t.Fatalf("values.yaml: %v", err)
	}

	d := DefaultConfig().Checkers
	want := map[string]struct{ interval, timeout time.Duration }{
		"tcp":  {d.TCP.Interval, d.TCP.Timeout},
		"udp":  {d.UDP.Interval, d.UDP.Timeout},
		"icmp": {d.ICMP.Interval, d.ICMP.Timeout},
		"pmtu": {d.PMTU.Interval, d.PMTU.Timeout},
		"dns":  {d.DNS.Interval, d.DNS.Timeout},
		"http": {d.HTTP.Interval, d.HTTP.Timeout},
	}
	for name, w := range want {
		c, ok := values.Config.Checkers[name]
		if !ok {
			t.Errorf("values.yaml has no config.checkers.%s", name)
			continue
		}
		for _, f := range []struct {
			key, got string
			want     time.Duration
		}{{"interval", c.Interval, w.interval}, {"timeout", c.Timeout, w.timeout}} {
			if got, err := time.ParseDuration(f.got); err != nil || got != f.want {
				t.Errorf("config.checkers.%s.%s is %q in values.yaml, the binary default is %s", name, f.key, f.got, f.want)
			}
		}
	}
}

func TestHelmRenderedDefaultsLoadWithoutWarnings(t *testing.T) {
	helm := helmBinary(t)
	out, err := exec.CommandContext(t.Context(), helm, "template", filepath.Join("..", "..", "charts", "kconmon-ng")).CombinedOutput()
	if err != nil {
		t.Fatalf("helm template failed: %v\n%s", err, out)
	}
	cfgYAML := extractConfigMapConfig(t, string(out))
	logs := captureLog(t)
	if err := NewLoader(writeConfig(t, cfgYAML)).Load(); err != nil {
		t.Fatal(err)
	}
	if got := logs.String(); strings.Contains(got, "level=WARN") {
		t.Fatalf("the chart's default config logs a warning about itself:\n%s", got)
	}
}
