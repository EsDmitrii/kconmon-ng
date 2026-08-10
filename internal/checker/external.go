package checker

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/EsDmitrii/kconmon-ng/internal/model"
)

// CheckExternal is the result type of the CONTINUOUS external checker; the constant itself now
// lives in internal/model beside the other check types.
const CheckExternal = model.CheckExternal

const (
	// ExternalTick is the scheduler cadence of the external checker.
	ExternalTick = 5 * time.Second

	// externalDefaultTimeout bounds one probe when the spec carries no timeout.
	externalDefaultTimeout = 5 * time.Second

	// externalDefaultTCPPort mirrors the on-demand path's default
	// (agent.defaultExternalTCPPort): an external host is not a kconmon agent,
	// so the agent's own HTTP port would be meaningless there.
	externalDefaultTCPPort = 80

	// externalDefaultDNSPort is the port an external DNS probe queries when the
	// target carries no explicit port.
	externalDefaultDNSPort = 53

	// externalDenyLogInterval is the per-target suppression window for refusal logs.
	externalDenyLogInterval = 5 * time.Minute

	// externalProbeConcurrency bounds simultaneous probes inside ONE Check call; targets are probed in
	// parallel because a Check runs them.
	externalProbeConcurrency = 8
)

// ExternalDetails is one target's outcome inside an external CheckResult.
type ExternalDetails = model.ExternalDetails

// ExternalCount is one target's cumulative accounting.
type ExternalCount struct {
	DefinitionID string
	Name         string
	Type         model.CheckType
	// Probes counts every attempt, including refused ones.
	Probes uint64
	// Failures counts attempts that reached the network and failed. A refusal
	// is NOT a failure: it never reached the network.
	Failures uint64
	// Denied counts attempts refused by the allowlist.
	Denied uint64
}

// ExternalSpecInput is the wire-shaped, UNVALIDATED form of one continuous external check; it
// exists so that validation lives next to the code that probes.
type ExternalSpecInput struct {
	DefinitionID string
	Name         string
	Address      string
	Port         uint32
	CheckType    string
	Interval     time.Duration
	Timeout      time.Duration
	ParamsJSON   []byte
}

// ExternalSpec is one validated continuous external check. It is only ever
// produced by ParseExternalSpec: the check-type params are unexported so a
// caller cannot hand the checker a spec that skipped validation.
type ExternalSpec struct {
	DefinitionID string
	Name         string
	Address      string
	Port         int
	Type         model.CheckType
	Interval     time.Duration
	Timeout      time.Duration

	httpParams externalHTTPParams
	dnsParams  externalDNSParams
	// httpHost/httpPort are the URL's host and dial port, resolved once at
	// parse time. httpHost is what the allowlist authorises and what TLS
	// verifies against; httpPort is what the approved address is dialled on.
	httpHost string
	httpPort string
}

// externalHTTPParams is the params_json schema of an http external check.
type externalHTTPParams struct {
	InsecureSkipVerify bool   `json:"insecureSkipVerify"`
	Method             string `json:"method"`
	ExpectStatus       int    `json:"expectStatus"`
}

// externalDNSParams is the params_json schema of a dns external check.
type externalDNSParams struct {
	Query string `json:"query"`
}

// externalTargetState is a target's mutable per-interval bookkeeping. It is
// keyed by externalStateKey and survives an assignment swap.
type externalTargetState struct {
	lastProbe time.Time

	probes   uint64
	failures uint64
	denied   uint64

	// denyLogAt is when this target's refusal was last logged; zero means the
	// next refusal logs immediately. denySuppressed counts refusals swallowed
	// since then, so the next log can say how many.
	denyLogAt      time.Time
	denySuppressed uint64
}

