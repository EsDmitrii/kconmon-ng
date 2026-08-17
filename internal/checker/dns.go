package checker

import (
	"context"
	"fmt"
	"net"
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
			detail, err := c.lookupHost(ctx, host, "", net.DefaultResolver)
			if err != nil && firstErr == "" {
				firstErr = fmt.Sprintf("DNS resolve %s via system: %v", host, err)
			}
			allResults = append(allResults, detail)
			continue
		}

		for _, resolverIP := range c.resolvers {
			resolverAddr := resolverDialAddr(resolverIP)
			resolver := &net.Resolver{
				PreferGo: true,
				/* The NETWORK Go asks for, not a hardcoded "udp".
				   The resolver retries a truncated answer over TCP — that is what the TC bit is for —
				   and this dialler sent the retry back over UDP, where it was truncated again. A host
				   with more records than fit in 512 bytes (a large A set, a long CNAME chain) was
				   therefore unresolvable through an explicit resolver, and reported as a DNS failure
				   of the network rather than of this dialler. */
				Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
					d := net.Dialer{Timeout: c.timeout}
					return d.DialContext(ctx, network, resolverAddr)
				},
			}
			detail, err := c.lookupHost(ctx, host, resolverIP, resolver)
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

func (c *DNSChecker) lookupHost(ctx context.Context, host, resolverLabel string, resolver *net.Resolver) (model.DNSDetails, error) {
	if resolverLabel == "" {
		resolverLabel = "system"
	}

	detail := model.DNSDetails{
		Host:     host,
		Resolver: resolverLabel,
	}

	/* checkers.dns.timeout bounds the LOOKUP, which it did not before. It was applied to exactly one
	   thing — the UDP dial on the explicit-resolver path, which is connectionless and returns at once
	   — so on the default path (resolvers: []) it was not applied at all: the real bound was
	   /etc/resolv.conf, where kubelet's ndots:5 plus three search domains means four query names by
	   two attempts by five seconds, about forty seconds, for a check an operator had configured to
	   give up after one. */
	lctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	start := time.Now()
	ips, err := resolver.LookupIPAddr(lctx, host)
	detail.Duration = time.Since(start)

	if err != nil {
		return detail, err
	}

	resolved := make([]net.IP, 0, len(ips))
	for _, ip := range ips {
		resolved = append(resolved, ip.IP)
	}
	detail.ResolvedIPs = resolved
	return detail, nil
}
