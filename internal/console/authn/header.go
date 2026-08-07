package authn

import (
	"net"
	"net/http"
	"strings"

	"github.com/EsDmitrii/kconmon-ng/internal/console/authz"
	"github.com/EsDmitrii/kconmon-ng/internal/console/config"
)

// headerAuthenticator implements NewHeader: see its doc comment.
type headerAuthenticator struct {
	userHeader      string
	groupsHeader    string
	groupsDelimiter string
	trustedCIDRs    []*net.IPNet
}

// NewHeader returns an Authenticator for auth.mode=header (trusted-proxy
// header auth). Trust is decided on r.RemoteAddr ONLY -- the TCP peer that
// actually dialed this process -- and never on X-Forwarded-For or any other
// header, because those are exactly as attacker-controlled as the identity
// headers this mode trusts once it decides to trust the peer at all. A
// request from outside cfg.TrustedProxyCIDRs is ErrNoCredentials, full stop,
// even if it carries a perfectly well-formed X-Remote-User: this is the
// entire reason config.HeaderConfig.validate makes TrustedProxyCIDRs
// mandatory and non-empty (SECURITY.md §10.1: "explicit opt-in").
func NewHeader(cfg config.HeaderConfig) Authenticator {
	h := &headerAuthenticator{
		userHeader:      cfg.UserHeader,
		groupsHeader:    cfg.GroupsHeader,
		groupsDelimiter: cfg.GroupsDelimiter,
	}
	for _, raw := range cfg.TrustedProxyCIDRs {
		_, ipnet, err := net.ParseCIDR(raw)
		if err != nil {
			// config.Config.Validate (Task 11) already rejects an
			// unparseable CIDR at boot for any config that goes through
			// Load; a value that fails to parse here only happens for a
			// hand-built config.HeaderConfig bypassing that validation
			// (e.g. in a test). Skip it rather than either trusting nothing
			// (if it were the only entry) or silently trusting every peer.
			continue
		}
		h.trustedCIDRs = append(h.trustedCIDRs, ipnet)
	}
	return h
}

func (h *headerAuthenticator) Mode() string { return "header" }

func (h *headerAuthenticator) Authenticate(r *http.Request) (authz.Subject, error) {
	if !h.isTrustedPeer(r.RemoteAddr) {
		return authz.Subject{}, ErrNoCredentials
	}

	// A well-behaved trusted proxy sets each identity header exactly once
	// per request. Two or more occurrences of the SAME header name is what
	// an append-not-overwrite proxy bug (or a client that snuck its own
	// X-Remote-User/X-Remote-Groups header in ahead of the proxy, which then
	// appended its own instead of replacing it) looks like from here: Go's
	// net/http merges repeated headers, so r.Header.Get would silently
	// return just the first value and hide the second, attacker-or-bug
	// -controlled one entirely. Treating this as "no credentials" (rather
	// than picking either value) is the only response that cannot be turned
	// into smuggling a second identity past the proxy.
	if len(r.Header.Values(h.userHeader)) > 1 || len(r.Header.Values(h.groupsHeader)) > 1 {
		return authz.Subject{}, ErrNoCredentials
	}

	user := r.Header.Get(h.userHeader)
	if user == "" {
		return authz.Subject{}, ErrNoCredentials
	}

	return authz.Subject{
		Kind:        authz.SubjectUser,
		ID:          user,
		DisplayName: user,
		Groups:      h.parseGroups(r.Header.Get(h.groupsHeader)),
	}, nil
}

// isTrustedPeer parses remoteAddr's host (net/http always sets
// r.RemoteAddr to "host:port" for a real connection; a bare host with no
// port is tolerated too, since tests often set it that way) and reports
// whether it falls inside any of trustedCIDRs. Deliberately the ONLY input
// this decision reads -- see NewHeader's doc comment.
func (h *headerAuthenticator) isTrustedPeer(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	for _, ipnet := range h.trustedCIDRs {
		if ipnet.Contains(ip) {
			return true
		}
	}
	return false
}

// parseGroups splits raw on the configured delimiter (falling back to ","
// if the delimiter was left empty, matching config.defaults()), trims
// whitespace around each entry, and drops empty entries -- so both
// "a, b, c" and "a,,b" behave sensibly. An empty raw returns nil, not
// []string{""}.
func (h *headerAuthenticator) parseGroups(raw string) []string {
	if raw == "" {
		return nil
	}
	delim := h.groupsDelimiter
	if delim == "" {
		delim = ","
	}

	fields := strings.Split(raw, delim)
	groups := make([]string, 0, len(fields))
	for _, f := range fields {
		f = strings.TrimSpace(f)
		if f != "" {
			groups = append(groups, f)
		}
	}
	return groups
}