// ExternalChecker probes operator-assigned destinations that are NOT registered peers; authorising
// once at assignment time would be a DNS rebinding hole.
type ExternalChecker struct {
	allowlist *Allowlist
	resolver  Resolver
	// authTimeout bounds resolution-and-authorisation of ONE destination, so a
	// hung resolver cannot hold a probe slot for the whole tick. It does not
	// bound the probe, which has the spec's own timeout.
	authTimeout time.Duration

	mu    sync.Mutex
	specs []ExternalSpec
	state map[string]*externalTargetState

	// now is the clock used for SCHEDULING decisions (is a target due, is its
	// refusal log suppressed). Probe durations are measured with time.Now so a
	// test clock cannot invent latency. Seam for tests only.
	now func() time.Time
	// ping performs the ICMP echo; it defaults to the agent's own ICMPChecker.
	ping func(ctx context.Context, timeout time.Duration, ip string) model.CheckResult
	// lookup performs the DNS query against an already-approved resolver
	// address. Seam for tests only.
	lookup func(ctx context.Context, serverAddr, query string) ([]netip.Addr, error)
}

// NewExternalChecker builds a checker with an empty assignment. allowlist and
// resolver are the same gate the on-demand path uses; a nil allowlist denies
// everything, which is the correct behaviour for an agent that never opted in.
func NewExternalChecker(allowlist *Allowlist, resolver Resolver, authTimeout time.Duration) *ExternalChecker {
	return &ExternalChecker{
		allowlist:   allowlist,
		resolver:    resolver,
		authTimeout: authTimeout,
		state:       make(map[string]*externalTargetState),
		now:         time.Now,
		ping:        defaultExternalPing,
		lookup:      defaultExternalLookup,
	}
}

func (c *ExternalChecker) Name() model.CheckType { return CheckExternal }

// SetSpecs replaces the whole target list; assignments are absolute, never deltas, so a swap is a
// replacement and not a merge.
func (c *ExternalChecker) SetSpecs(specs []ExternalSpec) {
	c.mu.Lock()
	defer c.mu.Unlock()

	next := make(map[string]*externalTargetState, len(specs))
	kept := make([]ExternalSpec, 0, len(specs))
	for i := range specs {
		key := externalStateKey(&specs[i])
		if _, dup := next[key]; dup {
			slog.Warn("dropping duplicate external check spec", "target", specs[i].Name, "checkType", specs[i].Type)
			continue
		}
		st, ok := c.state[key]
		if !ok {
			st = &externalTargetState{}
		}
		next[key] = st
		kept = append(kept, specs[i])
	}

	c.specs = kept
	c.state = next
}

// SpecCount reports how many targets are currently assigned.
func (c *ExternalChecker) SpecCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.specs)
}

// Counts returns a snapshot of per-target accounting in assignment order.
func (c *ExternalChecker) Counts() []ExternalCount {
	c.mu.Lock()
	defer c.mu.Unlock()

	out := make([]ExternalCount, 0, len(c.specs))
	for i := range c.specs {
		st := c.state[externalStateKey(&c.specs[i])]
		if st == nil {
			continue
		}
		out = append(out, ExternalCount{
			DefinitionID: c.specs[i].DefinitionID,
			Name:         c.specs[i].Name,
			Type:         c.specs[i].Type,
			Probes:       st.probes,
			Failures:     st.failures,
			Denied:       st.denied,
		})
	}
	return out
}

// Check probes every assigned target whose own interval has elapsed and reports one ExternalDetails
// per probed target.
func (c *ExternalChecker) Check(ctx context.Context, _ Target) model.CheckResult {
	result := model.CheckResult{
		Type:      CheckExternal,
		Timestamp: time.Now(),
		Success:   true,
	}

	specs, states := c.due()
	if len(specs) == 0 {
		return result
	}

	details := make([]ExternalDetails, len(specs))
	sem := make(chan struct{}, externalProbeConcurrency)
	var wg sync.WaitGroup
	for i := range specs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				details[i] = ExternalDetails{
					DefinitionID: specs[i].DefinitionID,
					Name:         specs[i].Name,
					CheckType:    specs[i].Type,
					Error:        "external probe cancelled",
				}
				return
			}
			defer func() { <-sem }()
			details[i] = c.probe(ctx, &specs[i], states[i])
		}(i)
	}
	wg.Wait()

	for i := range details {
		if !details[i].Success && result.Error == "" {
			// The target NAME is the one operator-chosen string that is allowed
			// to travel (it is already a metric label by design); the address
			// and the refusal detail stay in the local log.
			result.Error = fmt.Sprintf("external check %q failed", details[i].Name)
			result.Success = false
		}
	}

	result.Duration = details[0].Duration
	result.Details = details
	return result
}

