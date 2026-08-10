package checker

import (
	"context"
	"errors"
	"net/netip"
	"strings"
	"testing"
)

// stubResolver is the injected Resolver used by every AllowHostPort test, so
// none of them touch DNS. It records call count so the literal-IP path can be
// asserted to skip resolution entirely.
type stubResolver struct {
	calls int
	hosts map[string][]netip.Addr
	err   error
}

func (s *stubResolver) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	s.calls++
	if s.err != nil {
		return nil, s.err
	}
	return s.hosts[host], nil
}

func mustAllowlist(t *testing.T, allowed, denied []string) *Allowlist {
	t.Helper()
	a, err := NewAllowlist(allowed, denied)
	if err != nil {
		t.Fatalf("NewAllowlist(%v, %v) failed: %v", allowed, denied, err)
	}
	return a
}

func TestNewAllowlistRejectsMalformedCIDRWithIndex(t *testing.T) {
	_, err := NewAllowlist([]string{"10.0.0.0/8", "192.168.0.0/16", "not-a-cidr"}, nil)
	if err == nil {
		t.Fatal("expected an error for a malformed allowed CIDR")
	}
	if !strings.Contains(err.Error(), "allowedCidrs[2]") {
		t.Errorf("error must name the offending index, got: %v", err)
	}

	_, err = NewAllowlist([]string{"10.0.0.0/8"}, []string{"10.0.0.0/8", "::/nope"})
	if err == nil {
		t.Fatal("expected an error for a malformed denied CIDR")
	}
	if !strings.Contains(err.Error(), "deniedCidrs[1]") {
		t.Errorf("error must name the offending index, got: %v", err)
	}
}

func TestNewAllowlistRejectsBareIPWithoutPrefix(t *testing.T) {
	if _, err := NewAllowlist([]string{"10.0.0.1"}, nil); err == nil {
		t.Fatal("a bare IP is not a CIDR and must be rejected rather than guessed at")
	}
}

// A 4-in-6 written prefix (::ffff:10.0.0.0/104) is ambiguous; rather than silently reinterpret the
// operator's mask, the constructor refuses.
func TestNewAllowlistRejectsMappedV4Prefix(t *testing.T) {
	_, err := NewAllowlist([]string{"::ffff:10.0.0.0/104"}, nil)
	if err == nil {
		t.Fatal("expected a 4-in-6 mapped prefix to be rejected")
	}
	if !strings.Contains(err.Error(), "allowedCidrs[0]") {
		t.Errorf("error must name the offending index, got: %v", err)
	}
}

func TestAllowMatchesIPv4AndIPv6Prefixes(t *testing.T) {
	a := mustAllowlist(t, []string{"10.0.0.0/8", "2001:db8::/32"}, nil)

	allowed := []string{"10.0.0.1", "10.255.255.254", "2001:db8::1", "2001:db8:ffff::9"}
	for _, s := range allowed {
		if !a.Allow(netip.MustParseAddr(s)) {
			t.Errorf("Allow(%s) = false, want true", s)
		}
	}

	denied := []string{"11.0.0.1", "192.168.1.1", "2001:db9::1", "fd00::1"}
	for _, s := range denied {
		if a.Allow(netip.MustParseAddr(s)) {
			t.Errorf("Allow(%s) = true, want false", s)
		}
	}
}

func TestAllowDenyWinsOverAllowOnOverlap(t *testing.T) {
	a := mustAllowlist(t, []string{"10.0.0.0/8", "2001:db8::/32"}, []string{"10.1.2.0/24", "2001:db8:dead::/48"})

	if a.Allow(netip.MustParseAddr("10.1.2.3")) {
		t.Error("an address inside both an allowed and a denied prefix must be denied")
	}
	if a.Allow(netip.MustParseAddr("2001:db8:dead::1")) {
		t.Error("IPv6: deny must win over allow")
	}
	if !a.Allow(netip.MustParseAddr("10.1.3.3")) {
		t.Error("a neighbouring address outside the denied prefix must still be allowed")
	}
}

