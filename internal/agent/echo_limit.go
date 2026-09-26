package agent

import (
	"net/netip"
	"time"
)

const (
	// echoBurst covers two full udp probes (100 packets each) and a pmtu search (22 datagrams) from
	// one source port at once, in case the kernel hands a closed probe's port straight out again.
	echoBurst = 256
	// echoRate refills a drained source; a real probe socket never sends again once its probe ends.
	echoRate = 64
	// echoMaxSources bounds the map; a fleet probing on a short interval keeps far fewer sources
	// busy within echoIdleFull.
	echoMaxSources = 4096
	// echoIdleFull is how long a drained bucket takes to fill up again: an older entry changes nothing.
	echoIdleFull = echoBurst * time.Second / echoRate
)

/*
echoLimiter caps echo replies per source ip:port with a token bucket. A probe sends at most a few
hundred datagrams from one ephemeral socket and closes it, while a reflection loop between two echo
responders bounces one datagram between the same two ports at wire speed; draining the bucket drops
one reply and the loop ends. Keyed by ip:port, not ip, so peers behind one NAT do not share a
bucket. Owned by the serve goroutine, so it takes no lock.
*/
type echoLimiter struct {
	buckets map[netip.AddrPort]echoBucket
	now     func() time.Time
}

type echoBucket struct {
	tokens float64
	last   time.Time
}

func newEchoLimiter(now func() time.Time) *echoLimiter {
	return &echoLimiter{buckets: make(map[netip.AddrPort]echoBucket), now: now}
}

// allow spends one token of src's bucket and reports whether there was one.
func (l *echoLimiter) allow(src netip.AddrPort) bool {
	now := l.now()
	b, ok := l.buckets[src]
	if ok {
		b.tokens = min(echoBurst, b.tokens+max(0, now.Sub(b.last).Seconds())*echoRate)
	} else {
		if len(l.buckets) >= echoMaxSources {
			l.evict(now)
		}
		b.tokens = echoBurst
	}
	b.last = now
	allowed := b.tokens >= 1
	if allowed {
		b.tokens--
	}
	l.buckets[src] = b
	return allowed
}

// evict frees an eighth of the map at least, so a flood of new sources pays for one sweep per
// hundreds of datagrams. Idle buckets go first; if every source is busy, arbitrary ones follow.
func (l *echoLimiter) evict(now time.Time) {
	const keep = echoMaxSources * 7 / 8
	for src, b := range l.buckets {
		if now.Sub(b.last) >= echoIdleFull {
			delete(l.buckets, src)
		}
	}
	for src := range l.buckets {
		if len(l.buckets) < keep {
			break
		}
		delete(l.buckets, src)
	}
}
