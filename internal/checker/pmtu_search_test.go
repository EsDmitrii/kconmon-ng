package checker

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/EsDmitrii/kconmon-ng/internal/model"
)

// fakePMTUPath is a path with a known MTU and a known way of failing above it.
type fakePMTUPath struct {
	passUpTo  int         // datagrams up to this size cross
	refuse    bool        // oversize datagrams are refused with a reason instead of vanishing
	tooBigMTU int         // the MTU a refusal reports; 0 = the OS cannot say (Darwin)
	down      bool        // nothing crosses, not even the base datagram
	drops     map[int]int // size -> how many sends of that size are lost first
	sizes     []int       // every size that went out, in order
}

func (f *fakePMTUPath) Send(size int, _ time.Duration) error {
	f.sizes = append(f.sizes, size)
	if f.down {
		return errPMTULost
	}
	if f.drops[size] > 0 {
		f.drops[size]--
		return errPMTULost
	}
	if size <= f.passUpTo {
		return nil
	}
	if f.refuse {
		return &pmtuTooBigError{mtu: f.tooBigMTU}
	}
	return errPMTULost
}

func newTestSearch(ctx context.Context, path pmtuPath, probeMTU int, deadline time.Time) pmtuSearch {
	return pmtuSearch{
		ctx: ctx, path: path, probeMTU: probeMTU, timeout: time.Millisecond,
		deadline: deadline, now: time.Now,
	}
}

func TestPMTUSearchVerdicts(t *testing.T) {
	tests := []struct {
		name    string
		path    *fakePMTUPath
		probe   int
		verdict string
		pathMTU int
	}{
		{"healthy path", &fakePMTUPath{passUpTo: 1500}, 1500, model.PMTUVerdictOK, 1500},
		{"router reports the MTU", &fakePMTUPath{passUpTo: 1450, refuse: true, tooBigMTU: 1450}, 1500,
			model.PMTUVerdictReduced, 1450},
		{"refusals without a number are bisected", &fakePMTUPath{passUpTo: 1450, refuse: true}, 1500,
			model.PMTUVerdictReduced, 1450},
		{"reported MTU not below the probe is bisected", &fakePMTUPath{passUpTo: 1400, refuse: true, tooBigMTU: 1500},
			1500, model.PMTUVerdictReduced, 1400},
		{"silent drop above 1400", &fakePMTUPath{passUpTo: 1400}, 1500, model.PMTUVerdictBlackhole, 1400},
		{"jumbo probe on a 1500 path", &fakePMTUPath{passUpTo: 1500}, 9000, model.PMTUVerdictBlackhole, 1500},
		{"largest IPv4 probe converges", &fakePMTUPath{passUpTo: 1280}, 65535, model.PMTUVerdictBlackhole, 1280},
		{"peer down", &fakePMTUPath{down: true}, 1500, model.PMTUVerdictUnreachable, 0},
		{"one random drop of the full size", &fakePMTUPath{passUpTo: 1500, drops: map[int]int{1500: 1}}, 1500,
			model.PMTUVerdictOK, 1500},
		{"one random drop of the base datagram", &fakePMTUPath{passUpTo: 1500, drops: map[int]int{pmtuBaseSize: 1}},
			1500, model.PMTUVerdictOK, 1500},
		{"the full size lost twice at random", &fakePMTUPath{passUpTo: 1500, drops: map[int]int{1500: 2}}, 1500,
			model.PMTUVerdictOK, 1500},
		{"everything above the base lost", &fakePMTUPath{passUpTo: pmtuBaseSize}, 1500,
			model.PMTUVerdictUnreachable, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestSearch(context.Background(), tc.path, tc.probe, time.Now().Add(time.Hour))
			d := s.run()
			if d.Verdict != tc.verdict || d.PathMTU != tc.pathMTU || d.ProbeMTU != tc.probe {
				t.Fatalf("run() = %+v, want verdict %s, path %d, probe %d", d, tc.verdict, tc.pathMTU, tc.probe)
			}
			if d.Steps != len(tc.path.sizes) {
				t.Errorf("Steps = %d, but %d datagrams went out", d.Steps, len(tc.path.sizes))
			}
			if d.Truncated {
				t.Errorf("an hour of budget must not truncate: %+v", d)
			}
		})
	}
}

// flakyAbove crosses every size up to passUpTo once, then loses the one size it was told to lose on its
// second send: a lossy path, not a smaller one.
type flakyAbove struct {
	fakePMTUPath
	loseOnRepeat int
	seen         map[int]int
}