func TestAllowAddressInNoAllowedPrefixIsDenied(t *testing.T) {
	a := mustAllowlist(t, []string{"10.0.0.0/8"}, nil)
	if a.Allow(netip.MustParseAddr("203.0.113.1")) {
		t.Error("the allowlist is exhaustive: an address in no allowed prefix must be denied")
	}
}

func TestAllowWithNoAllowedPrefixesDeniesEverything(t *testing.T) {
	a := mustAllowlist(t, nil, nil)
	for _, s := range []string{"10.0.0.1", "127.0.0.1", "::1", "8.8.8.8"} {
		if a.Allow(netip.MustParseAddr(s)) {
			t.Errorf("empty allowlist must deny %s", s)
		}
	}

	var nilList *Allowlist
	if nilList.Allow(netip.MustParseAddr("10.0.0.1")) {
		t.Error("a nil Allowlist must deny, not panic or permit")
	}
}

// The 4-in-6 mapped form is the classic allowlist hole: ::ffff:10.0.0.1 is
// 10.0.0.1 on the wire, so it must be matched against IPv4 prefixes, and a
// denied IPv4 range must catch its mapped spelling too.
func TestAllowMappedV4FormsMatchIPv4PrefixesBothDirections(t *testing.T) {
	a := mustAllowlist(t, []string{"10.0.0.0/8"}, []string{"10.1.2.0/24"})

	if !a.Allow(netip.MustParseAddr("::ffff:10.0.0.1")) {
		t.Error("mapped form of an allowed IPv4 address must be allowed")
	}
	if a.Allow(netip.MustParseAddr("::ffff:10.1.2.3")) {
		t.Error("mapped form of a denied IPv4 address must be denied")
	}
	if a.Allow(netip.MustParseAddr("::ffff:127.0.0.1")) {
		t.Error("mapped form of an address in no allowed prefix must be denied")
	}
	if a.Allow(netip.MustParseAddr("::ffff:169.254.169.254")) {
		t.Error("mapped form of the cloud metadata address must be denied")
	}
}

// A zone-scoped address is refused outright rather than having its zone
// stripped: stripping would let fe80::1%eth0 match an fe80::/10 allow prefix
// and then be dialled out of whichever interface the stack picks.
func TestAllowZoneScopedAddressIsDenied(t *testing.T) {
	a := mustAllowlist(t, []string{"fe80::/10", "10.0.0.0/8"}, nil)

	if a.Allow(netip.MustParseAddr("fe80::1%eth0")) {
		t.Error("a zone-scoped address must be denied even when its zoneless form is allowed")
	}
	if !a.Allow(netip.MustParseAddr("fe80::1")) {
		t.Error("the zoneless form is still governed by the operator's prefixes")
	}
	// A zone on a 4-in-6 spelling must not survive Unmap into a plain v4 match.
	if a.Allow(netip.MustParseAddr("::ffff:10.0.0.1%eth0")) {
		t.Error("a zone-scoped 4-in-6 address must be denied, not unmapped into an allowed v4 match")
	}
}

func TestAllowInvalidAddressIsDenied(t *testing.T) {
	a := mustAllowlist(t, []string{"0.0.0.0/0", "::/0"}, nil)
	if a.Allow(netip.Addr{}) {
		t.Error("the zero Addr must be denied even under a catch-all allowlist")
	}
}

// Loopback, link-local and multicast get no special case in code: they are denied because they sit
// in no allowed prefix.
func TestAllowSpecialRangesFollowTheOperatorNotTheCode(t *testing.T) {
	special := []string{"127.0.0.1", "::1", "169.254.169.254", "fe80::1", "224.0.0.1", "ff02::1"}

	strict := mustAllowlist(t, []string{"10.0.0.0/8"}, nil)
	for _, s := range special {
		if strict.Allow(netip.MustParseAddr(s)) {
			t.Errorf("%s must be denied when only 10.0.0.0/8 is allowed", s)
		}
	}

	permissive := mustAllowlist(t, []string{"127.0.0.0/8", "::1/128", "169.254.0.0/16", "fe80::/10", "224.0.0.0/4", "ff00::/8"}, nil)
	for _, s := range special {
		if !permissive.Allow(netip.MustParseAddr(s)) {
			t.Errorf("%s must be allowed when the operator explicitly allowed its range", s)
		}
	}
}