// due snapshots the targets whose interval has elapsed and stamps their last probe time; stamping
// BEFORE the probe rather than after keeps the cadence fixed instead of drifting by each probe's
// duration.
func (c *ExternalChecker) due() (specs []ExternalSpec, states []*externalTargetState) {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := c.now()
	specs = make([]ExternalSpec, 0, len(c.specs))
	states = make([]*externalTargetState, 0, len(c.specs))
	for i := range c.specs {
		st := c.state[externalStateKey(&c.specs[i])]
		if st == nil {
			continue
		}
		if !st.lastProbe.IsZero() && now.Sub(st.lastProbe) < c.specs[i].Interval-ExternalTick/2 {
			continue
		}
		st.lastProbe = now
		specs = append(specs, c.specs[i])
		states = append(states, st)
	}
	return specs, states
}

// probe authorises one destination and runs its check. Authorisation happens
// here, inside the probe, on every single invocation -- never at assignment
// time.
func (c *ExternalChecker) probe(ctx context.Context, spec *ExternalSpec, st *externalTargetState) ExternalDetails {
	detail := ExternalDetails{
		DefinitionID: spec.DefinitionID,
		Name:         spec.Name,
		CheckType:    spec.Type,
	}

	// For http the authorised host is the URL's hostname, not the whole URL.
	host := spec.Address
	if spec.Type == model.CheckHTTP {
		host = spec.httpHost
	}

	authCtx := ctx
	if c.authTimeout > 0 {
		var cancel context.CancelFunc
		authCtx, cancel = context.WithTimeout(ctx, c.authTimeout)
		defer cancel()
	}

	approved, err := c.allowlist.ResolveAllowed(authCtx, c.resolver, host)
	if err != nil {
		detail.Denied = true
		detail.DenyReason = externalDenyReason(err)
		detail.Error = err.Error()
		c.recordDenial(spec, st, err)
		return detail
	}

	probeCtx, cancel := context.WithTimeout(ctx, spec.Timeout)
	defer cancel()

	// Every returned address passed the allowlist, so the first is authorised
	// and is what gets dialled. Nothing below resolves the hostname a second
	// time, which is what closes the TOCTOU window.
	addr := approved[0]

	switch spec.Type {
	case model.CheckTCP:
		detail.Duration, err = probeExternalTCP(probeCtx, net.JoinHostPort(addr.String(), strconv.Itoa(spec.Port)))
	case model.CheckICMP:
		res := c.ping(probeCtx, spec.Timeout, addr.String())
		detail.Duration = res.Duration
		if d, ok := res.Details.(*model.ICMPDetails); ok {
			detail.RTT = d.RTT
			detail.LossRatio = d.LossRatio
		}
		if !res.Success {
			if res.Error == "" {
				res.Error = "icmp probe failed"
			}
			err = errors.New(res.Error)
		}
	case model.CheckDNS:
		var ips []netip.Addr
		start := time.Now()
		ips, err = c.lookup(probeCtx, net.JoinHostPort(addr.String(), strconv.Itoa(spec.Port)), spec.dnsParams.Query)
		detail.Duration = time.Since(start)
		detail.ResolvedIPs = len(ips)
		if err == nil && len(ips) == 0 {
			err = errors.New("resolver returned no addresses")
		}
	case model.CheckHTTP:
		detail.StatusCode, detail.Duration, err = c.probeHTTP(probeCtx, spec, addr)
	case model.CheckUDP, model.CheckMTR, model.CheckExternal:
		// Unreachable: ParseExternalSpec refuses all three and the controller never assigns them (see the
		// ExternalCheckSpec proto comment).
		err = fmt.Errorf("check type %q is not valid for a continuous external check", spec.Type)
	default:
		err = fmt.Errorf("check type %q is not valid for a continuous external check", spec.Type)
	}

	if err != nil {
		detail.Error = err.Error()
	} else {
		detail.Success = true
	}
	c.recordProbe(st, detail.Success)
	return detail
}

