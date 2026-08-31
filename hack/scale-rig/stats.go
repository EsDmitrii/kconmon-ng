package main

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"net/http"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// percentile returns the p-th percentile (nearest-rank) of the samples; it sorts a copy.
func percentile(samples []time.Duration, p float64) time.Duration {
	if len(samples) == 0 {
		return 0
	}
	sorted := make([]time.Duration, len(samples))
	copy(sorted, samples)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	rank := int(p/100*float64(len(sorted))+0.5) - 1
	if rank < 0 {
		rank = 0
	}
	if rank >= len(sorted) {
		rank = len(sorted) - 1
	}
	return sorted[rank]
}

// durSamples is a mutex-guarded duration collector (register latencies, delivery delays).
type durSamples struct {
	mu sync.Mutex
	ds []time.Duration
}

func (s *durSamples) add(d time.Duration) {
	s.mu.Lock()
	s.ds = append(s.ds, d)
	s.mu.Unlock()
}

func (s *durSamples) snapshot() []time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]time.Duration, len(s.ds))
	copy(out, s.ds)
	return out
}

// freePort asks the kernel for an unused localhost TCP port. There is a small window between
// closing and the controller re-binding it; acceptable for a measurement rig.
func freePort() (int, error) {
	var lc net.ListenConfig
	l, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	port := l.Addr().(*net.TCPAddr).Port
	return port, l.Close()
}

// controllerScrape is the controller's own view, read over its real /metrics listener as a
// cross-check for the rig's driver-side counters.
type controllerScrape struct {
	Registered       float64
	GRPCConnections  float64
	PeerUpdatesTotal float64
	OK               bool
}

func scrapeMetrics(ctx context.Context, url string) controllerScrape {
	var out controllerScrape
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return out
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return out
	}
	defer func() { _ = resp.Body.Close() }()

	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) != 2 {
			continue
		}
		v, perr := strconv.ParseFloat(fields[1], 64)
		if perr != nil {
			continue
		}
		switch fields[0] {
		case "kconmon_ng_controller_registered_agents":
			out.Registered = v
		case "kconmon_ng_controller_grpc_connections":
			out.GRPCConnections = v
		case "kconmon_ng_controller_peer_updates_total":
			out.PeerUpdatesTotal = v
		}
	}
	out.OK = sc.Err() == nil
	return out
}

// resSnap is one phase-boundary snapshot. CPU is read BEFORE the forced GC and memory AFTER it, so
// a phase's CPU delta (close(next) - open(this)) does not include the boundary GC, while the heap
// numbers describe live memory rather than garbage awaiting collection.
type resSnap struct {
	When       time.Time
	CPUClose   time.Duration // rusage user+sys read before the boundary GC
	CPUOpen    time.Duration // rusage user+sys read after it
	HeapAlloc  uint64
	HeapInuse  uint64
	Sys        uint64
	Goroutines int
	Flushes    uint64 // max over observers: broadcast flushes delivered to a stable subscriber
	Changes    uint64 // driver-side registry mutations
	Callbacks  uint64
	Resubs     uint64
	Rereg      uint64
	Scrape     controllerScrape
}

func takeSnap(ctx context.Context, metricsURL string, counters *rigCounters, flushes uint64) resSnap {
	s := resSnap{When: time.Now()}
	u, sys := processCPU()
	s.CPUClose = u + sys
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	s.HeapAlloc, s.HeapInuse, s.Sys = m.HeapAlloc, m.HeapInuse, m.Sys
	s.Goroutines = runtime.NumGoroutine()
	s.Flushes = flushes
	s.Changes = counters.changes()
	s.Callbacks = counters.peerCallbacks.Load()
	s.Resubs = counters.watchResubs.Load()
	s.Rereg = counters.reregEntries.Load()
	s.Scrape = scrapeMetrics(ctx, metricsURL)
	u, sys = processCPU()
	s.CPUOpen = u + sys
	return s
}

func mib(b uint64) string { return fmt.Sprintf("%.1f MiB", float64(b)/(1<<20)) }
