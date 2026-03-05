package checker

import (
	"context"
	"testing"
	"time"

	"github.com/EsDmitrii/kconmon-ng/internal/model"
)

func TestDNSCheckerSuccess(t *testing.T) {
	c := NewDNSChecker([]string{"localhost"}, nil)
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
	c := NewDNSChecker([]string{"localhost", "localhost"}, nil)
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
	c := NewDNSChecker([]string{"this.host.definitely.does.not.exist.invalid"}, nil)

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
	c := NewDNSChecker(nil, nil)
	result := c.Check(context.Background(), Target{})

	if !result.Success {
		t.Error("expected success for empty hosts list")
	}
}