// probeExternalTCP is a PLAIN connect: open the socket, close it, report how long it took.
func probeExternalTCP(ctx context.Context, addr string) (time.Duration, error) {
	var dialer net.Dialer
	start := time.Now()
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	elapsed := time.Since(start)
	if err != nil {
		return elapsed, fmt.Errorf("tcp connect failed: %w", err)
	}
	_ = conn.Close()
	return elapsed, nil
}

// probeHTTP runs the http probe against the APPROVED address while keeping the URL's hostname for
// TLS verification and the Host header; the client is built per probe rather than shared.
func (c *ExternalChecker) probeHTTP(ctx context.Context, spec *ExternalSpec, addr netip.Addr) (int, time.Duration, error) {
	dialAddr := net.JoinHostPort(addr.String(), spec.httpPort)

	client := &http.Client{
		Timeout: spec.Timeout,
		// Redirects are NOT followed. A 3xx is reported as the status it is:
		// following one would probe a destination the operator never assigned
		// and the allowlist never saw.
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
		Transport: &http.Transport{
			DisableKeepAlives: true,
			DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
				// The requested address is discarded: this dials exactly what
				// the allowlist approved, so no second resolution happens
				// between the decision and the connection.
				var d net.Dialer
				return d.DialContext(ctx, network, dialAddr)
			},
			TLSClientConfig: &tls.Config{
				MinVersion: tls.VersionTLS12,
				// Overriding DialContext does not weaken it: the transport still derives ServerName from the
				// URL's hostname.
				InsecureSkipVerify: spec.httpParams.InsecureSkipVerify, //nolint:gosec // G402: per-target opt-in, default false
			},
		},
	}

	req, err := http.NewRequestWithContext(ctx, spec.httpParams.Method, spec.Address, http.NoBody)
	if err != nil {
		return 0, 0, fmt.Errorf("building request: %w", err)
	}
	req.Header.Set("User-Agent", "kconmon-ng")
	req.Header.Set("Connection", "close")

	start := time.Now()
	resp, err := client.Do(req) //nolint:gosec // G704: SSRF by design -- the destination is allowlist-approved
	elapsed := time.Since(start)
	if err != nil {
		return 0, elapsed, fmt.Errorf("http request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))

	if want := spec.httpParams.ExpectStatus; want != 0 {
		if resp.StatusCode != want {
			return resp.StatusCode, elapsed, fmt.Errorf("unexpected status %d, want %d", resp.StatusCode, want)
		}
		return resp.StatusCode, elapsed, nil
	}
	if resp.StatusCode >= http.StatusBadRequest {
		return resp.StatusCode, elapsed, fmt.Errorf("unexpected status %d", resp.StatusCode)
	}
	return resp.StatusCode, elapsed, nil
}

// defaultExternalPing reuses the agent's own ICMP checker verbatim.
func defaultExternalPing(ctx context.Context, timeout time.Duration, ip string) model.CheckResult {
	return NewICMPChecker(timeout).Check(ctx, Target{PodIP: ip})
}

// defaultExternalLookup queries the APPROVED resolver address for query; the dialler ignores the
// address the Go resolver computes and uses the authorised.
func defaultExternalLookup(ctx context.Context, serverAddr, query string) ([]netip.Addr, error) {
	r := &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, serverAddr)
		},
	}
	addrs, err := r.LookupNetIP(ctx, "ip", query)
	if err != nil {
		return nil, fmt.Errorf("dns lookup failed: %w", err)
	}
	return addrs, nil
}

