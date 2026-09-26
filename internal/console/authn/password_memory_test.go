package authn_test

import (
	"encoding/base64"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/argon2"

	"github.com/EsDmitrii/kconmon-ng/internal/console/authn"
)

// consoleMemoryLimitMiB is the chart's default console memory limit (charts/kconmon-ng/values.yaml
// console.resources.limits.memory).
const consoleMemoryLimitMiB = 256

// legacyPHC is a hash at the parameters releases before 2.5.0 wrote (64 MiB, t=3, p=2).
func legacyPHC(plain string) string {
	salt := []byte("0123456789ABCDEF")
	key := argon2.IDKey([]byte(plain), salt, 3, 64*1024, 2, 32)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=3,p=2$%s$%s", argon2.Version, 64*1024,
		base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(key))
}

// TestArgonWorkFitsTheConsoleMemoryLimit floods HashPassword and VerifyPassword concurrently, old
// and new hashes mixed, and samples the heap: whatever arrives at once, argon2 must leave the
// console's default limit most of its room.
func TestArgonWorkFitsTheConsoleMemoryLimit(t *testing.T) {
	phc, err := authn.HashPassword("memory ceiling check")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	legacy := legacyPHC("memory ceiling check")
	if ok, verr := authn.VerifyPassword(legacy, "memory ceiling check"); verr != nil || !ok {
		t.Fatalf("a pre-2.5.0 hash must keep verifying: ok=%v err=%v", ok, verr)
	}
	runtime.GC()
	var base runtime.MemStats
	runtime.ReadMemStats(&base)

	var peak atomic.Uint64
	stop := make(chan struct{})
	sampled := make(chan struct{})
	go func() {
		defer close(sampled)
		var ms runtime.MemStats
		for {
			runtime.ReadMemStats(&ms)
			if ms.HeapInuse > peak.Load() {
				peak.Store(ms.HeapInuse)
			}
			select {
			case <-stop:
				return
			case <-time.After(2 * time.Millisecond):
			}
		}
	}()

	var wg sync.WaitGroup
	for i := range 9 {
		wg.Go(func() {
			switch i % 3 {
			case 0:
				_, _ = authn.HashPassword("memory ceiling check")
			case 1:
				_, _ = authn.VerifyPassword(phc, "wrong guess")
			default:
				_, _ = authn.VerifyPassword(legacy, "wrong guess")
			}
		})
	}
	wg.Wait()
	close(stop)
	<-sampled

	grown := (peak.Load() - min(peak.Load(), base.HeapInuse)) >> 20
	t.Logf("peak heap growth: %d MiB", grown)
	if limit := uint64(consoleMemoryLimitMiB / 2); grown > limit {
		t.Fatalf("9 concurrent argon2 calls grew the heap by %d MiB, want at most %d MiB (half the chart's %d MiB console limit)",
			grown, limit, consoleMemoryLimitMiB)
	}
}
