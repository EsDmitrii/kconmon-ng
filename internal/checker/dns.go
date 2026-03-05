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
}

func NewDNSChecker(hosts, resolvers []string) *DNSChecker {
	return &DNSChecker{
		hosts:     hosts,
		resolvers: resolvers,
	}
}

func (c *DNSChecker) Name() model.CheckType {
	return model.CheckDNS
}

func (c *DNSChecker) Check(ctx context.Context, _ Target) model.CheckResult {
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
			resolverAddr := net.JoinHostPort(resolverIP, "53")
			resolver := &net.Resolver{
				PreferGo: true,
				Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
					d := net.Dialer{Timeout: 5 * time.Second}
					return d.DialContext(ctx, "udp", resolverAddr)
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

	start := time.Now()
	ips, err := resolver.LookupIPAddr(ctx, host)
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
