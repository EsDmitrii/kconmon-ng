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

// NewHeader returns an Authenticator for auth.mode=header (trusted-proxy header auth); trust is
// decided on r.RemoteAddr ONLY -- the TCP peer that actually dialed this process.
func NewHeader(cfg config.HeaderConfig) Authenticator {
	h := &headerAuthenticator{
		userHeader:      cfg.UserHeader,
		groupsHeader:    cfg.GroupsHeader,
		groupsDelimiter: cfg.GroupsDelimiter,
	}
	for _, raw := range cfg.TrustedProxyCIDRs {
		_, ipnet, err := net.ParseCIDR(raw)
		if err != nil {
			// config.Config.Validate already rejects an unparseable CIDR at boot for any config that goes
			// through Load.
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

	// A well-behaved trusted proxy sets each identity header exactly once per request.
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

// isTrustedPeer parses remoteAddr's host.
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

// parseGroups splits raw on the configured delimiter (falling back to "," if the delimiter was left
// empty, matching config.defaults).
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