// externalDenyReason classifies a refusal into the closed set the external_denied_total label
// carries; it matches on the SENTINEL errors, never on their text.
func externalDenyReason(err error) model.ExternalDenyReason {
	switch {
	case errors.Is(err, ErrNoAllowlist):
		return model.ExternalDenyDisabled
	case errors.Is(err, ErrResolutionFailed),
		errors.Is(err, ErrNoAddresses),
		errors.Is(err, ErrNoDestination):
		return model.ExternalDenyResolve
	default:
		// ErrDeniedIPv4, ErrDeniedIPv6, ErrDeniedZoneScoped.
		return model.ExternalDenyCIDR
	}
}

// recordDenial counts a refusal and logs it AT MOST once per
// externalDenyLogInterval per target. The count is always exact; only the log
// is rate limited, and the next log line carries the number it swallowed.
func (c *ExternalChecker) recordDenial(spec *ExternalSpec, st *externalTargetState, reason error) {
	c.mu.Lock()
	st.probes++
	st.denied++
	now := c.now()
	shouldLog := st.denyLogAt.IsZero() || now.Sub(st.denyLogAt) >= externalDenyLogInterval
	suppressed := st.denySuppressed
	if shouldLog {
		st.denyLogAt = now
		st.denySuppressed = 0
	} else {
		st.denySuppressed++
	}
	c.mu.Unlock()

	if !shouldLog {
		return
	}
	slog.Warn("external destination refused, skipping probe",
		"target", spec.Name,
		"definitionId", spec.DefinitionID,
		"checkType", spec.Type,
		"address", spec.Address,
		"reason", reason,
		"suppressed", suppressed,
	)
}

// recordProbe counts an attempt that reached the network; it also clears the refusal-log
// suppression: the destination authorised this time.
func (c *ExternalChecker) recordProbe(st *externalTargetState, success bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	st.probes++
	if !success {
		st.failures++
	}
	st.denyLogAt = time.Time{}
	st.denySuppressed = 0
}

// externalStateKey identifies a target across assignments. The controller's
// definition ID is the intended key; the tuple is the fallback for a spec that
// arrives without one, so two targets differing only by port stay distinct.
func externalStateKey(spec *ExternalSpec) string {
	if spec.DefinitionID != "" {
		return "def:" + spec.DefinitionID
	}
	return strings.Join([]string{string(spec.Type), spec.Name, spec.Address, strconv.Itoa(spec.Port)}, "|")
}

