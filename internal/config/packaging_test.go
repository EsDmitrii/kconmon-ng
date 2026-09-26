package config

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
)

// commentedKey matches an example key the packaged config ships commented out ("# httpPort: 8080",
// "#   pmtu:"), and not the prose comments around it, which never start with a lowerCamel key.
var commentedKey = regexp.MustCompile(`(?m)^([ \t]*)# ([ \t]*[a-z][A-Za-z]*:(?: .*)?)$`)

// The deb/rpm example config is the first file a bare-host operator edits: as shipped and with every
// example key uncommented it must pass the same strict decoding and validation the agent applies.
func TestPackagingAgentConfigLoads(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "packaging", "agent", "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}

	t.Run("as shipped", func(t *testing.T) {
		if err := NewLoader(writeConfig(t, string(raw))).Load(); err != nil {
			t.Fatalf("packaging/agent/config.yaml does not load: %v", err)
		}
	})

	t.Run("every example key uncommented", func(t *testing.T) {
		uncommented := commentedKey.ReplaceAllString(string(raw), "$1$2")
		if uncommented == string(raw) {
			t.Fatal("found no commented example keys to uncomment")
		}
		loader := NewLoader(writeConfig(t, uncommented))
		if err := loader.Load(); err != nil {
			t.Fatalf("packaging/agent/config.yaml with its examples uncommented does not load: %v\n%s", err, uncommented)
		}
		cfg := loader.Get()
		want := PMTUCheckerConfig{Enabled: true, Interval: time.Minute, Timeout: 500 * time.Millisecond, Size: 0}
		if cfg.Checkers.PMTU != want {
			t.Errorf("pmtu = %+v, want %+v", cfg.Checkers.PMTU, want)
		}
		if cfg.Agent.NodeName != "edge-host-01" || cfg.Agent.TLS.CertFile == "" || cfg.MetricsPort != 9091 {
			t.Errorf("uncommented examples did not all apply: nodeName=%q certFile=%q metricsPort=%d",
				cfg.Agent.NodeName, cfg.Agent.TLS.CertFile, cfg.MetricsPort)
		}
	})
}

// scriptRootPath matches an absolute path the package scripts touch, so a test can move it under a
// scratch root.
var scriptRootPath = regexp.MustCompile(`(^|[\s'"])/(etc|run|usr|lib)/`)

// runPostinstall runs packaging/scripts/postinstall.sh against root with every command it calls
// stubbed, and returns the sysctl invocations it made, root stripped.
func runPostinstall(t *testing.T, root string, systemdSysctl bool) []string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "packaging", "scripts", "postinstall.sh"))
	if err != nil {
		t.Fatal(err)
	}
	vendor, err := os.ReadFile(filepath.Join("..", "..", "packaging", "agent", "sysctl.d", "50-kconmon-ng.conf"))
	if err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{"etc/sysctl.d", "run/sysctl.d", "usr/lib/sysctl.d", "usr/lib/systemd", "bin"} {
		if err = os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err = os.WriteFile(filepath.Join(root, "usr/lib/sysctl.d/50-kconmon-ng.conf"), vendor, 0o644); err != nil {
		t.Fatal(err)
	}
	calls := filepath.Join(root, "calls")
	stub := func(path, name string) {
		body := "#!/bin/sh\necho \"" + name + " $*\" >> \"$CALLS\"\n"
		if werr := os.WriteFile(path, []byte(body), 0o755); werr != nil { //nolint:gosec // an executable stub
			t.Fatal(werr)
		}
	}
	for _, name := range []string{"getent", "groupadd", "useradd", "systemctl", "sysctl"} {
		stub(filepath.Join(root, "bin", name), name)
	}
	if systemdSysctl {
		stub(filepath.Join(root, "usr/lib/systemd/systemd-sysctl"), "systemd-sysctl")
	}
	grep := "/usr/bin/grep"
	if _, err = os.Stat(grep); err != nil {
		if grep, err = exec.LookPath("grep"); err != nil {
			t.Skip("no grep to run the package script with")
		}
	}
	if err = os.Symlink(grep, filepath.Join(root, "bin", "grep")); err != nil {
		t.Fatal(err)
	}

	script := filepath.Join(root, "postinstall.sh")
	rooted := scriptRootPath.ReplaceAllString(string(raw), "${1}"+root+"/${2}/")
	if err = os.WriteFile(script, []byte(rooted), 0o755); err != nil { //nolint:gosec // the script under test
		t.Fatal(err)
	}
	cmd := exec.CommandContext(context.Background(), "/bin/sh", script, "configure") //nolint:gosec // test-built path
	cmd.Env = []string{"PATH=" + filepath.Join(root, "bin"), "CALLS=" + calls}
	if out, runErr := cmd.CombinedOutput(); runErr != nil {
		t.Fatalf("postinstall.sh failed: %v\n%s", runErr, out)
	}
	recorded, err := os.ReadFile(calls)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	var sysctl []string
	for line := range strings.SplitSeq(strings.TrimSpace(string(recorded)), "\n") {
		if strings.HasPrefix(line, "sysctl ") || strings.HasPrefix(line, "systemd-sysctl ") {
			sysctl = append(sysctl, strings.ReplaceAll(line, root, ""))
		}
	}
	return sysctl
}