func TestAllowHostPortLiteralIPSkipsDNS(t *testing.T) {
	a := mustAllowlist(t, []string{"10.0.0.0/8"}, nil)
	r := &stubResolver{hosts: map[string][]netip.Addr{}}

	if err := a.AllowHostPort(t.Context(), r, "10.0.0.7"); err != nil {
		t.Fatalf("literal allowed IP must pass: %v", err)
	}
	if r.calls != 0 {
		t.Errorf("a literal IP must never be sent to DNS, resolver called %d times", r.calls)
	}

	if err := a.AllowHostPort(t.Context(), r, "203.0.113.9"); err == nil {
		t.Fatal("literal denied IP must be refused")
	}
	if r.calls != 0 {
		t.Errorf("a denied literal IP must not be sent to DNS either, resolver called %d times", r.calls)
	}

	// Bracketed IPv6 literals are still literals.
	if err := a.AllowHostPort(t.Context(), r, "[::1]"); err == nil {
		t.Fatal("bracketed literal outside the allowlist must be refused")
	}
	if r.calls != 0 {
		t.Errorf("a bracketed literal must not be sent to DNS, resolver called %d times", r.calls)
	}

	// A mapped literal must be matched against the IPv4 prefixes.
	if err := a.AllowHostPort(t.Context(), r, "::ffff:10.0.0.7"); err != nil {
		t.Fatalf("mapped literal of an allowed v4 address must pass: %v", err)
	}
	if r.calls != 0 {
		t.Errorf("a mapped literal must not be sent to DNS, resolver called %d times", r.calls)
	}
}

func TestAllowHostPortAllResolvedAddressesAllowed(t *testing.T) {
	a := mustAllowlist(t, []string{"10.0.0.0/8", "2001:db8::/32"}, nil)
	r := &stubResolver{hosts: map[string][]netip.Addr{
		"svc.internal": {
			netip.MustParseAddr("10.0.0.1"),
			netip.MustParseAddr("10.9.9.9"),
			netip.MustParseAddr("2001:db8::5"),
		},
	}}

	if err := a.AllowHostPort(t.Context(), r, "svc.internal"); err != nil {
		t.Fatalf("all resolved addresses are allowed, want nil error, got: %v", err)
	}
	if r.calls != 1 {
		t.Errorf("expected exactly one resolution, got %d", r.calls)
	}
}

// A name that resolves to one permitted and one forbidden address is denied
// outright: the connection, not the allowlist, picks which address is dialled.
func TestAllowHostPortPartialResolutionIsDenied(t *testing.T) {
	a := mustAllowlist(t, []string{"10.0.0.0/8"}, nil)
	r := &stubResolver{hosts: map[string][]netip.Addr{
		"rebind.example.com": {
			netip.MustParseAddr("10.0.0.1"),
			netip.MustParseAddr("169.254.169.254"),
		},
	}}

	if err := a.AllowHostPort(t.Context(), r, "rebind.example.com"); err == nil {
		t.Fatal("a partially allowed resolution must be denied")
	}
}

func TestAllowHostPortDeniedResolvedAddressIsDeniedEvenWhenAlsoAllowed(t *testing.T) {
	a := mustAllowlist(t, []string{"10.0.0.0/8"}, []string{"10.1.2.0/24"})
	r := &stubResolver{hosts: map[string][]netip.Addr{
		"inner.example.com": {netip.MustParseAddr("10.1.2.3")},
	}}

	if err := a.AllowHostPort(t.Context(), r, "inner.example.com"); err == nil {
		t.Fatal("a resolved address inside a denied prefix must be refused")
	}
}

