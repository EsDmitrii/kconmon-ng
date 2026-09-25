package checker

import (
	"context"
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

func TestPMTUSearchStopsAtTheDeadline(t *testing.T) {
	p := &fakePMTUPath{passUpTo: 1400}
	s := newTestSearch(context.Background(), p, 1500, time.Now().Add(-time.Second))
	d := s.run()
	if !d.Truncated || d.Verdict != model.PMTUVerdictBlackhole || d.PathMTU != pmtuBaseSize {
		t.Fatalf("run() = %+v, want a truncated black hole with the base size as the lower bound", d)
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
