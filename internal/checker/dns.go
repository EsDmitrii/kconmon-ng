package checker

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"time"

	"github.com/EsDmitrii/kconmon-ng/internal/model"
)

type DNSChecker struct {
	hosts     []string
	resolvers []string
	timeout   time.Duration
}

func NewDNSChecker(hosts, resolvers []string, timeout time.Duration) *DNSChecker {
	return &DNSChecker{
		hosts:     hosts,
		resolvers: resolvers,
		timeout:   timeout,
	}
}

func (c *DNSChecker) Name() model.CheckType {
	return model.CheckDNS
}

/*
resolverDialAddr turns one configured resolver into the address the dialler gets.

checkers.dns.resolvers accepts "host" OR "host:port" — the config validator says so in a comment and
validates the port when one is there (internal/config/config.go, validateDNS). This used to be a
flat net.JoinHostPort(entry, "53"), which is right only for the first form:

  - "10.96.0.10:5353" (a NodeLocal DNS or a test resolver on a non-standard port) became
    "[10.96.0.10:5353]:53", which no dialler can resolve. Every query through that resolver failed
    forever, the DNS alert fired permanently, and the agent had never actually sent a packet.
  - IPv6 was unusable in every spelling: a bare "2001:4860:4860::8888" is refused at startup by the
    validator (SplitHostPort errors on it), and the bracketed "[2001:4860:4860::8888]:53" came out
    of JoinHostPort mangled the same way.

Splitting first and re-joining the parts handles all four spellings, and matches how the validator
already parses the same string.
*/
func resolverDialAddr(entry string) string {
	host, port, err := net.SplitHostPort(entry)
	if err != nil {
		// No port in the entry (the validator has already rejected anything malformed that carries
		// a colon), so the whole string is the host and 53 is the port.
		return net.JoinHostPort(entry, "53")
	}
	if port == "" {
		port = "53"
	}
	return net.JoinHostPort(host, port)
}

func (c *DNSChecker) Check(ctx context.Context, _ Target) model.CheckResult { //nolint:gocritic // hugeParam: Target is a VALUE by design -- a checker must not be able to mutate the caller's copy, and one 80-byte copy per probe is nothing next to the probe itself
	result := model.CheckResult{
		Type:      model.CheckDNS,
		Timestamp: time.Now(),
	}

	capacity := len(c.hosts)
	if len(c.resolvers) > 0 {
		capacity *= len(c.resolvers)
	}
	allResults := make([]model.DNSDetails, 0, capacity)
	var firstErr string

	for _, host := range c.hosts {
		if len(c.resolvers) == 0 {
			// The system path keeps /etc/hosts and the search list: it measures what the pod's own
			// lookups go through.
			detail, err := c.lookupHost(ctx, host, "system", func(ctx context.Context, name string) ([]netip.Addr, error) {
				return net.DefaultResolver.LookupNetIP(ctx, "ip", name)
			})
			if err != nil && firstErr == "" {
				firstErr = fmt.Sprintf("DNS resolve %s via system: %v", host, err)
			}
			allResults = append(allResults, detail)
			continue
		}

		for _, resolverIP := range c.resolvers {
			server := resolverDialAddr(resolverIP)
			detail, err := c.lookupHost(ctx, host, resolverIP, func(ctx context.Context, name string) ([]netip.Addr, error) {
				return queryResolver(ctx, server, name)
			})
			if err != nil && firstErr == "" {
				firstErr = fmt.Sprintf("DNS resolve %s via %s: %v", host, resolverIP, err)
			}
			allResults = append(allResults, detail)
		}
	}

	if firstErr != "" {
		result.Error = firstErr
	} else {
		result.Success = true
	}

	if len(allResults) > 0 {
		result.Duration = allResults[0].Duration
		result.Details = allResults
	}

	return result
}

func (c *DNSChecker) lookupHost(
	ctx context.Context, host, resolverLabel string, lookup func(context.Context, string) ([]netip.Addr, error),
) (model.DNSDetails, error) {
	detail := model.DNSDetails{
		Host:     host,
		Resolver: resolverLabel,
	}

	// checkers.dns.timeout bounds the whole lookup; on the system path resolv.conf's ndots and search
	// list would otherwise stretch one check to about forty seconds.
	lctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	start := time.Now()
	ips, err := lookup(lctx, host)
	detail.Duration = time.Since(start)

	if err != nil {
		return detail, err
	}

	resolved := make([]net.IP, 0, len(ips))
	for _, ip := range ips {
		resolved = append(resolved, ip.Unmap().AsSlice())
	}
	detail.ResolvedIPs = resolved
	return detail, nil
}
