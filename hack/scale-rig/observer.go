package main

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	pb "github.com/EsDmitrii/kconmon-ng/api/proto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/proto"
)

/*
observer is a raw WatchPeers subscriber outside the fleet. It exists because the agent client
deliberately narrows updates to []checker.Target, dropping exactly what the rig must measure: the
server's flush timestamp and the wire message itself. One observer stream costs the controller one
extra watcher and gives the rig, from real received messages:

  - the broadcast flush count (post-initial receives on a stable stream — the denominator of the
    coalescing ratio),
  - real FULL_SYNC sizes per peer count (proto.Size of the received message = its marshaled size),
  - flush-to-delivery latency for this subscriber (server timestamp vs receive time; one clock,
    same process).
*/
type observer struct {
	id   string
	addr string

	initials atomic.Uint64
	flushes  atomic.Uint64
	resubs   atomic.Uint64

	mu          sync.Mutex
	sizeByPeers map[int]int
	maxPeers    int
	delays      []time.Duration
}

func newObserver(idx int, addr string) *observer {
	return &observer{
		id:          fmt.Sprintf("rig-observer-%d", idx),
		addr:        addr,
		sizeByPeers: make(map[int]int),
	}
}

func (o *observer) run(ctx context.Context) {
	conn, err := grpc.NewClient(o.addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		// Mirror the agent's recv guard so a huge fleet's FULL_SYNC cannot wedge the observer first.
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(16*1024*1024)),
	)
	if err != nil {
		return
	}
	defer func() { _ = conn.Close() }()
	client := pb.NewAgentRegistryClient(conn)

	for ctx.Err() == nil {
		stream, serr := client.WatchPeers(ctx, &pb.WatchPeersRequest{AgentId: o.id})
		if serr != nil {
			select {
			case <-ctx.Done():
				return
			case <-time.After(200 * time.Millisecond):
			}
			continue
		}
		first := true
		for {
			update, rerr := stream.Recv()
			if rerr != nil {
				break
			}
			o.record(update, first)
			first = false
		}
		if ctx.Err() == nil {
			o.resubs.Add(1)
		}
	}
}

func (o *observer) record(update *pb.PeerUpdate, first bool) {
	now := time.Now()
	size := proto.Size(update)
	peers := len(update.GetPeers())

	o.mu.Lock()
	o.sizeByPeers[peers] = size
	if peers > o.maxPeers {
		o.maxPeers = peers
	}
	if !first && update.GetTimestamp() != nil && len(o.delays) < 8192 {
		o.delays = append(o.delays, now.Sub(update.GetTimestamp().AsTime()))
	}
	o.mu.Unlock()

	if first {
		o.initials.Add(1)
	} else {
		o.flushes.Add(1)
	}
}

// sizeAt returns the last observed FULL_SYNC size for exactly `peers` entries (0 if never seen).
func (o *observer) sizeAt(peers int) int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.sizeByPeers[peers]
}

func (o *observer) delaySamples() []time.Duration {
	o.mu.Lock()
	defer o.mu.Unlock()
	out := make([]time.Duration, len(o.delays))
	copy(out, o.delays)
	return out
}
