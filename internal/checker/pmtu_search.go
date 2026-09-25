package checker

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/EsDmitrii/kconmon-ng/internal/model"
)

const (
	// pmtuBaseSize is the reachability datagram: small enough to cross any path that carries
	// traffic at all, so losing it is a connectivity question for the udp plane, not an MTU verdict.
	pmtuBaseSize = 64
	// pmtuMaxSteps bounds the bisection: ceil(log2(65535-64)) halvings close any interval to one byte.
	pmtuMaxSteps = 16
	// pmtuAttempts is how often the base and the full-size datagram are tried before they count as
	// lost. The verdict rests on the full size failing twice, so one random drop on a healthy path
	// never becomes a black hole. Bisection steps go once: a random drop there can only make the
	// reported MTU smaller, never turn a healthy path into a failing one.
	pmtuAttempts = 2
	// pmtuConfirmSends re-checks a black hole before it is reported: the full size once more, which
	// must vanish again, and the size that crossed once more, which must cross again. Without it a
	// lossy path turns into a black hole whenever both full-size sends happen to drop.
	pmtuConfirmSends = 2
)

// errPMTULost means neither the datagram's echo nor an error came back within the timeout.
var errPMTULost = errors.New("pmtu: datagram lost")

// pmtuTooBigError means the size was refused with a reason: an ICMP frag-needed from a router on the
// path, or the local route. mtu is what the kernel learned, 0 where the OS cannot say.
type pmtuTooBigError struct{ mtu int }

func (e *pmtuTooBigError) Error() string {
	return fmt.Sprintf("pmtu: path refused the datagram, reported MTU %d", e.mtu)
}

// pmtuPath is the seam between the search and the socket.
type pmtuPath interface {
	// Send puts one DF-marked datagram of exactly size IP-level bytes on the wire and waits up to
	// timeout for its echo: nil when the echo came back, errPMTULost when nothing did, a
	// *pmtuTooBigError when the size was refused with a reason, anything else for a broken socket.
	Send(size int, timeout time.Duration) error
}

// pmtuSearch finds the largest datagram a path carries. It is pure over pmtuPath, so every verdict is
// unit-tested without a network that misbehaves on cue.
type pmtuSearch struct {
	ctx      context.Context
	path     pmtuPath
	probeMTU int
	timeout  time.Duration
	deadline time.Time
	now      func() time.Time
}

func (s *pmtuSearch) send(size, attempts int, d *model.PMTUDetails) error {
	var err error
	for range attempts {
		if cerr := s.ctx.Err(); cerr != nil {
			return cerr
		}
		d.Steps++
		err = s.path.Send(size, s.timeout)
		if !errors.Is(err, errPMTULost) {
			return err
		}
	}
	return err
}

func (s *pmtuSearch) run() model.PMTUDetails {
	d := model.PMTUDetails{ProbeMTU: s.probeMTU}

	if err := s.send(pmtuBaseSize, pmtuAttempts, &d); err != nil {
		d.Verdict = model.PMTUVerdictUnreachable
		return d
	}

	err := s.send(s.probeMTU, pmtuAttempts, &d)
	if err == nil {
		d.Verdict, d.PathMTU = model.PMTUVerdictOK, s.probeMTU
		return d
	}
	tooBig, sawTooBig := errors.AsType[*pmtuTooBigError](err)
	if sawTooBig && tooBig.mtu > pmtuBaseSize && tooBig.mtu < s.probeMTU {
		// A router said how big: PMTUD works on this path and the number is authoritative.
		d.Verdict, d.PathMTU = model.PMTUVerdictReduced, tooBig.mtu
		return d
	}
	if !sawTooBig && !errors.Is(err, errPMTULost) {
		// A cancelled context or a dead socket is not an MTU verdict.
		d.Verdict = model.PMTUVerdictUnreachable
		return d
	}

	lo, hi := pmtuBaseSize, s.probeMTU // lo crossed, hi did not
	reserve := time.Duration(pmtuConfirmSends) * s.timeout
	bisected := false
	for step := 0; step < pmtuMaxSteps && hi-lo > 1; step++ {
		if s.now().Add(reserve).After(s.deadline) {
			d.Truncated = true
			break
		}
		bisected = true
		mid := lo + (hi-lo)/2
		stepErr := s.send(mid, 1, &d)
		switch tb, refused := errors.AsType[*pmtuTooBigError](stepErr); {
		case stepErr == nil:
			lo = mid
		case refused:
			sawTooBig = true
			if tb.mtu > pmtuBaseSize && tb.mtu < mid {
				d.Verdict, d.PathMTU = model.PMTUVerdictReduced, tb.mtu
				return d
			}
			hi = mid
		case errors.Is(stepErr, errPMTULost):
			hi = mid
		default:
			d.Verdict = model.PMTUVerdictUnreachable
			return d
		}
	}

	d.PathMTU = lo
	d.Verdict = model.PMTUVerdictBlackhole
	if sawTooBig {
		// Refusals came back without a number (Darwin cannot read the learned MTU): the path still
		// signals that it is smaller, so it is reduced, not silent.
		d.Verdict = model.PMTUVerdictReduced
		return d
	}
	if bisected && lo == pmtuBaseSize {
		// Every size the bisection tried was lost, down to a few bytes: a lossy path, not a small one.
		d.Verdict, d.PathMTU = model.PMTUVerdictUnreachable, 0
		return d
	}
	return s.confirmBlackhole(d)
}

// confirmBlackhole sends the full size and the size that crossed once more each. Without the budget
// for both, the verdict stands unconfirmed.
func (s *pmtuSearch) confirmBlackhole(d model.PMTUDetails) model.PMTUDetails {
	if s.now().Add(time.Duration(pmtuConfirmSends) * s.timeout).After(s.deadline) {
		return d
	}
	err := s.send(s.probeMTU, 1, &d)
	if err == nil {
		d.Verdict, d.PathMTU = model.PMTUVerdictOK, s.probeMTU
		return d
	}
	if tb, refused := errors.AsType[*pmtuTooBigError](err); refused {
		d.Verdict = model.PMTUVerdictReduced
		if tb.mtu > pmtuBaseSize && tb.mtu < s.probeMTU {
			d.PathMTU = tb.mtu
		}
		return d
	}
	if !errors.Is(err, errPMTULost) {
		d.Verdict, d.PathMTU = model.PMTUVerdictUnreachable, 0
		return d
	}
	if err := s.send(d.PathMTU, 1, &d); err != nil {
		d.Verdict, d.PathMTU = model.PMTUVerdictUnreachable, 0
	}
	return d
}
