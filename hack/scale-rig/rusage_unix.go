//go:build unix

package main

import (
	"runtime"
	"syscall"
	"time"
)

// processCPU returns the process's cumulative user and system CPU time. The controller and the N
// thin agent clients share this process by design, so these numbers are an upper bound on the
// controller's own cost; the README explains the split.
func processCPU() (user, sys time.Duration) {
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
		return 0, 0
	}
	return time.Duration(ru.Utime.Nano()), time.Duration(ru.Stime.Nano())
}

// raiseNoFile lifts the soft open-files limit towards the hard one: N agents cost 2 FDs per
// connection (both ends live in this process). Best-effort; a refusal just surfaces later as dial
// errors the report counts.
func raiseNoFile() {
	var rl syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &rl); err != nil {
		return
	}
	const want = 65536
	if rl.Cur >= want {
		return
	}
	rl.Cur = want
	/* syscall.RLIM_INFINITY is an untyped -1: on linux/amd64 Rlimit fields are uint64 and the
	   direct comparison fails to COMPILE with an overflow error (darwin builds hid this).
	   All-ones is the same sentinel on every unix Go supports. */
	rlimInfinity := ^uint64(0)
	if rl.Max != rlimInfinity && rl.Max < want {
		rl.Cur = rl.Max
	}
	_ = syscall.Setrlimit(syscall.RLIMIT_NOFILE, &rl)
}

// maxRSSBytes returns the process's peak resident set size. Darwin reports bytes, Linux kibibytes.
func maxRSSBytes() uint64 {
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
		return 0
	}
	rss := uint64(ru.Maxrss) //nolint:gosec // G115: Maxrss is non-negative on both platforms
	if runtime.GOOS != "darwin" {
		rss *= 1024
	}
	return rss
}