func TestAllowHostPortResolutionFailureIsDenial(t *testing.T) {
	a := mustAllowlist(t, []string{"0.0.0.0/0", "::/0"}, nil)
	r := &stubResolver{err: errors.New("lookup nowhere.example.com: no such host")}

	err := a.AllowHostPort(t.Context(), r, "nowhere.example.com")
	if err == nil {
		t.Fatal("a resolution failure must deny, never allow")
	}
	if strings.Contains(err.Error(), "nowhere.example.com") {
		t.Errorf("the refusal must not echo the hostname back, got: %v", err)
	}
	if strings.Contains(err.Error(), "no such host") {
		t.Errorf("the refusal must not echo the resolver error back, got: %v", err)
	}
}

func TestAllowHostPortEmptyResolutionIsDenial(t *testing.T) {
	a := mustAllowlist(t, []string{"0.0.0.0/0", "::/0"}, nil)
	r := &stubResolver{hosts: map[string][]netip.Addr{"void.example.com": {}}}

	if err := a.AllowHostPort(t.Context(), r, "void.example.com"); err == nil {
		t.Fatal("an empty resolution result must deny, never allow")
	}
}

func TestAllowHostPortNilResolverAndEmptyHostAreDenials(t *testing.T) {
	a := mustAllowlist(t, []string{"0.0.0.0/0", "::/0"}, nil)

	if err := a.AllowHostPort(t.Context(), nil, "svc.internal"); err == nil {
		t.Error("a missing resolver must deny rather than allow or panic")
	}
	r := &stubResolver{}
	if err := a.AllowHostPort(t.Context(), r, ""); err == nil {
		t.Error("an empty host must deny")
	}
	if err := a.AllowHostPort(t.Context(), r, "   "); err == nil {
		t.Error("a blank host must deny")
	}

	var nilList *Allowlist
	if err := nilList.AllowHostPort(t.Context(), r, "10.0.0.1"); err == nil {
		t.Error("a nil Allowlist must deny rather than panic")
	}
}

// The refusal string is copied into a TaskResult that flows back through the
// controller into the event stream, so it must name only the address family --
// never the attacker-chosen hostname or the address it resolved to.
func TestAllowHostPortRefusalNamesOnlyTheAddressFamily(t *testing.T) {
	a := mustAllowlist(t, []string{"10.0.0.0/8"}, nil)
	r := &stubResolver{hosts: map[string][]netip.Addr{
		"evil.example.com": {netip.MustParseAddr("169.254.169.254")},
		"evil6.example.com": {
			netip.MustParseAddr("2001:db8::dead"),
		},
	}}

	err := a.AllowHostPort(t.Context(), r, "evil.example.com")
	if err == nil {
		t.Fatal("expected a refusal")
	}
	msg := err.Error()
	for _, leak := range []string{"evil.example.com", "169.254.169.254", "example"} {
		if strings.Contains(msg, leak) {
			t.Errorf("refusal leaked %q: %s", leak, msg)
		}
	}
	if !strings.Contains(msg, "IPv4") {
		t.Errorf("refusal must name the refused address family, got: %s", msg)
	}

	err6 := a.AllowHostPort(t.Context(), r, "evil6.example.com")
	if err6 == nil {
		t.Fatal("expected a refusal for the IPv6 destination")
	}
	if strings.Contains(err6.Error(), "2001:db8") || strings.Contains(err6.Error(), "evil6") {
		t.Errorf("refusal leaked the destination: %s", err6.Error())
	}
	if !strings.Contains(err6.Error(), "IPv6") {
		t.Errorf("refusal must name the refused address family, got: %s", err6.Error())
	}
}

// ResolveAllowed is what the executor calls: it returns the approved addresses
// so the probe dials exactly what was authorised instead of resolving again.
func TestResolveAllowedReturnsUnmappedApprovedAddresses(t *testing.T) {
	a := mustAllowlist(t, []string{"10.0.0.0/8"}, nil)
	r := &stubResolver{hosts: map[string][]netip.Addr{
		"svc.internal": {netip.MustParseAddr("::ffff:10.0.0.4"), netip.MustParseAddr("10.0.0.5")},
	}}

	addrs, err := a.ResolveAllowed(t.Context(), r, "svc.internal")
	if err != nil {
		t.Fatalf("unexpected refusal: %v", err)
	}
	if len(addrs) != 2 {
		t.Fatalf("expected 2 approved addresses, got %d (%v)", len(addrs), addrs)
	}
	for _, addr := range addrs {
		if addr.Is4In6() {
			t.Errorf("approved address %v is still 4-in-6 mapped; the dialled form must be unmapped", addr)
		}
	}
	if addrs[0].String() != "10.0.0.4" || addrs[1].String() != "10.0.0.5" {
		t.Errorf("approved addresses = %v, want [10.0.0.4 10.0.0.5]", addrs)
	}
}

