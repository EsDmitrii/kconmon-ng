package config

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func portConfig(port int) []byte {
	return []byte(fmt.Sprintf("httpPort: %d\ngrpcPort: 9090\n", port))
}

func watchedLoader(t *testing.T, path string) (*Loader, <-chan *Config) {
	t.Helper()
	loader := NewLoader(path)
	if err := loader.Load(); err != nil {
		t.Fatal(err)
	}
	changed := make(chan *Config, 16)
	loader.OnChange(func(cfg *Config) { changed <- cfg })
	if err := loader.WatchForChanges(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = loader.Close() })
	return loader, changed
}

// expectReload waits for exactly one reload to port: the debounce must fold one rewrite's burst of
// events (rename, create, chmod, write) into a single OnChange call.
func expectReload(t *testing.T, changed <-chan *Config, port int) {
	t.Helper()
	select {
	case cfg := <-changed:
		if cfg.HTTPPort != port {
			t.Fatalf("reloaded httpPort = %d, want %d", cfg.HTTPPort, port)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("no reload to httpPort %d within 3s", port)
	}
	select {
	case cfg := <-changed:
		t.Fatalf("a second reload (httpPort %d) for one rewrite", cfg.HTTPPort)
	case <-time.After(3 * configReloadDebounce):
	}
}

func expectNoReload(t *testing.T, changed <-chan *Config) {
	t.Helper()
	select {
	case cfg := <-changed:
		t.Fatalf("unexpected reload (httpPort %d)", cfg.HTTPPort)
	case <-time.After(4 * configReloadDebounce):
	}
}

func replaceByRename(t *testing.T, path string, content []byte) {
	t.Helper()
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, content, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
}

// ansible, puppet's file resource and `mv tmp config.yaml` replace the file by rename. The second
// rename is the regression: a watch on the file itself follows the old inode and never fires again.
func TestHotReloadSurvivesAtomicRename(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, portConfig(8080), 0o600); err != nil {
		t.Fatal(err)
	}
	_, changed := watchedLoader(t, path)

	replaceByRename(t, path, portConfig(7001))
	expectReload(t, changed, 7001)
	replaceByRename(t, path, portConfig(7002))
	expectReload(t, changed, 7002)
}

// vim with backupcopy=no: move the original aside, write a new file, delete the backup.
func TestHotReloadSurvivesVimStyleRewrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, portConfig(8080), 0o600); err != nil {
		t.Fatal(err)
	}
	_, changed := watchedLoader(t, path)

	for _, port := range []int{7101, 7102} {
		if err := os.Rename(path, path+"~"); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, portConfig(port), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(path + "~"); err != nil {
			t.Fatal(err)
		}
		expectReload(t, changed, port)
	}
}

// A ConfigMap volume: config.yaml -> ..data/config.yaml, and an update swaps the ..data symlink
// atomically. Nothing happens to config.yaml itself, only a directory watch sees the swap.
func TestHotReloadSurvivesConfigMapSymlinkSwap(t *testing.T) {
	if runtime.GOOS == "darwin" {
		t.Skip("kqueue does not report a rename onto an existing name; ConfigMap volumes exist only on Linux")
	}
	dir := t.TempDir()
	writeVersion := func(name string, port int) {
		t.Helper()
		if err := os.MkdirAll(filepath.Join(dir, name), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name, "config.yaml"), portConfig(port), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	swapData := func(target string) {
		t.Helper()
		tmp := filepath.Join(dir, "..data_tmp")
		if err := os.Symlink(target, tmp); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(tmp, filepath.Join(dir, "..data")); err != nil {
			t.Fatal(err)
		}
	}
	writeVersion("..v1", 8080)
	if err := os.Symlink("..v1", filepath.Join(dir, "..data")); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.yaml")
	if err := os.Symlink(filepath.Join("..data", "config.yaml"), path); err != nil {
		t.Fatal(err)
	}
	_, changed := watchedLoader(t, path)

	writeVersion("..v2", 7201)
	swapData("..v2")
	expectReload(t, changed, 7201)
	writeVersion("..v3", 7202)
	swapData("..v3")
	expectReload(t, changed, 7202)
}

// chmod and a rewrite with the same bytes change nothing: no OnChange hook may run.
func TestHotReloadIgnoresNoOpEvents(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, portConfig(8080), 0o600); err != nil {
		t.Fatal(err)
	}
	_, changed := watchedLoader(t, path)

	if err := os.Chmod(path, 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, portConfig(8080), 0o600); err != nil {
		t.Fatal(err)
	}
	expectNoReload(t, changed)
}

// A deleted file keeps the config already applied; a new file with new content is picked up.
func TestHotReloadKeepsConfigWhenFileRemoved(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, portConfig(8080), 0o600); err != nil {
		t.Fatal(err)
	}
	loader, changed := watchedLoader(t, path)

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	expectNoReload(t, changed)
	if got := loader.Get().HTTPPort; got != 8080 {
		t.Fatalf("httpPort = %d after the file vanished, want the applied 8080", got)
	}

	if err := os.WriteFile(path, portConfig(7301), 0o600); err != nil {
		t.Fatal(err)
	}
	expectReload(t, changed, 7301)
}
