package config

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
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

// expectReload waits for exactly one reload to port and returns it: the debounce must fold one
// rewrite's burst of events (rename, create, chmod, write) into a single OnChange call.
func expectReload(t *testing.T, changed <-chan *Config, port int) *Config {
	t.Helper()
	var reloaded *Config
	select {
	case reloaded = <-changed:
		if reloaded.HTTPPort != port {
			t.Fatalf("reloaded httpPort = %d, want %d", reloaded.HTTPPort, port)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("no reload to httpPort %d within 3s", port)
	}
	select {
	case cfg := <-changed:
		t.Fatalf("a second reload (httpPort %d) for one rewrite", cfg.HTTPPort)
	case <-time.After(3 * configReloadDebounce):
	}
	return reloaded
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

// ansible, puppet's file resource and `mv tmp config.yaml` replace the file by rename. A rename-replace
// must keep reloading on the second rewrite too: a file watch would follow the old inode.
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

// os.WriteFile and `cat > config.yaml` rewrite the file in place: truncate, then write. The reload
// carries the whole file, not just the key the helper checks.
func TestHotReloadInPlaceWrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, portConfig(8080), 0o600); err != nil {
		t.Fatal(err)
	}
	_, changed := watchedLoader(t, path)

	for _, port := range []int{7601, 7602} {
		if err := os.WriteFile(path, append(portConfig(port), "logLevel: debug\n"...), 0o600); err != nil {
			t.Fatal(err)
		}
		if cfg := expectReload(t, changed, port); cfg.LogLevel != "debug" {
			t.Fatalf("reloaded logLevel = %q, want debug", cfg.LogLevel)
		}
	}
}

// An in-place writer that stalls between the truncate and the write leaves an empty file behind for
// longer than the debounce. That is a torn read, not an operator resetting everything to defaults.
func TestHotReloadIgnoresTruncatedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, portConfig(7401), 0o600); err != nil {
		t.Fatal(err)
	}
	loader, changed := watchedLoader(t, path)

	if err := os.Truncate(path, 0); err != nil {
		t.Fatal(err)
	}
	expectNoReload(t, changed)
	if got := loader.Get().HTTPPort; got != 7401 {
		t.Fatalf("httpPort = %d after the file was truncated, want the applied 7401", got)
	}

	if err := os.WriteFile(path, portConfig(7402), 0o600); err != nil {
		t.Fatal(err)
	}
	expectReload(t, changed, 7402)
}

func symlinkedConfig(t *testing.T, port int) (path, target string) {
	t.Helper()
	target = filepath.Join(t.TempDir(), "kconmon.yaml")
	if err := os.WriteFile(target, portConfig(port), 0o600); err != nil {
		t.Fatal(err)
	}
	path = filepath.Join(t.TempDir(), "config.yaml")
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	return path, target
}

// config.yaml as a symlink to a file in another directory: inotify on the config directory never
// sees an in-place edit of the target, so the target's directory is watched too.
func TestHotReloadFollowsSymlinkToAnotherDirectory(t *testing.T) {
	path, target := symlinkedConfig(t, 8080)
	_, changed := watchedLoader(t, path)

	for _, port := range []int{7701, 7702} {
		if err := os.WriteFile(target, portConfig(port), 0o600); err != nil {
			t.Fatal(err)
		}
		expectReload(t, changed, port)
	}
}

// config.yaml as a symlink to config-prod.yaml next to it, the usual way to switch configs on a bare
// host. inotify names the events config-prod.yaml, and they must count as ours.
func TestHotReloadFollowsSymlinkInTheSameDirectory(t *testing.T) {
	for _, tc := range []struct {
		name     string
		absolute bool
	}{{"relative link", false}, {"absolute link", true}} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			target := filepath.Join(dir, "config-prod.yaml")
			if err := os.WriteFile(target, portConfig(8080), 0o600); err != nil {
				t.Fatal(err)
			}
			link := "config-prod.yaml"
			if tc.absolute {
				link = target
			}
			path := filepath.Join(dir, "config.yaml")
			if err := os.Symlink(link, path); err != nil {
				t.Fatal(err)
			}
			_, changed := watchedLoader(t, path)

			if err := os.WriteFile(target, portConfig(7711), 0o600); err != nil {
				t.Fatal(err)
			}
			expectReload(t, changed, 7711)
			replaceByRename(t, target, portConfig(7712))
			expectReload(t, changed, 7712)
			if err := os.WriteFile(target, portConfig(7713), 0o600); err != nil {
				t.Fatal(err)
			}
			expectReload(t, changed, 7713)
		})
	}
}