func TestResolveAllowedZoneScopedResolverResultIsDenied(t *testing.T) {
	a := mustAllowlist(t, []string{"fe80::/10"}, nil)
	r := &stubResolver{hosts: map[string][]netip.Addr{
		"local.internal": {netip.MustParseAddr("fe80::1%eth0")},
	}}

	if _, err := a.ResolveAllowed(t.Context(), r, "local.internal"); err == nil {
		t.Fatal("a zone-scoped resolver result must be denied")
	}
}

// FuzzAllowlist drives the constructor with arbitrary CIDR strings and Allow
// with arbitrary address bytes. Two invariants: nothing panics, and a denied
// address stays denied no matter what the fuzzer feeds the constructor.
func FuzzAllowlist(f *testing.F) {
	f.Add("10.0.0.0/8", "10.1.0.0/16", []byte{10, 0, 0, 1})
	f.Add("::/0", "2001:db8::/32", []byte{0x20, 0x01, 0x0d, 0xb8, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1})
	f.Add("0.0.0.0/0", "", []byte("127.0.0.1"))
	f.Add("::ffff:10.0.0.0/104", "", []byte("::ffff:127.0.0.1"))
	f.Add("", "", []byte{})
	f.Add("10.0.0.0/8", "", []byte("fe80::1%eth0"))
	f.Add("garbage/33", "///", []byte{0xff})

	loopbacks := []netip.Addr{
		netip.MustParseAddr("127.0.0.1"),
		netip.MustParseAddr("127.255.255.254"),
		netip.MustParseAddr("::ffff:127.0.0.1"),
		netip.MustParseAddr("::1"),
	}

	f.Fuzz(func(t *testing.T, allowed, denied string, raw []byte) {
		a, err := NewAllowlist([]string{allowed}, []string{denied})
		if err != nil {
			if a != nil {
				t.Fatal("NewAllowlist must return a nil Allowlist alongside an error")
			}
			return
		}

		var addr netip.Addr
		switch len(raw) {
		case 4:
			addr = netip.AddrFrom4([4]byte(raw))
		case 16:
			addr = netip.AddrFrom16([16]byte(raw))
		default:
			addr, _ = netip.ParseAddr(string(raw))
		}

		// Must not panic for any address, valid or not.
		allowedAddr := a.Allow(addr)

		// An address the constructor never saw a prefix for cannot be allowed.
		if allowedAddr && len(a.allowed) == 0 {
			t.Fatalf("Allow(%v) = true with no allowed prefixes", addr)
		}

		// A fixed, strict allowlist must never permit loopback regardless of
		// what the fuzzer did to the other instance.
		strict, serr := NewAllowlist([]string{"10.0.0.0/8"}, nil)
		if serr != nil {
			t.Fatalf("fixed allowlist failed to build: %v", serr)
		}
		for _, lo := range loopbacks {
			if strict.Allow(lo) {
				t.Fatalf("loopback %v allowed under a 10.0.0.0/8-only allowlist", lo)
			}
		}
		if addr.IsValid() && addr.Zone() == "" && addr.Unmap().IsLoopback() && strict.Allow(addr) {
			t.Fatalf("fuzz-produced loopback %v allowed under a 10.0.0.0/8-only allowlist", addr)
		}
		if addr.IsValid() && addr.Zone() != "" && strict.Allow(addr) {
			t.Fatalf("zone-scoped %v allowed", addr)
		}

		// AllowHostPort must not panic on arbitrary host strings either.
		r := &stubResolver{hosts: map[string][]netip.Addr{string(raw): {addr}}}
		_ = a.AllowHostPort(t.Context(), r, string(raw))
	})
}