func (f *flakyAbove) Send(size int, timeout time.Duration) error {
	f.seen[size]++
	if size == f.loseOnRepeat && f.seen[size] > 1 {
		f.sizes = append(f.sizes, size)
		return errPMTULost
	}
	return f.fakePMTUPath.Send(size, timeout)
}

// lateICMP loses the full size silently, then the router's frag-needed for it lands as the error of
// the first bisection send, whatever that send's size: the number is the router's and must be kept.
type lateICMP struct {
	fakePMTUPath
	delivered bool
}

func (f *lateICMP) Send(size int, timeout time.Duration) error {
	if size < 1500 && size > pmtuBaseSize && !f.delivered {
		f.delivered = true
		f.sizes = append(f.sizes, size)
		return &pmtuTooBigError{mtu: 1400}
	}
	return f.fakePMTUPath.Send(size, timeout)
}

func TestPMTUSearchKeepsALateRouterNumber(t *testing.T) {
	p := &lateICMP{fakePMTUPath: fakePMTUPath{passUpTo: 1400}}
	s := newTestSearch(context.Background(), p, 1500, time.Now().Add(time.Hour))
	if d := s.run(); d.Verdict != model.PMTUVerdictReduced || d.PathMTU != 1400 {
		t.Fatalf("run() = %+v, want reduced at the router's 1400", d)
	}
}

// The size the bisection settled on must cross again before the pair is called a black hole.
func TestPMTUSearchBlackholeNeedsTheFoundSizeToCrossAgain(t *testing.T) {
	p := &flakyAbove{fakePMTUPath: fakePMTUPath{passUpTo: 1400}, loseOnRepeat: 1400, seen: map[int]int{}}
	s := newTestSearch(context.Background(), p, 1500, time.Now().Add(time.Hour))
	if d := s.run(); d.Verdict != model.PMTUVerdictUnreachable || d.PathMTU != 0 {
		t.Fatalf("run() = %+v, want unreachable: 1400 crossed once and then vanished", d)
	}
}

// A healthy path costs exactly two datagrams: the budget arithmetic in the spec depends on it.
func TestPMTUSearchHealthyPathSendsTwoDatagrams(t *testing.T) {
	p := &fakePMTUPath{passUpTo: 1500}
	s := newTestSearch(context.Background(), p, 1500, time.Now().Add(time.Hour))
	s.run()
	if len(p.sizes) != 2 || p.sizes[0] != pmtuBaseSize || p.sizes[1] != 1500 {
		t.Fatalf("sizes = %v, want [%d 1500]", p.sizes, pmtuBaseSize)
	}
}

// Worst case per peer: base and full size twice each, one datagram per bisection step, and the two
// that confirm a black hole.
func TestPMTUSearchWorstCaseIsBounded(t *testing.T) {
	p := &fakePMTUPath{passUpTo: 1280}
	s := newTestSearch(context.Background(), p, 65535, time.Now().Add(time.Hour))
	s.run()
	if limit := 1 + pmtuAttempts + pmtuMaxSteps + pmtuConfirmSends; len(p.sizes) > limit {
		t.Fatalf("%d datagrams went out, the budget allows %d", len(p.sizes), limit)
	}
}

// A search that ran out of budget before any size above the base crossed has no size to report:
// "64 bytes cross" is no path MTU, and a lossy path would read as a black hole.
func TestPMTUSearchStopsAtTheDeadline(t *testing.T) {
	p := &fakePMTUPath{passUpTo: 1400}
	s := newTestSearch(context.Background(), p, 1500, time.Now().Add(-time.Second))
	d := s.run()
	if !d.Truncated || d.Verdict != model.PMTUVerdictUnreachable || d.PathMTU != 0 {
		t.Fatalf("run() = %+v, want a truncated search with no MTU verdict", d)
	}
	if !strings.Contains(s.reason, "time budget") {
		t.Errorf("reason = %q, want the time budget named", s.reason)
	}
}

// clockedPath charges the search's clock one timeout for every datagram that did not come back, as a
// real socket does, and nothing for an echo.
type clockedPath struct {
	pmtuPath
	clock *time.Time
}

func (p *clockedPath) Send(size int, timeout time.Duration) error {
	err := p.pmtuPath.Send(size, timeout)
	if errors.Is(err, errPMTULost) {
		*p.clock = p.clock.Add(timeout)
	}
	return err
}

func newClockedSearch(path pmtuPath, probeMTU int, budget time.Duration) *pmtuSearch {
	clock := time.Unix(0, 0)
	return &pmtuSearch{
		ctx: context.Background(), path: &clockedPath{pmtuPath: path, clock: &clock}, probeMTU: probeMTU,
		timeout: 100 * time.Millisecond, deadline: clock.Add(budget), now: func() time.Time { return clock },
	}
}

