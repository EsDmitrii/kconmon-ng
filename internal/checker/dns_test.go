package checker

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/EsDmitrii/kconmon-ng/internal/model"
)

func TestDNSCheckerSuccess(t *testing.T) {
	c := NewDNSChecker([]string{"localhost"}, nil, 5*time.Second)
	result := c.Check(context.Background(), Target{})

	if !result.Success {
		t.Errorf("expected success resolving localhost, got error: %s", result.Error)
	}
	if result.Type != model.CheckDNS {
		t.Errorf("expected type DNS, got %s", result.Type)
	}
	if result.Duration <= 0 {
		t.Error("expected positive duration")
	}
}

func TestDNSCheckerMultipleHosts(t *testing.T) {
	c := NewDNSChecker([]string{"localhost", "localhost"}, nil, 5*time.Second)
	result := c.Check(context.Background(), Target{})

	if !result.Success {
		t.Errorf("expected success, got error: %s", result.Error)
	}

	details, ok := result.Details.([]model.DNSDetails)
	if !ok {
		t.Fatal("expected []DNSDetails")
	}
	if len(details) != 2 {
		t.Errorf("expected 2 results, got %d", len(details))
	}
}

func TestDNSCheckerUnresolvable(t *testing.T) {
	c := NewDNSChecker([]string{"this.host.definitely.does.not.exist.invalid"}, nil, 5*time.Second)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	result := c.Check(ctx, Target{})

	if result.Success {
		t.Error("expected failure for unresolvable host")
	}
	if result.Error == "" {
		t.Error("expected error message")
	}
}

func TestDNSCheckerEmptyHosts(t *testing.T) {
	c := NewDNSChecker(nil, nil, 5*time.Second)
	result := c.Check(context.Background(), Target{})

	if !result.Success {
		t.Error("expected success for empty hosts list")
	}
}

func TestDNSCheckerTimeoutPropagated(t *testing.T) {
	const customTimeout = 3 * time.Second
	c := NewDNSChecker([]string{"localhost"}, nil, customTimeout)
	if c.timeout != customTimeout {
		t.Errorf("expected timeout %v, got %v", customTimeout, c.timeout)
	}
}

/* ── the configured timeout has to bound the lookup ──────────────────────── */

/*
 * checkers.dns.timeout used to be applied to exactly one thing: the UDP dial on the explicit-resolver
 * path, which is connectionless and returns immediately. On the default path it was not applied at
 * all, so the real bound was /etc/resolv.conf — with kubelet's ndots:5 and three search domains,
 * about forty seconds for a check configured to give up after one.
 */
func TestDNSCheckerHonoursItsOwnTimeout(t *testing.T) {
	// A resolver that never answers: the only thing that can end this lookup is the checker's bound.
	blocked := &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		},
	}

	c := &DNSChecker{hosts: []string{"nowhere.invalid"}, timeout: 200 * time.Millisecond}

	start := time.Now()
	_, err := c.lookupHost(context.Background(), "nowhere.invalid", "test", blocked)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("lookup against a resolver that never answers returned no error")
	}
	if elapsed > 2*time.Second {
		t.Errorf("lookup took %s against a 200ms timeout — the bound is not being applied", elapsed)
	}
}
