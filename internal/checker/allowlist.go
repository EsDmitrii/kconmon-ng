package checker

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"strings"
)

// Resolver is the minimal DNS surface the allowlist needs.
type Resolver interface {
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
}

// They are deliberately static, self-contained errors.
var (
	// ErrDeniedIPv4 and ErrDeniedIPv6 name only the family of the address that
	// failed the check.
	ErrDeniedIPv4 = errors.New("destination resolved into a denied IPv4 range")
	ErrDeniedIPv6 = errors.New("destination resolved into a denied IPv6 range")
	// ErrDeniedZoneScoped covers fe80::1%eth0-style addresses: a zone names a
	// local interface, which is not something a remote-destination allowlist can
	// reason about, so it is refused rather than stripped.
	ErrDeniedZoneScoped = errors.New("destination is a zone-scoped address, which is never permitted")
	// ErrResolutionFailed replaces the resolver's own error, which would contain
	// the queried name.
	ErrResolutionFailed = errors.New("destination could not be resolved")
	// ErrNoAddresses is an empty (but successful) resolution: nothing to check,
	// so nothing to allow.
	ErrNoAddresses = errors.New("destination resolved to no addresses")
	// ErrNoDestination is an empty/blank host.
	ErrNoDestination = errors.New("destination is empty")
	// ErrNoAllowlist means enforcement was asked for without a configured
	// allowlist or resolver. It denies rather than falling through.
	ErrNoAllowlist = errors.New("external destination checks are not configured on this agent")
)

// Allowlist decides whether an agent may probe an address; matching happens on the RESOLVED IP,
// never on the hostname.
type Allowlist struct{ allowed, denied []netip.Prefix }

// NewAllowlist parses two CIDR lists into an Allowlist; an empty allowed list is accepted here and
// denies everything.
func NewAllowlist(allowed, denied []string) (*Allowlist, error) {
	allowedPrefixes, err := parsePrefixes("allowedCidrs", allowed)
	if err != nil {
		return nil, err
	}
	deniedPrefixes, err := parsePrefixes("deniedCidrs", denied)
	if err != nil {
		return nil, err
	}
	return &Allowlist{allowed: allowedPrefixes, denied: deniedPrefixes}, nil
}

func parsePrefixes(field string, raw []string) ([]netip.Prefix, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	out := make([]netip.Prefix, 0, len(raw))
	for i, s := range raw {
		p, err := netip.ParsePrefix(strings.TrimSpace(s))
		if err != nil {
			return nil, fmt.Errorf("%s[%d] %q is not a valid CIDR: %w", field, i, s, err)
		}
		if p.Addr().Is4In6() {
			return nil, fmt.Errorf(
				"%s[%d] %q is an IPv4-mapped IPv6 prefix; write IPv4 ranges in dotted form (10.0.0.0/8)",
				field, i, s)
		}
		// Masked() zeroes the host bits so "10.0.0.1/8" and "10.0.0.0/8" behave
		// identically instead of depending on how the operator typed it.
		out = append(out, p.Masked())
	}
	return out, nil
}

// Allow reports whether addr is permitted; an address in NO allowed prefix is denied: the allowlist
// is exhaustive.
func (a *Allowlist) Allow(addr netip.Addr) bool {
	if a == nil {
		return false
	}
	if !addr.IsValid() || addr.Zone() != "" {
		return false
	}
	ip := addr.Unmap()
	if !ip.IsValid() || ip.Zone() != "" {
		return false
	}
	for _, p := range a.denied {
		if p.Contains(ip) {
			return false
		}
	}
	for _, p := range a.allowed {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}

// AllowHostPort resolves host (via the supplied resolver, so tests need no DNS) and reports allowed
// only when EVERY resolved address passes.
func (a *Allowlist) AllowHostPort(ctx context.Context, r Resolver, host string) error {
	_, err := a.ResolveAllowed(ctx, r, host)
	return err
}

// ResolveAllowed is AllowHostPort with the approved addresses handed back.
func (a *Allowlist) ResolveAllowed(ctx context.Context, r Resolver, host string) ([]netip.Addr, error) {
	if a == nil {
		return nil, ErrNoAllowlist
	}
	h := strings.TrimSpace(host)
	if h == "" {
		return nil, ErrNoDestination
	}

	// A literal address is checked as typed and never sent to DNS: resolving it would be a pointless
	// round trip and.
	if lit, ok := parseLiteral(h); ok {
		if err := a.check(lit); err != nil {
			return nil, err
		}
		return []netip.Addr{lit.Unmap()}, nil
	}

	if r == nil {
		return nil, ErrNoAllowlist
	}

	// "ip" asks for both families: a name that resolves into a forbidden range
	// on either one must be caught, so neither may be skipped.
	addrs, err := r.LookupNetIP(ctx, "ip", h)
	if err != nil {
		// The resolver's error embeds the queried name; it is dropped here and
		// only the generic denial travels back to the controller.
		return nil, ErrResolutionFailed
	}
	if len(addrs) == 0 {
		return nil, ErrNoAddresses
	}

	approved := make([]netip.Addr, 0, len(addrs))
	for _, addr := range addrs {
		if checkErr := a.check(addr); checkErr != nil {
			return nil, checkErr
		}
		approved = append(approved, addr.Unmap())
	}
	return approved, nil
}

// check turns Allow's boolean into the family-named refusal the caller reports.
func (a *Allowlist) check(addr netip.Addr) error {
	if !addr.IsValid() {
		return ErrNoDestination
	}
	if addr.Zone() != "" {
		return ErrDeniedZoneScoped
	}
	if a.Allow(addr) {
		return nil
	}
	if addr.Unmap().Is4() {
		return ErrDeniedIPv4
	}
	return ErrDeniedIPv6
}

// parseLiteral recognises a host that is itself an IP address, tolerating the
// bracketed IPv6 spelling.
func parseLiteral(host string) (netip.Addr, bool) {
	s := host
	if len(s) > 1 && s[0] == '[' && s[len(s)-1] == ']' {
		s = s[1 : len(s)-1]
	}
	addr, err := netip.ParseAddr(s)
	if err != nil {
		return netip.Addr{}, false
	}
	return addr, true
}
