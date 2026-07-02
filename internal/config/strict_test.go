package config

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestStrictDecodeUnknownKeyFails(t *testing.T) {
	content := `
httpPort: 8080
grpcPort: 9090
logLevle: debug
`
	loader := NewLoader(writeConfig(t, content))
	err := loader.Load()
	if err == nil {
		t.Fatal("expected error for unknown key logLevle, got nil")
	}
	if !strings.Contains(err.Error(), "logLevle") {
		t.Errorf("error should name the offending key, got: %v", err)
	}
}

func TestStrictDecodeUnknownNestedKeyFails(t *testing.T) {
	content := `
checkers:
  tcp:
    enabled: true
    timout: 1s
`
	loader := NewLoader(writeConfig(t, content))
	if err := loader.Load(); err == nil {
		t.Fatal("expected error for unknown nested key timout, got nil")
	}
}

func TestStrictDecodeEmptyFileIsDefaults(t *testing.T) {
	loader := NewLoader(writeConfig(t, ""))
	if err := loader.Load(); err != nil {
		t.Fatalf("empty config file should load as defaults, got: %v", err)
	}
	cfg := loader.Get()
	if cfg.HTTPPort != 8080 {
		t.Errorf("expected default httpPort 8080, got %d", cfg.HTTPPort)
	}
}

func TestStrictDecodeKnownConfigLoads(t *testing.T) {
	content := `
httpPort: 8080
grpcPort: 9090
logLevel: info
logFormat: json
checkers:
  tcp:
    enabled: true
    interval: 5s
    timeout: 1s
`
	loader := NewLoader(writeConfig(t, content))
	if err := loader.Load(); err != nil {
		t.Fatalf("valid config with known keys should load, got: %v", err)
	}
}

// TestHelmRenderedConfigIsValid renders the chart, extracts the ConfigMap's
// config.yaml, and feeds it through the loader under strict decoding. This
// guards against the chart emitting a key that strict decode would reject,
// which would break deployment. Skips gracefully when helm is not installed.
func TestHelmRenderedConfigIsValid(t *testing.T) {
	helm, err := exec.LookPath("helm")
	if err != nil {
		t.Skip("helm not found in PATH, skipping chart render validation")
	}

	// Repo root is two levels up from internal/config.
	chartPath := filepath.Join("..", "..", "charts", "kconmon-ng")
	if _, statErr := os.Stat(chartPath); statErr != nil {
		t.Skipf("chart path %s not found: %v", chartPath, statErr)
	}

	out, err := exec.CommandContext(t.Context(), helm, "template", chartPath).CombinedOutput()
	if err != nil {
		t.Fatalf("helm template failed: %v\n%s", err, out)
	}

	cfgYAML := extractConfigMapConfig(t, string(out))
	loader := NewLoader(writeConfig(t, cfgYAML))
	if err := loader.Load(); err != nil {
		t.Fatalf("rendered chart config failed strict validation: %v\nconfig:\n%s", err, cfgYAML)
	}
}

// TestValuesLocalConfigIsValid renders the chart with hack/values-local.yaml
// and validates the resulting app config under strict decoding.
func TestValuesLocalConfigIsValid(t *testing.T) {
	helm, err := exec.LookPath("helm")
	if err != nil {
		t.Skip("helm not found in PATH, skipping values-local render validation")
	}

	chartPath := filepath.Join("..", "..", "charts", "kconmon-ng")
	valuesPath := filepath.Join("..", "..", "hack", "values-local.yaml")
	if _, statErr := os.Stat(valuesPath); statErr != nil {
		t.Skipf("values file %s not found: %v", valuesPath, statErr)
	}

	out, err := exec.CommandContext(t.Context(), helm, "template", chartPath, "-f", valuesPath).CombinedOutput()
	if err != nil {
		t.Fatalf("helm template with values-local failed: %v\n%s", err, out)
	}

	cfgYAML := extractConfigMapConfig(t, string(out))
	loader := NewLoader(writeConfig(t, cfgYAML))
	if err := loader.Load(); err != nil {
		t.Fatalf("values-local rendered config failed strict validation: %v\nconfig:\n%s", err, cfgYAML)
	}
}

// extractConfigMapConfig pulls the "config.yaml: |" block out of the rendered
// helm manifest and returns its dedented content.
func extractConfigMapConfig(t *testing.T, manifest string) string {
	t.Helper()
	lines := strings.Split(manifest, "\n")
	blockRe := regexp.MustCompile(`^(\s*)config\.yaml:\s*\|`)

	for i, line := range lines {
		m := blockRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		indent := len(m[1]) + 2 // block content is indented two more than the key
		var body []string
		for _, bl := range lines[i+1:] {
			if strings.TrimSpace(bl) == "" {
				body = append(body, "")
				continue
			}
			leading := len(bl) - len(strings.TrimLeft(bl, " "))
			if leading < indent {
				break
			}
			body = append(body, bl[indent:])
		}
		return strings.Join(body, "\n")
	}

	t.Fatalf("could not find config.yaml block in rendered manifest")
	return ""
}