/*
postinstall applies the vendor ping_group_range file at once instead of at the next boot. It must
leave the range alone wherever boot would: the admin sets the key (dot or slash form), or shadows the
vendor file by name, a /dev/null mask included. systemd-sysctl resolves the name itself, so it is
preferred over `sysctl -p FILE`, which reads the one file it is given.
*/
func TestPostinstallAppliesPingRangeOnlyWhereBootWould(t *testing.T) {
	const systemdApply = "systemd-sysctl 50-kconmon-ng.conf"
	const plainApply = "sysctl -p /usr/lib/sysctl.d/50-kconmon-ng.conf"
	for _, tc := range []struct {
		name          string
		systemdSysctl bool
		files         map[string]string // path under root -> content; "->" prefix makes a symlink
		want          []string
	}{
		{name: "fresh systemd host", systemdSysctl: true, want: []string{systemdApply}},
		{name: "fresh host without systemd", want: []string{plainApply}},
		{name: "admin key, dot form", systemdSysctl: true,
			files: map[string]string{"etc/sysctl.d/60-ping.conf": "net.ipv4.ping_group_range = 1 0\n"}},
		{name: "admin key, slash form", systemdSysctl: true,
			files: map[string]string{"etc/sysctl.d/60-ping.conf": "net/ipv4/ping_group_range = 1 0\n"}},
		{name: "admin key in sysctl.conf, ignore-failure prefix",
			files: map[string]string{"etc/sysctl.conf": "  -net.ipv4.ping_group_range=1 0\n"}},
		{name: "admin key in /run", systemdSysctl: true,
			files: map[string]string{"run/sysctl.d/10-ping.conf": "net.ipv4.ping_group_range = 1 0\n"}},
		{name: "commented key does not count",
			files: map[string]string{"etc/sysctl.d/60-ping.conf": "# net.ipv4.ping_group_range = 1 0\n"},
			want:  []string{plainApply}},
		{name: "vendor file masked, systemd resolves the mask", systemdSysctl: true,
			files: map[string]string{"etc/sysctl.d/50-kconmon-ng.conf": "->/dev/null"},
			want:  []string{systemdApply}},
		{name: "vendor file masked, no systemd",
			files: map[string]string{"etc/sysctl.d/50-kconmon-ng.conf": "->/dev/null"}},
		{name: "vendor file overridden by name in /run, no systemd",
			files: map[string]string{"run/sysctl.d/50-kconmon-ng.conf": "kernel.domainname = example\n"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			for path, content := range tc.files {
				full := filepath.Join(root, path)
				if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
					t.Fatal(err)
				}
				var err error
				if target, ok := strings.CutPrefix(content, "->"); ok {
					err = os.Symlink(target, full)
				} else {
					err = os.WriteFile(full, []byte(content), 0o644)
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			if got := runPostinstall(t, root, tc.systemdSysctl); !slices.Equal(got, tc.want) {
				t.Fatalf("sysctl calls = %q, want %q", got, tc.want)
			}
		})
	}
}
