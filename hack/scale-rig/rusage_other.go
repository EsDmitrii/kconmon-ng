//go:build !unix

package main

import "time"

// Non-unix builds get no rusage; the rig still compiles so `go build ./...` stays green everywhere.
func processCPU() (user, sys time.Duration) { return 0, 0 }

func maxRSSBytes() uint64 { return 0 }

func raiseNoFile() {}