// ParseExternalSpec validates one assigned check and returns its agent-side form; every failure is
// a reason to DROP THAT SPEC, never to fail the whole assignment.
func ParseExternalSpec(in *ExternalSpecInput) (ExternalSpec, error) {
	spec := ExternalSpec{
		DefinitionID: strings.TrimSpace(in.DefinitionID),
		Name:         strings.TrimSpace(in.Name),
		Address:      strings.TrimSpace(in.Address),
		Interval:     in.Interval,
		Timeout:      in.Timeout,
	}

	if spec.Name == "" {
		return ExternalSpec{}, fmt.Errorf("target name is empty")
	}
	if spec.Address == "" {
		return ExternalSpec{}, fmt.Errorf("target address is empty")
	}
	if in.Port > 65535 {
		return ExternalSpec{}, fmt.Errorf("target port is out of range")
	}
	if spec.Interval <= 0 {
		return ExternalSpec{}, fmt.Errorf("interval must be positive")
	}
	if spec.Timeout <= 0 {
		spec.Timeout = externalDefaultTimeout
	}
	// The agent cannot probe faster than its own scheduler tick, so an interval
	// below it is clamped rather than silently honoured as "every tick" -- the
	// clamp is what the effective cadence actually is.
	if spec.Interval < ExternalTick {
		spec.Interval = ExternalTick
	}

	switch model.CheckType(in.CheckType) {
	case model.CheckTCP:
		spec.Type = model.CheckTCP
		spec.Port = int(in.Port)
		if spec.Port == 0 {
			spec.Port = externalDefaultTCPPort
		}
		if err := decodeExternalParams(in.ParamsJSON, &struct{}{}, spec.Name, in.CheckType); err != nil {
			return ExternalSpec{}, err
		}
	case model.CheckICMP:
		spec.Type = model.CheckICMP
		// ICMP has no ports; an explicit one is ignored rather than refused.
		if err := decodeExternalParams(in.ParamsJSON, &struct{}{}, spec.Name, in.CheckType); err != nil {
			return ExternalSpec{}, err
		}
	case model.CheckDNS:
		spec.Type = model.CheckDNS
		spec.Port = int(in.Port)
		if spec.Port == 0 {
			spec.Port = externalDefaultDNSPort
		}
		if err := decodeExternalParams(in.ParamsJSON, &spec.dnsParams, spec.Name, in.CheckType); err != nil {
			return ExternalSpec{}, err
		}
		spec.dnsParams.Query = strings.TrimSpace(spec.dnsParams.Query)
		if spec.dnsParams.Query == "" {
			return ExternalSpec{}, fmt.Errorf(`dns params require a non-empty "query"`)
		}
	case model.CheckHTTP:
		spec.Type = model.CheckHTTP
		if err := decodeExternalParams(in.ParamsJSON, &spec.httpParams, spec.Name, in.CheckType); err != nil {
			return ExternalSpec{}, err
		}
		if err := validateExternalHTTP(&spec); err != nil {
			return ExternalSpec{}, err
		}
	case model.CheckUDP, model.CheckMTR, model.CheckExternal:
		// The controller rejects all of these at the PUT (validExternalCheckTypes), so they should never
		// arrive.
		return ExternalSpec{}, fmt.Errorf("check type %q is not valid for a continuous external check", in.CheckType)
	default:
		return ExternalSpec{}, fmt.Errorf("unknown check type %q", in.CheckType)
	}

	return spec, nil
}

// validateExternalHTTP checks the URL and the http params; the dial port comes from the URL (or its
// scheme's default).
func validateExternalHTTP(spec *ExternalSpec) error {
	u, err := url.Parse(spec.Address)
	if err != nil {
		return fmt.Errorf("address is not a valid URL")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("http target address must be an http:// or https:// URL")
	}
	if u.Hostname() == "" {
		return fmt.Errorf("http target address has no host")
	}
	spec.httpHost = u.Hostname()
	spec.httpPort = u.Port()
	if spec.httpPort == "" {
		if u.Scheme == "https" {
			spec.httpPort = "443"
		} else {
			spec.httpPort = "80"
		}
	}

	if spec.httpParams.Method == "" {
		spec.httpParams.Method = http.MethodGet
	}
	spec.httpParams.Method = strings.ToUpper(spec.httpParams.Method)
	if spec.httpParams.Method != http.MethodGet && spec.httpParams.Method != http.MethodHead {
		return fmt.Errorf(`http params "method" must be GET or HEAD`)
	}

	if s := spec.httpParams.ExpectStatus; s != 0 && (s < 100 || s > 599) {
		return fmt.Errorf(`http params "expectStatus" must be 0 or a valid HTTP status`)
	}
	return nil
}

// decodeExternalParams decodes params_json into dst; the warn fires at ASSIGNMENT time, once per
// spec, never per probe.
func decodeExternalParams(raw []byte, dst any, name, checkType string) error {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return nil
	}

	strict := json.NewDecoder(bytes.NewReader(trimmed))
	strict.DisallowUnknownFields()
	if err := strict.Decode(dst); err != nil {
		if lenientErr := json.Unmarshal(trimmed, dst); lenientErr != nil {
			return fmt.Errorf("params are not valid for check type %q: %w", checkType, lenientErr)
		}
		slog.Warn("ignoring unknown keys in external check params",
			"target", name, "checkType", checkType, "detail", err)
	}
	return nil
}