// Pointing the symlink at a file in yet another directory moves the extra watch along with it.
func TestHotReloadFollowsRetargetedSymlink(t *testing.T) {
	if runtime.GOOS == "darwin" {
		t.Skip("fsnotify's kqueue backend does not report a symlink replaced faster than it re-lists " +
			"the directory; bare-host agents run on Linux")
	}
	path, _ := symlinkedConfig(t, 8080)
	_, changed := watchedLoader(t, path)

	secondTarget := filepath.Join(t.TempDir(), "kconmon.yaml")
	if err := os.WriteFile(secondTarget, portConfig(7702), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secondTarget, path+".tmp"); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path+".tmp", path); err != nil {
		t.Fatal(err)
	}
	expectReload(t, changed, 7702)

	if err := os.WriteFile(secondTarget, portConfig(7703), 0o600); err != nil {
		t.Fatal(err)
	}
	expectReload(t, changed, 7703)
}

// Subscribers are registered from other goroutines while the watcher may be reloading; run with
// -race.
func TestOnChangeRegistrationDuringReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, portConfig(8080), 0o600); err != nil {
		t.Fatal(err)
	}
	loader, changed := watchedLoader(t, path)

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Go(func() {
		for {
			select {
			case <-stop:
				return
			default:
				loader.OnChange(func(*Config) {})
				time.Sleep(time.Millisecond)
			}
		}
	})
	for _, port := range []int{7501, 7502} {
		if err := os.WriteFile(path, portConfig(port), 0o600); err != nil {
			t.Fatal(err)
		}
		expectReload(t, changed, port)
	}
	close(stop)
	wg.Wait()
}

// Close must not return while a reload is still running its OnChange callbacks, or a caller that
// tears down what the callback touches races it.
func TestCloseWaitsForInFlightReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, portConfig(8080), 0o600); err != nil {
		t.Fatal(err)
	}
	loader := NewLoader(path)
	if err := loader.Load(); err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	releaseOnce := sync.OnceFunc(func() { close(release) })
	t.Cleanup(releaseOnce)
	loader.OnChange(func(*Config) {
		entered <- struct{}{}
		<-release
	})
	if err := loader.WatchForChanges(); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(path, portConfig(7801), 0o600); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("no reload within 3s")
	}

	closed := make(chan struct{})
	go func() {
		_ = loader.Close()
		close(closed)
	}()
	select {
	case <-closed:
		t.Fatal("Close returned while an OnChange callback was still running")
	case <-time.After(4 * configReloadDebounce):
	}
	releaseOnce()
	select {
	case <-closed:
	case <-time.After(3 * time.Second):
		t.Fatal("Close did not return after the callback finished")
	}
}

// A second WatchForChanges must not start a second watcher that Close then leaves running.
func TestWatchForChangesTwiceThenClose(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, portConfig(8080), 0o600); err != nil {
		t.Fatal(err)
	}
	loader, changed := watchedLoader(t, path)
	if err := loader.WatchForChanges(); err != nil {
		t.Fatal(err)
	}
	if err := loader.Close(); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(path, portConfig(7901), 0o600); err != nil {
		t.Fatal(err)
	}
	expectNoReload(t, changed)
}

// A ConfigMap volume: config.yaml -> ..data/config.yaml, and an update swaps the ..data symlink
// atomically. Nothing happens to config.yaml itself, only a directory watch sees the swap.
func TestHotReloadSurvivesConfigMapSymlinkSwap(t *testing.T) {
	if runtime.GOOS == "darwin" {
		t.Skip("fsnotify's kqueue backend does not reliably report the ..data symlink swap " +
			"(the tmp symlink is renamed away before it is watched); ConfigMap volumes exist only on Linux")
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
