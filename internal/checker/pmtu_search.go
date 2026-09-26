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

// PMTUMinInterval is the shortest checkers.pmtu.interval whose budget (half the interval) fits a
// typical black-hole search at this per-datagram timeout: base and full size lost twice each, half
// the bisection steps lost, and the confirmation. Below it a search is cut short or gives no verdict.
func PMTUMinInterval(timeout time.Duration) time.Duration {
	return 2 * time.Duration(2*pmtuAttempts+pmtuMaxSteps/2+pmtuConfirmSends) * timeout
}

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
	// reason says why the verdict is unreachable; Check puts it in the result's error.
	reason string
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

// usableMTU reports whether a router's number lies strictly between the base datagram and the probe.
// It is compared with the probe, not with the size just sent: a frag-needed for an earlier, larger
// send can land on a later one, and its number is still the router's.
func (s *pmtuSearch) usableMTU(mtu int) bool {
	return mtu > pmtuBaseSize && mtu < s.probeMTU
}

// noVerdict ends the search without an MTU verdict and records why.
func (s *pmtuSearch) noVerdict(d model.PMTUDetails, reason string) model.PMTUDetails {
	d.Verdict, d.PathMTU = model.PMTUVerdictUnreachable, 0
	s.reason = reason
	return d
}

func stoppedReason(err error) string {
	return fmt.Sprintf("the probe stopped before a verdict: %v", err)
}

func (s *pmtuSearch) run() model.PMTUDetails {
	d := model.PMTUDetails{ProbeMTU: s.probeMTU}

	if err := s.send(pmtuBaseSize, pmtuAttempts, &d); err != nil {
		if !errors.Is(err, errPMTULost) {
			return s.noVerdict(d, stoppedReason(err))
		}
		return s.noVerdict(d, fmt.Sprintf(
			"the peer's echo did not answer a %d-byte datagram; reachability is the udp plane's verdict", pmtuBaseSize))
	}

	err := s.send(s.probeMTU, pmtuAttempts, &d)
	if err == nil {
		d.Verdict, d.PathMTU = model.PMTUVerdictOK, s.probeMTU
		return d
	}
	tooBig, sawTooBig := errors.AsType[*pmtuTooBigError](err)
	if sawTooBig && s.usableMTU(tooBig.mtu) {
		// A router said how big: PMTUD works on this path and the number is authoritative.
		d.Verdict, d.PathMTU = model.PMTUVerdictReduced, tooBig.mtu
		return d
	}
	if !sawTooBig && !errors.Is(err, errPMTULost) {
		// A cancelled context or a dead socket is not an MTU verdict.
		return s.noVerdict(d, stoppedReason(err))
	}

	lo, hi := pmtuBaseSize, s.probeMTU // lo crossed, hi did not
	// A step goes out only while it and the confirmation still fit: a truncated search is confirmed too.
	reserve := time.Duration(1+pmtuConfirmSends) * s.timeout
	for step := 0; step < pmtuMaxSteps && hi-lo > 1; step++ {
		if s.now().Add(reserve).After(s.deadline) {
			d.Truncated = true
			break
		}
		mid := lo + (hi-lo)/2
		stepErr := s.send(mid, 1, &d)
		switch tb, refused := errors.AsType[*pmtuTooBigError](stepErr); {
		case stepErr == nil:
			lo = mid
		case refused:
			sawTooBig = true
			if s.usableMTU(tb.mtu) {
				d.Verdict, d.PathMTU = model.PMTUVerdictReduced, tb.mtu
				return d
			}
			hi = mid
		case errors.Is(stepErr, errPMTULost):
			hi = mid
		default:
			return s.noVerdict(d, stoppedReason(stepErr))
		}
	}

	if lo == pmtuBaseSize && d.Truncated {
		return s.noVerdict(d, fmt.Sprintf("the search ran out of its time budget (half the pmtu interval, or the "+
			"run's deadline) before any size above %d bytes crossed", pmtuBaseSize))
	}
	if lo == pmtuBaseSize && !sawTooBig {
		// Every size the bisection tried was lost, down to a few bytes: a lossy path, not a small one.
		return s.noVerdict(d, fmt.Sprintf("the full size and every size tried above %d bytes were lost: "+
			"a lossy path, not an MTU verdict", pmtuBaseSize))
	}
	d.PathMTU = lo
	d.Verdict = model.PMTUVerdictBlackhole
	if sawTooBig {
		// Refusals came back without a number (Darwin cannot read the learned MTU): the path still
		// signals that it is smaller, so it is reduced, not silent.
		d.Verdict = model.PMTUVerdictReduced
		return d
	}
	return s.confirmBlackhole(d)
}

// confirmBlackhole sends the full size and the size that crossed once more each. It does not look at
// the deadline: the bisection kept the time for both in reserve.
func (s *pmtuSearch) confirmBlackhole(d model.PMTUDetails) model.PMTUDetails {
	err := s.send(s.probeMTU, 1, &d)
	if err == nil {
		d.Verdict, d.PathMTU = model.PMTUVerdictOK, s.probeMTU
		return d
	}
	if tb, refused := errors.AsType[*pmtuTooBigError](err); refused {
		d.Verdict = model.PMTUVerdictReduced
		if s.usableMTU(tb.mtu) {
			d.PathMTU = tb.mtu
		}
		return d
	}
	if !errors.Is(err, errPMTULost) {
		return s.noVerdict(d, stoppedReason(err))
	}
	if err := s.send(d.PathMTU, 1, &d); err != nil {
		if !errors.Is(err, errPMTULost) {
			return s.noVerdict(d, stoppedReason(err))
		}
		return s.noVerdict(d, fmt.Sprintf("%d bytes crossed once and were lost when sent again: "+
			"a lossy path, not an MTU verdict", d.PathMTU))
	}
	return d
}