// A healthy path that drops the full size twice and one bisection step must not become a black hole
// because the budget ran out: whatever the budget, the verdict is confirmed or there is none.
func TestPMTUSearchTruncatedLossyPathIsNeverABlackhole(t *testing.T) {
	for budget := 300 * time.Millisecond; budget <= time.Second; budget += 50 * time.Millisecond {
		p := &fakePMTUPath{passUpTo: 1500, drops: map[int]int{1500: 2, 1141: 1}}
		s := newClockedSearch(p, 1500, budget)
		if d := s.run(); d.Verdict == model.PMTUVerdictBlackhole {
			t.Errorf("budget %v: run() = %+v after sizes %v, want no black hole on a healthy path", budget, d, p.sizes)
		}
	}
}

// A real black hole the budget cuts short is still confirmed: the full size vanishes once more and
// the size the search settled on crosses once more.
func TestPMTUSearchTruncatedBlackholeIsConfirmed(t *testing.T) {
	p := &fakePMTUPath{passUpTo: 1400}
	s := newClockedSearch(p, 1500, 650*time.Millisecond)
	d := s.run()
	if d.Verdict != model.PMTUVerdictBlackhole || !d.Truncated || d.PathMTU < 1320 || d.PathMTU > 1400 {
		t.Fatalf("run() = %+v, want a truncated black hole with a lower bound in [1320, 1400]", d)
	}
	if n := len(p.sizes); n < 2 || p.sizes[n-2] != 1500 || p.sizes[n-1] != d.PathMTU {
		t.Fatalf("sizes = %v, want the full size and %d sent last to confirm", p.sizes, d.PathMTU)
	}
}

// Every unreachable verdict says why: the error reaches the CLI and the run detail.
func TestPMTUSearchUnreachableCarriesItsReason(t *testing.T) {
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	tests := []struct {
		name string
		ctx  context.Context
		path pmtuPath
		want string
	}{
		{"base lost", context.Background(), &fakePMTUPath{down: true}, "did not answer a 64-byte datagram"},
		{"everything above the base lost", context.Background(), &fakePMTUPath{passUpTo: pmtuBaseSize}, "lossy path"},
		{"found size lost when sent again",
			context.Background(), &flakyAbove{fakePMTUPath: fakePMTUPath{passUpTo: 1400}, loseOnRepeat: 1400, seen: map[int]int{}},
			"1400 bytes crossed once"},
		{"cancelled", cancelled, &fakePMTUPath{passUpTo: 1500}, "context canceled"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestSearch(tc.ctx, tc.path, 1500, time.Now().Add(time.Hour))
			d := s.run()
			if d.Verdict != model.PMTUVerdictUnreachable || !strings.Contains(s.reason, tc.want) {
				t.Fatalf("run() = %+v, reason %q, want unreachable with a reason containing %q", d, s.reason, tc.want)
			}
		})
	}
}

func TestPMTUSearchCancelledContextIsNoVerdict(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	p := &fakePMTUPath{passUpTo: 1500}
	s := newTestSearch(ctx, p, 1500, time.Now().Add(time.Hour))
	if d := s.run(); d.Verdict != model.PMTUVerdictUnreachable || len(p.sizes) != 0 {
		t.Fatalf("run() = %+v after %d sends, want unreachable with nothing sent", d, len(p.sizes))
	}
}

// At PMTUMinInterval a black hole of any size a real path has is sized and confirmed inside the budget,
// with nothing cut short: config warns below it.
func TestPMTUMinIntervalFitsARealBlackhole(t *testing.T) {
	const timeout = 100 * time.Millisecond // newClockedSearch's
	if got := PMTUMinInterval(500 * time.Millisecond); got != 14*time.Second {
		t.Errorf("PMTUMinInterval(500ms) = %v, want 14s", got)
	}
	for _, tc := range []struct{ probe, hole int }{
		{1500, 1280}, {1500, 1400}, {1500, 1499}, {1500, pmtuMinSize}, {9000, 1500}, {9000, 8999}, {65535, 9000},
	} {
		p := &fakePMTUPath{passUpTo: tc.hole}
		s := newClockedSearch(p, tc.probe, PMTUMinInterval(timeout)/2)
		if d := s.run(); d.Verdict != model.PMTUVerdictBlackhole || d.Truncated || d.PathMTU != tc.hole {
			t.Errorf("probe %d, hole %d: run() = %+v, want a confirmed black hole at %d inside the budget",
				tc.probe, tc.hole, d, tc.hole)
		}
	}
}
