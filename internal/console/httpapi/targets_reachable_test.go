package httpapi

import (
	"context"
	"errors"
	"net/netip"
	"testing"
	"time"
)

/*
The refusal is deliberately narrow: it fires only when the Console KNOWS the allowlist and the
address is provably outside it. Everything else — no controller, no list, a name that will not
resolve — leaves the target alone, because "unknown" must not read as "forbidden".
*/

func prefixList(t *testing.T, cidrs ...string) externalAllowlist {
	t.Helper()
	list := externalAllowlist{raw: cidrs, known: true}
	for _, c := range cidrs {
		p, err := netip.ParsePrefix(c)
		if err != nil {
			t.Fatalf("bad fixture CIDR %q: %v", c, err)
		}
		list.prefixes = append(list.prefixes, p)
	}
	return list
}

func TestHostOfStripsPortAndBrackets(t *testing.T) {
	tests := map[string]string{
		"8.8.8.8":           "8.8.8.8",
		"8.8.8.8:53":        "8.8.8.8",
		"[2001:db8::1]:443": "2001:db8::1",
		"2001:db8::1":       "2001:db8::1",
		"example.test":      "example.test",
		"example.test:8080": "example.test",
		"  10.0.0.1  ":      "10.0.0.1",
	}
	for in, want := range tests {
		if got := hostOf(in); got != want {
			t.Errorf("hostOf(%q) = %q, want %q", in, got, want)
		}
	}
}

/*
A kind=url target is the case SplitHostPort answers WRONG rather than refusing: it cuts at the last
colon and never checks that what follows is a port, so "https://api.test/health" came back as the
host "https". The refusal then judged a label that is not an address and fired for no URL target at
all.
*/
func TestHostOfReadsAURLTarget(t *testing.T) {
	tests := map[string]string{
		"https://api.test/health":      "api.test",
		"http://api.test:8080/health":  "api.test",
		"https://10.0.0.1/health":      "10.0.0.1",
		"https://[2001:db8::1]:443/up": "2001:db8::1",
		"https://api.test":             "api.test",
		// Any "//" form is reduced to its host, whatever the scheme says.
		"ftp://api.test/x": "api.test",
		// No host at all: nothing resolvable, so nothing is refused downstream.
		"https://": "https",
	}
	for in, want := range tests {
		if got := hostOf(in); got != want {
			t.Errorf("hostOf(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestAllowlistContains(t *testing.T) {
	list := prefixList(t, "10.0.0.0/8", "8.8.8.8/32")
	inside := []string{"10.1.2.3", "8.8.8.8", "10.0.0.1:53"}
	outside := []string{"1.1.1.1", "192.168.1.1", "9.9.9.9:53"}

	for _, a := range inside {
		if !allowlistCovers(&list, hostOf(a)) {
			t.Errorf("%q read as outside an allowlist that contains it", a)
		}
	}
	for _, a := range outside {
		if allowlistCovers(&list, hostOf(a)) {
			t.Errorf("%q read as inside an allowlist that does not contain it", a)
		}
	}
}

func TestUnknownAllowlistNeverRefuses(t *testing.T) {
	// No controller, no list, or a list that parsed to nothing: the Console makes no claim.
	for _, list := range []externalAllowlist{{}, {known: false, raw: []string{"10.0.0.0/8"}}} {
		if allowlistCovers(&list, "1.1.1.1") {
			t.Error("an unknown allowlist answered a containment question")
		}
		if list.known {
			t.Error("fixture is not the unknown case")
		}
	}
}

/*
The agent's rule is all-must-pass: checker.Allowlist refuses a name on the FIRST resolved address
outside the list. The Console asked the opposite question -- is ANY address inside -- so a dual-stack
name with one covered record was accepted here and denied by every agent that probed it, which is
exactly the create-time answer this guard exists to give.

DNS is stubbed and nothing else is: the defect is the polarity of the loop over a given address set,
and that loop runs for real.
*/
func TestPartlyCoveredNameIsRefusedTheWayTheAgentWould(t *testing.T) {
	orig := lookupNetIP
	t.Cleanup(func() { lookupNetIP = orig })
	lookupNetIP = func(context.Context, string, string) ([]netip.Addr, error) {
		return []netip.Addr{
			netip.MustParseAddr("1.1.1.1"),              // inside
			netip.MustParseAddr("1.0.0.1"),              // outside: the agent denies on this one
			netip.MustParseAddr("2606:4700:4700::1111"), // outside
		}, nil
	}
	list := prefixList(t, "1.1.1.1/32")
	list.fetched = time.Now()
	s := &Server{allowlist: list}

	if _, outside := s.targetOutsideAllowlist(context.Background(), "one.one.one.one"); !outside {
		t.Fatal("accepted a name whose 1.0.0.1 record is outside the allowlist; every agent probe denies it")
	}
}

// The other half, without which the fix above could be "passed" by refusing everything.
func TestFullyCoveredNameIsStillAccepted(t *testing.T) {
	orig := lookupNetIP
	t.Cleanup(func() { lookupNetIP = orig })
	lookupNetIP = func(context.Context, string, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("1.1.1.1"), netip.MustParseAddr("8.8.8.8")}, nil
	}
	list := prefixList(t, "1.1.1.1/32", "8.8.8.8/32")
	list.fetched = time.Now()
	s := &Server{allowlist: list}

	if _, outside := s.targetOutsideAllowlist(context.Background(), "dns.test"); outside {
		t.Fatal("refused a name whose every record is inside the allowlist")
	}
}

// A name nobody can resolve is UNKNOWN, never forbidden.
func TestUnresolvableNameIsNotRefused(t *testing.T) {
	orig := lookupNetIP
	t.Cleanup(func() { lookupNetIP = orig })
	lookupNetIP = func(context.Context, string, string) ([]netip.Addr, error) { return nil, errors.New("no such host") }
	list := prefixList(t, "1.1.1.1/32")
	list.fetched = time.Now()
	s := &Server{allowlist: list}

	if _, outside := s.targetOutsideAllowlist(context.Background(), "nowhere.test"); outside {
		t.Fatal("refused a target on a DNS failure; unknown must not read as forbidden")
	}
}
