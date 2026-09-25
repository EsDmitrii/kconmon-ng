// Command pmtu-netns runs one side of the pmtu real-kernel check: an echo responder or a single
// probe that prints its verdict as JSON. run.sh wires the two across network namespaces.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/EsDmitrii/kconmon-ng/internal/agent"
	"github.com/EsDmitrii/kconmon-ng/internal/checker"
)

func main() {
	mode := flag.String("mode", "", "echo | probe")
	addr := flag.String("addr", "", "peer address (probe mode)")
	port := flag.Int("port", 19090, "echo port")
	size := flag.Int("size", 0, "probe size, 0 = interface MTU")
	flag.Parse()

	switch *mode {
	case "echo":
		if err := runEcho(*port); err != nil {
			fmt.Fprintln(os.Stderr, "echo:", err)
			os.Exit(1)
		}
	case "probe":
		c := checker.NewPMTUChecker(300*time.Millisecond, time.Minute, *size, *port)
		res := c.Check(context.Background(), checker.Target{NodeName: "pb", PodIP: *addr})
		out, _ := json.Marshal(map[string]any{"success": res.Success, "error": res.Error, "details": res.Details})
		fmt.Println(string(out))
	default:
		fmt.Fprintln(os.Stderr, "usage: pmtu-netns -mode echo|probe [-addr IP] [-port N] [-size N]")
		os.Exit(2)
	}
}

// runEcho serves the agent's own echo responder until SIGINT or SIGTERM.
func runEcho(port int) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	srv := agent.NewProbeServer(port)
	if err := srv.ListenUDP(ctx); err != nil {
		return err
	}
	<-ctx.Done()
	return srv.Close()
}
