package checker

import (
	"bytes"
	"context"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/EsDmitrii/kconmon-ng/internal/model"
)

// loopbackAllowlist permits only loopback, which is where every listener in
// this file lives. No test here touches a network beyond lo.
func loopbackAllowlist(t *testing.T) *Allowlist {
	t.Helper()
	return mustAllowlist(t, []string{"127.0.0.0/8", "::1/128"}, nil)
}

// literalResolver fails every lookup: the tests address their targets by IP
// literal, which ResolveAllowed never sends to DNS. A resolver that errors is
// therefore proof that nothing resolved behind our back.
type literalResolver struct{ calls atomic.Int64 }

func (r *literalResolver) LookupNetIP(_ context.Context, _, _ string) ([]netip.Addr, error) {
	r.calls.Add(1)
	return nil, ErrResolutionFailed
}

// newTestExternalChecker builds a checker with the ICMP and DNS probe seams
// stubbed out (an ICMP socket is not available in every test environment, and
// a real DNS query would need a nameserver) and a controllable clock.
func newTestExternalChecker(t *testing.T, allowlist *Allowlist) *ExternalChecker {
	t.Helper()
	c := NewExternalChecker(allowlist, &literalResolver{}, time.Second)
	c.ping = func(_ context.Context, _ time.Duration, _ string) model.CheckResult {
		return model.CheckResult{Success: true, Duration: 3 * time.Millisecond,
			Details: &model.ICMPDetails{RTT: 3 * time.Millisecond}}
	}
	c.lookup = func(_ context.Context, _, _ string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("192.0.2.10")}, nil
	}
	return c
}

// countingListener is a loopback TCP listener that counts accepted
// connections, so a test can assert a probe never dialled.
type countingListener struct {
	ln     net.Listener
	accept atomic.Int64
}

func newCountingListener(t *testing.T) *countingListener {
	t.Helper()
	var lc net.ListenConfig
	ln, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listening on loopback: %v", err)
	}
	cl := &countingListener{ln: ln}
	go func() {
		for {
			conn, acceptErr := ln.Accept()
			if acceptErr != nil {
				return
			}
			cl.accept.Add(1)
			_ = conn.Close()
		}
	}()
	t.Cleanup(func() { _ = ln.Close() })
	return cl
}

func (c *countingListener) hostPort(t *testing.T) (host string, port uint32) {
	t.Helper()
	h, p, err := net.SplitHostPort(c.ln.Addr().String())
	if err != nil {
		t.Fatalf("splitting listener address: %v", err)
	}
	n, err := strconv.Atoi(p)
	if err != nil {
		t.Fatalf("parsing listener port: %v", err)
	}
	return h, uint32(n) //nolint:gosec // G115: a listener port is always in range
}

// waitAccepts waits for the listener to have accepted want connections. The
// accept loop runs on its own goroutine, so the count lands slightly after the
// prober's dial returns.
func (c *countingListener) waitAccepts(t *testing.T, want int64) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if c.accept.Load() >= want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("listener accepted %d connections, want %d", c.accept.Load(), want)
}

func mustParseSpec(t *testing.T, in *ExternalSpecInput) ExternalSpec {
	t.Helper()
	spec, err := ParseExternalSpec(in)
	if err != nil {
		t.Fatalf("ParseExternalSpec(%+v) failed: %v", in, err)
	}
	return spec
}

// externalDetails asserts the CheckResult carries the slice shape the result
// handler expects and returns it.
func externalDetails(t *testing.T, res *model.CheckResult) []ExternalDetails {
	t.Helper()
	if res.Type != CheckExternal {
		t.Fatalf("result type = %q, want %q", res.Type, CheckExternal)
	}
	details, ok := res.Details.([]ExternalDetails)
	if !ok {
		t.Fatalf("Details must be a []ExternalDetails slice, got %T", res.Details)
	}
	return details
}

// captureLogs redirects the default slog logger into a buffer for the duration
// of the test.
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

func TestExternalCheckerTCPProbeShape(t *testing.T) {
	lis := newCountingListener(t)
	host, port := lis.hostPort(t)

	c := newTestExternalChecker(t, loopbackAllowlist(t))
	c.SetSpecs([]ExternalSpec{mustParseSpec(t, &ExternalSpecInput{
		DefinitionID: "def-tcp",
		Name:         "loopback-tcp",
		Address:      host,
		Port:         port,
		CheckType:    "tcp",
		Interval:     30 * time.Second,
		Timeout:      2 * time.Second,
	})})

	res := c.Check(context.Background(), Target{})
	details := externalDetails(t, &res)

	if !res.Success {
		t.Fatalf("expected success, got error %q", res.Error)
	}
	if len(details) != 1 {
		t.Fatalf("expected 1 detail, got %d", len(details))
	}
	d := details[0]
	if !d.Success || d.Denied {
		t.Errorf("detail should be a successful, non-denied probe: %+v", d)
	}
	if d.Name != "loopback-tcp" || d.DefinitionID != "def-tcp" || d.CheckType != model.CheckTCP {
		t.Errorf("detail identity wrong: %+v", d)
	}
	lis.waitAccepts(t, 1)
}

func TestExternalCheckerTCPProbeIsAPlainConnect(t *testing.T) {
	// A raw listener that accepts and immediately closes serves no HTTP at all.
	// TCPChecker would fail here because its probe is a GET /readyz; the
	// external TCP probe must be satisfied by the connect alone.
	lis := newCountingListener(t)
	host, port := lis.hostPort(t)

	c := newTestExternalChecker(t, loopbackAllowlist(t))
	c.SetSpecs([]ExternalSpec{mustParseSpec(t, &ExternalSpecInput{
		Name: "plain", Address: host, Port: port, CheckType: "tcp",
		Interval: 30 * time.Second, Timeout: 2 * time.Second,
	})})

	res := c.Check(context.Background(), Target{})
	if !res.Success {
		t.Fatalf("a plain connect to a bare TCP listener must succeed, got %q", res.Error)
	}
}

func TestExternalCheckerHTTPProbeShape(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	c := newTestExternalChecker(t, loopbackAllowlist(t))
	c.SetSpecs([]ExternalSpec{mustParseSpec(t, &ExternalSpecInput{
		DefinitionID: "def-http",
		Name:         "loopback-http",
		Address:      srv.URL,
		CheckType:    "http",
		Interval:     30 * time.Second,
		Timeout:      2 * time.Second,
		ParamsJSON:   []byte(`{"method":"GET","expectStatus":200}`),
	})})

	res := c.Check(context.Background(), Target{})
	details := externalDetails(t, &res)

	if !res.Success {
		t.Fatalf("expected success, got error %q", res.Error)
	}
	if details[0].StatusCode != http.StatusOK {
		t.Errorf("status code = %d, want 200", details[0].StatusCode)
	}
	if details[0].CheckType != model.CheckHTTP {
		t.Errorf("check type = %q, want http", details[0].CheckType)
	}
}

func TestExternalCheckerHTTPExpectStatusMismatchFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := newTestExternalChecker(t, loopbackAllowlist(t))
	c.SetSpecs([]ExternalSpec{mustParseSpec(t, &ExternalSpecInput{
		Name: "strict", Address: srv.URL, CheckType: "http",
		Interval: 30 * time.Second, Timeout: 2 * time.Second,
		ParamsJSON: []byte(`{"expectStatus":200}`),
	})})

	res := c.Check(context.Background(), Target{})
	details := externalDetails(t, &res)
	if res.Success {
		t.Fatal("expected failure when the status does not match expectStatus")
	}
	if details[0].StatusCode != http.StatusNoContent {
		t.Errorf("status code should still be reported on mismatch, got %d", details[0].StatusCode)
	}
}

func TestExternalCheckerDNSProbeShape(t *testing.T) {
	c := newTestExternalChecker(t, loopbackAllowlist(t))

	var gotServer, gotQuery string
	c.lookup = func(_ context.Context, serverAddr, query string) ([]netip.Addr, error) {
		gotServer, gotQuery = serverAddr, query
		return []netip.Addr{netip.MustParseAddr("192.0.2.1"), netip.MustParseAddr("192.0.2.2")}, nil
	}

	c.SetSpecs([]ExternalSpec{mustParseSpec(t, &ExternalSpecInput{
		Name: "loopback-dns", Address: "127.0.0.53", Port: 5353, CheckType: "dns",
		Interval: 30 * time.Second, Timeout: 2 * time.Second,
		ParamsJSON: []byte(`{"query":"example.com"}`),
	})})

	res := c.Check(context.Background(), Target{})
	details := externalDetails(t, &res)

	if !res.Success {
		t.Fatalf("expected success, got error %q", res.Error)
	}
	// The query must be sent to the APPROVED address, not to the hostname.
	if gotServer != "127.0.0.53:5353" {
		t.Errorf("resolver dialled %q, want the approved 127.0.0.53:5353", gotServer)
	}
	if gotQuery != "example.com" {
		t.Errorf("query = %q, want example.com", gotQuery)
	}
	if details[0].ResolvedIPs != 2 {
		t.Errorf("resolvedIps = %d, want the COUNT 2", details[0].ResolvedIPs)
	}
}

func TestExternalCheckerDNSDefaultsToPort53(t *testing.T) {
	c := newTestExternalChecker(t, loopbackAllowlist(t))
	var gotServer string
	c.lookup = func(_ context.Context, serverAddr, _ string) ([]netip.Addr, error) {
		gotServer = serverAddr
		return []netip.Addr{netip.MustParseAddr("192.0.2.1")}, nil
	}
	c.SetSpecs([]ExternalSpec{mustParseSpec(t, &ExternalSpecInput{
		Name: "dns", Address: "127.0.0.53", CheckType: "dns",
		Interval: 30 * time.Second, ParamsJSON: []byte(`{"query":"example.com"}`),
	})})

	c.Check(context.Background(), Target{})
	if gotServer != "127.0.0.53:53" {
		t.Errorf("resolver dialled %q, want the default port 53", gotServer)
	}
}

func TestExternalCheckerICMPProbeShape(t *testing.T) {
	c := newTestExternalChecker(t, loopbackAllowlist(t))

	var gotIP string
	c.ping = func(_ context.Context, _ time.Duration, ip string) model.CheckResult {
		gotIP = ip
		return model.CheckResult{Success: true, Duration: 7 * time.Millisecond,
			Details: &model.ICMPDetails{RTT: 7 * time.Millisecond, LossRatio: 0}}
	}

	c.SetSpecs([]ExternalSpec{mustParseSpec(t, &ExternalSpecInput{
		Name: "loopback-icmp", Address: "127.0.0.1", CheckType: "icmp",
		Interval: 30 * time.Second, Timeout: 2 * time.Second,
	})})

	res := c.Check(context.Background(), Target{})
	details := externalDetails(t, &res)

	if !res.Success {
		t.Fatalf("expected success, got error %q", res.Error)
	}
	if gotIP != "127.0.0.1" {
		t.Errorf("ping went to %q, want the approved 127.0.0.1", gotIP)
	}
	if details[0].RTT != 7*time.Millisecond {
		t.Errorf("rtt = %v, want 7ms", details[0].RTT)
	}
}

// A denied destination must never reach a socket. The listener exists and is
// reachable; the allowlist is what stops the probe, so an accept count above
// zero means the gate ran after the dial or not at all.
func TestExternalCheckerDeniedTargetIsNotDialled(t *testing.T) {
	lis := newCountingListener(t)
	host, port := lis.hostPort(t)

	// Loopback is deliberately NOT in the allowlist.
	c := newTestExternalChecker(t, mustAllowlist(t, []string{"10.0.0.0/8"}, nil))
	c.SetSpecs([]ExternalSpec{mustParseSpec(t, &ExternalSpecInput{
		DefinitionID: "def-denied",
		Name:         "denied-tcp", Address: host, Port: port, CheckType: "tcp",
		Interval: 30 * time.Second, Timeout: 2 * time.Second,
	})})

	res := c.Check(context.Background(), Target{})
	details := externalDetails(t, &res)

	if res.Success {
		t.Fatal("a denied target must make the check fail, not silently pass")
	}
	if !details[0].Denied {
		t.Errorf("detail must be marked Denied: %+v", details[0])
	}
	if details[0].Success {
		t.Error("a denied probe is not a success")
	}
	if lis.accept.Load() != 0 {
		t.Fatalf("denied target was dialled: listener accepted %d connections", lis.accept.Load())
	}

	counts := c.Counts()
	if len(counts) != 1 || counts[0].Denied != 1 || counts[0].Failures != 0 {
		t.Errorf("denial must be counted apart from failures, got %+v", counts)
	}
}

// The allowlist is re-evaluated on EVERY probe, never cached from the first
// one: an assignment outlives any single resolution.
func TestExternalCheckerAuthorisesOnEveryProbe(t *testing.T) {
	lis := newCountingListener(t)
	host, port := lis.hostPort(t)

	c := newTestExternalChecker(t, loopbackAllowlist(t))
	clock := time.Now()
	c.now = func() time.Time { return clock }
	c.SetSpecs([]ExternalSpec{mustParseSpec(t, &ExternalSpecInput{
		Name: "tcp", Address: host, Port: port, CheckType: "tcp",
		Interval: 30 * time.Second, Timeout: 2 * time.Second,
	})})

	for range 3 {
		c.Check(context.Background(), Target{})
		clock = clock.Add(time.Minute)
	}

	if got := c.Counts()[0].Probes; got != 3 {
		t.Fatalf("probes = %d, want 3", got)
	}
	lis.waitAccepts(t, 3)
}

// TLS verification is ON by default (Decision 10): httptest's self-signed
// certificate must be refused. The same target with insecureSkipVerify:true
// must then succeed, proving the opt-out is per target and not global.
func TestExternalCheckerHTTPSVerifiesTLSByDefault(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := newTestExternalChecker(t, loopbackAllowlist(t))
	c.SetSpecs([]ExternalSpec{mustParseSpec(t, &ExternalSpecInput{
		Name: "self-signed", Address: srv.URL, CheckType: "http",
		Interval: 30 * time.Second, Timeout: 5 * time.Second,
	})})

	res := c.Check(context.Background(), Target{})
	details := externalDetails(t, &res)
	if res.Success {
		t.Fatal("a self-signed certificate must fail with verification on by default")
	}
	if !strings.Contains(strings.ToLower(details[0].Error), "certificate") {
		t.Errorf("failure should be a certificate error, got %q", details[0].Error)
	}
	if details[0].Denied {
		t.Error("a TLS failure is a probe failure, not an allowlist denial")
	}

	c2 := newTestExternalChecker(t, loopbackAllowlist(t))
	c2.SetSpecs([]ExternalSpec{mustParseSpec(t, &ExternalSpecInput{
		Name: "self-signed", Address: srv.URL, CheckType: "http",
		Interval: 30 * time.Second, Timeout: 5 * time.Second,
		ParamsJSON: []byte(`{"insecureSkipVerify":true}`),
	})})

	res2 := c2.Check(context.Background(), Target{})
	if !res2.Success {
		t.Fatalf("insecureSkipVerify:true must accept the self-signed cert, got %q", res2.Error)
	}
}

// Dialling the approved IP must not weaken hostname verification: the
// certificate is still checked against the URL's hostname.
func TestExternalCheckerHTTPSVerifiesAgainstURLHostname(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parsing test server URL: %v", err)
	}
	// httptest's certificate is valid for 127.0.0.1 and example.com; "localhost"
	// resolves to loopback but is not on it, so verification must reject it even
	// though the connection itself succeeds.
	hostnameURL := "https://localhost:" + u.Port()

	al := loopbackAllowlist(t)
	c := NewExternalChecker(al, &staticResolver{
		hosts: map[string][]netip.Addr{"localhost": {netip.MustParseAddr("127.0.0.1")}},
	}, time.Second)
	c.SetSpecs([]ExternalSpec{mustParseSpec(t, &ExternalSpecInput{
		Name: "hostname", Address: hostnameURL, CheckType: "http",
		Interval: 30 * time.Second, Timeout: 5 * time.Second,
	})})

	res := c.Check(context.Background(), Target{})
	details := externalDetails(t, &res)
	if res.Success {
		t.Fatal("certificate must be verified against the URL hostname, not the dialled IP")
	}
	if !strings.Contains(strings.ToLower(details[0].Error), "certificate") {
		t.Errorf("failure must be a certificate name mismatch, got %q", details[0].Error)
	}
}

// staticResolver answers from a fixed table, so the allowlist path can be
// exercised for a NAME without touching DNS.
type staticResolver struct{ hosts map[string][]netip.Addr }

func (s *staticResolver) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	addrs, ok := s.hosts[host]
	if !ok {
		return nil, ErrResolutionFailed
	}
	return addrs, nil
}

func TestExternalCheckerEmptyAssignmentIsNoOp(t *testing.T) {
	c := newTestExternalChecker(t, loopbackAllowlist(t))

	res := c.Check(context.Background(), Target{})
	if !res.Success {
		t.Errorf("an empty assignment is not a failure, got error %q", res.Error)
	}
	if res.Error != "" {
		t.Errorf("expected no error, got %q", res.Error)
	}
	if res.Details != nil {
		t.Errorf("expected no details, got %v", res.Details)
	}
	if res.Type != CheckExternal {
		t.Errorf("type = %q, want %q", res.Type, CheckExternal)
	}

	// An assignment that is explicitly emptied again is still a no-op.
	c.SetSpecs(nil)
	if c.SpecCount() != 0 {
		t.Errorf("SpecCount = %d, want 0", c.SpecCount())
	}
}

// The swap replaces the target list under the checker's mutex while probes are
// in flight. Run under -race; any unsynchronised access shows up here.
func TestExternalCheckerAssignmentSwapIsRaceFree(t *testing.T) {
	lis := newCountingListener(t)
	host, port := lis.hostPort(t)

	c := newTestExternalChecker(t, loopbackAllowlist(t))

	specFor := func(name string) ExternalSpec {
		return mustParseSpec(t, &ExternalSpecInput{
			DefinitionID: name,
			Name:         name, Address: host, Port: port, CheckType: "tcp",
			Interval: 5 * time.Second, Timeout: 2 * time.Second,
		})
	}
	c.SetSpecs([]ExternalSpec{specFor("a"), specFor("b")})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 25 {
				c.Check(ctx, Target{})
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := range 50 {
			if i%2 == 0 {
				c.SetSpecs([]ExternalSpec{specFor("a")})
			} else {
				c.SetSpecs([]ExternalSpec{specFor("a"), specFor("b"), specFor("c")})
			}
			_ = c.Counts()
		}
	}()
	wg.Wait()
}

// A target's own interval gates it: the checker runs on a fixed scheduler tick
// but a 60s target must not be probed on every tick.
func TestExternalCheckerPerTargetIntervalHonored(t *testing.T) {
	lis := newCountingListener(t)
	host, port := lis.hostPort(t)

	c := newTestExternalChecker(t, loopbackAllowlist(t))
	clock := time.Now()
	c.now = func() time.Time { return clock }

	c.SetSpecs([]ExternalSpec{
		mustParseSpec(t, &ExternalSpecInput{
			DefinitionID: "slow", Name: "slow", Address: host, Port: port, CheckType: "tcp",
			Interval: 60 * time.Second, Timeout: 2 * time.Second,
		}),
		mustParseSpec(t, &ExternalSpecInput{
			DefinitionID: "fast", Name: "fast", Address: host, Port: port, CheckType: "tcp",
			Interval: ExternalTick, Timeout: 2 * time.Second,
		}),
	})

	first := c.Check(context.Background(), Target{})
	if len(externalDetails(t, &first)) != 2 {
		t.Fatalf("first tick must probe both targets, got %d", len(externalDetails(t, &first)))
	}

	// Second tick, immediately: neither interval has elapsed for the slow
	// target, and the fast one is not due either until a tick has passed.
	second := c.Check(context.Background(), Target{})
	if second.Details != nil {
		t.Fatalf("no target is due on an immediate second call, got %v", second.Details)
	}
	if !second.Success {
		t.Errorf("a tick with nothing due is a no-op, got error %q", second.Error)
	}

	// One tick later only the fast target is due.
	clock = clock.Add(ExternalTick)
	third := c.Check(context.Background(), Target{})
	d3 := externalDetails(t, &third)
	if len(d3) != 1 || d3[0].Name != "fast" {
		t.Fatalf("only the 5s target is due after one tick, got %+v", d3)
	}

	counts := c.Counts()
	if counts[0].Name != "slow" || counts[0].Probes != 1 {
		t.Errorf("60s target must be probed exactly once across three ticks, got %+v", counts[0])
	}
	if counts[1].Name != "fast" || counts[1].Probes != 2 {
		t.Errorf("5s target must be probed twice, got %+v", counts[1])
	}
}

// State is keyed per target and carried across an identical re-push, so the
// Console's reconcile ticker does not reset every interval.
func TestExternalCheckerSwapPreservesPerTargetInterval(t *testing.T) {
	lis := newCountingListener(t)
	host, port := lis.hostPort(t)

	c := newTestExternalChecker(t, loopbackAllowlist(t))
	clock := time.Now()
	c.now = func() time.Time { return clock }

	spec := mustParseSpec(t, &ExternalSpecInput{
		DefinitionID: "def-1", Name: "slow", Address: host, Port: port, CheckType: "tcp",
		Interval: 60 * time.Second, Timeout: 2 * time.Second,
	})
	c.SetSpecs([]ExternalSpec{spec})
	c.Check(context.Background(), Target{})

	// An identical assignment arrives again (reconcile), then a tick fires.
	c.SetSpecs([]ExternalSpec{spec})
	clock = clock.Add(ExternalTick)
	res := c.Check(context.Background(), Target{})

	if res.Details != nil {
		t.Fatalf("a re-pushed identical assignment must not reset the interval, got %v", res.Details)
	}
	if got := c.Counts()[0].Probes; got != 1 {
		t.Errorf("probes = %d, want 1", got)
	}
}

// A refused target is counted on every probe but logged at most once per
// suppression window, so a denied assignment cannot turn into its own outage.
func TestExternalCheckerWarnsOncePerTargetForDenials(t *testing.T) {
	buf := captureLogs(t)

	c := newTestExternalChecker(t, mustAllowlist(t, []string{"10.0.0.0/8"}, nil))
	clock := time.Now()
	c.now = func() time.Time { return clock }

	c.SetSpecs([]ExternalSpec{mustParseSpec(t, &ExternalSpecInput{
		DefinitionID: "def-denied", Name: "denied", Address: "127.0.0.1", Port: 80, CheckType: "tcp",
		Interval: ExternalTick, Timeout: time.Second,
	})})

	const ticks = 6
	for range ticks {
		c.Check(context.Background(), Target{})
		clock = clock.Add(ExternalTick)
	}

	if got := c.Counts()[0].Denied; got != ticks {
		t.Fatalf("every refusal must be counted: denied = %d, want %d", got, ticks)
	}
	if got := strings.Count(buf.String(), "external destination refused"); got != 1 {
		t.Fatalf("refusal must be logged once per window, logged %d times", got)
	}

	// Past the suppression window the next refusal is reported again, carrying
	// the number of lines it swallowed.
	clock = clock.Add(externalDenyLogInterval)
	c.Check(context.Background(), Target{})
	if got := strings.Count(buf.String(), "external destination refused"); got != 2 {
		t.Fatalf("refusal must be logged again after the window, logged %d times", got)
	}
	if !strings.Contains(buf.String(), "suppressed="+strconv.Itoa(ticks-1)) {
		t.Errorf("the second log must report the suppressed count, got:\n%s", buf.String())
	}
}

func TestParseExternalSpecRejects(t *testing.T) {
	cases := []struct {
		name string
		in   ExternalSpecInput
		want string
	}{
		{"empty name", ExternalSpecInput{Address: "1.1.1.1", CheckType: "tcp", Interval: time.Second}, "name"},
		{"empty address", ExternalSpecInput{Name: "n", CheckType: "tcp", Interval: time.Second}, "address"},
		{"port out of range", ExternalSpecInput{Name: "n", Address: "1.1.1.1", Port: 70000, CheckType: "tcp", Interval: time.Second}, "port"},
		{"zero interval", ExternalSpecInput{Name: "n", Address: "1.1.1.1", CheckType: "tcp"}, "interval"},
		{"unknown check type", ExternalSpecInput{Name: "n", Address: "1.1.1.1", CheckType: "gopher", Interval: time.Second}, "unknown check type"},
		{"mtr refused", ExternalSpecInput{Name: "n", Address: "1.1.1.1", CheckType: "mtr", Interval: time.Second}, "not valid"},
		{"udp refused", ExternalSpecInput{Name: "n", Address: "1.1.1.1", CheckType: "udp", Interval: time.Second}, "not valid"},
		{"dns without query", ExternalSpecInput{Name: "n", Address: "1.1.1.1", CheckType: "dns", Interval: time.Second}, "query"},
		{"dns blank query", ExternalSpecInput{Name: "n", Address: "1.1.1.1", CheckType: "dns", Interval: time.Second, ParamsJSON: []byte(`{"query":"  "}`)}, "query"},
		{"http non-url address", ExternalSpecInput{Name: "n", Address: "1.1.1.1", CheckType: "http", Interval: time.Second}, "URL"},
		{"http bad scheme", ExternalSpecInput{Name: "n", Address: "ftp://example.com", CheckType: "http", Interval: time.Second}, "URL"},
		{"http bad method", ExternalSpecInput{Name: "n", Address: "https://example.com", CheckType: "http", Interval: time.Second, ParamsJSON: []byte(`{"method":"POST"}`)}, "method"},
		{"http bad expect status", ExternalSpecInput{Name: "n", Address: "https://example.com", CheckType: "http", Interval: time.Second, ParamsJSON: []byte(`{"expectStatus":42}`)}, "expectStatus"},
		{"params wrong type", ExternalSpecInput{Name: "n", Address: "https://example.com", CheckType: "http", Interval: time.Second, ParamsJSON: []byte(`{"insecureSkipVerify":"yes"}`)}, "params"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := tc.in
			_, err := ParseExternalSpec(&in)
			if err == nil {
				t.Fatalf("expected %+v to be rejected", tc.in)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q should mention %q", err, tc.want)
			}
		})
	}
}

// An unknown params key is ignored and warned about, not fatal: a Console that
// learned a newer param must not silently disable the check on every agent
// that has not been rolled yet.
func TestParseExternalSpecIgnoresUnknownParamKeys(t *testing.T) {
	buf := captureLogs(t)

	spec, err := ParseExternalSpec(&ExternalSpecInput{
		Name: "n", Address: "https://example.com", CheckType: "http", Interval: 30 * time.Second,
		ParamsJSON: []byte(`{"method":"HEAD","futureKnob":7}`),
	})
	if err != nil {
		t.Fatalf("an unknown key must not drop the spec: %v", err)
	}
	if spec.httpParams.Method != http.MethodHead {
		t.Errorf("known keys must still apply, method = %q", spec.httpParams.Method)
	}
	if !strings.Contains(buf.String(), "unknown keys in external check params") {
		t.Errorf("unknown keys must be warned about, log was:\n%s", buf.String())
	}
}

func TestParseExternalSpecDefaults(t *testing.T) {
	tcp, err := ParseExternalSpec(&ExternalSpecInput{
		Name: "n", Address: "1.1.1.1", CheckType: "tcp", Interval: 30 * time.Second,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tcp.Port != externalDefaultTCPPort {
		t.Errorf("tcp port = %d, want the %d default", tcp.Port, externalDefaultTCPPort)
	}
	if tcp.Timeout != externalDefaultTimeout {
		t.Errorf("timeout = %v, want the %v default", tcp.Timeout, externalDefaultTimeout)
	}

	// An interval below the scheduler tick is clamped: the agent cannot probe
	// faster than it is invoked, and pretending otherwise is a lie in the spec.
	fast, err := ParseExternalSpec(&ExternalSpecInput{
		Name: "n", Address: "1.1.1.1", CheckType: "tcp", Interval: time.Second,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if fast.Interval != ExternalTick {
		t.Errorf("interval = %v, want it clamped to the %v tick", fast.Interval, ExternalTick)
	}

	httpsSpec, err := ParseExternalSpec(&ExternalSpecInput{
		Name: "n", Address: "https://example.com/health", CheckType: "http", Interval: 30 * time.Second,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if httpsSpec.httpHost != "example.com" || httpsSpec.httpPort != "443" {
		t.Errorf("https default dial target = %s:%s, want example.com:443", httpsSpec.httpHost, httpsSpec.httpPort)
	}
	if httpsSpec.httpParams.InsecureSkipVerify {
		t.Error("insecureSkipVerify must default to FALSE")
	}
	if httpsSpec.httpParams.Method != http.MethodGet {
		t.Errorf("method default = %q, want GET", httpsSpec.httpParams.Method)
	}
}

// A denial reason must reach the metrics layer as a TYPED value from a closed
// set, never as a substring of the human-readable error: the refusal messages
// are deliberately vague (they must not echo an attacker-chosen name), so
// resolve and cidr are indistinguishable by text.
func TestExternalCheckerDenialCarriesTypedReason(t *testing.T) {
	cases := []struct {
		name    string
		checker func(t *testing.T) *ExternalChecker
		address string
		want    model.ExternalDenyReason
	}{
		{
			name: "address outside the allowed cidrs",
			checker: func(t *testing.T) *ExternalChecker {
				return newTestExternalChecker(t, mustAllowlist(t, []string{"10.0.0.0/8"}, nil))
			},
			address: "127.0.0.1",
			want:    model.ExternalDenyCIDR,
		},
		{
			name:    "name that cannot be resolved",
			checker: func(t *testing.T) *ExternalChecker { return newTestExternalChecker(t, loopbackAllowlist(t)) },
			address: "nowhere.invalid",
			want:    model.ExternalDenyResolve,
		},
		{
			name:    "agent with no allowlist configured",
			checker: func(_ *testing.T) *ExternalChecker { return NewExternalChecker(nil, nil, time.Second) },
			address: "127.0.0.1",
			want:    model.ExternalDenyDisabled,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := tc.checker(t)
			c.SetSpecs([]ExternalSpec{mustParseSpec(t, &ExternalSpecInput{
				DefinitionID: "def-1",
				Name:         "some-target", Address: tc.address, Port: 8080, CheckType: "tcp",
				Interval: 30 * time.Second, Timeout: time.Second,
			})})

			details := externalDetails(t, ptr(c.Check(context.Background(), Target{})))
			if !details[0].Denied {
				t.Fatalf("expected a denied probe, got %+v", details[0])
			}
			if details[0].DenyReason != tc.want {
				t.Errorf("DenyReason = %q, want %q", details[0].DenyReason, tc.want)
			}
		})
	}
}

// A probe that reached the network never carries a denial reason: the reason
// label exists only on denied_total.
func TestExternalCheckerSuccessfulProbeHasNoDenyReason(t *testing.T) {
	lis := newCountingListener(t)
	host, port := lis.hostPort(t)

	c := newTestExternalChecker(t, loopbackAllowlist(t))
	c.SetSpecs([]ExternalSpec{mustParseSpec(t, &ExternalSpecInput{
		Name: "ok-tcp", Address: host, Port: port, CheckType: "tcp",
		Interval: 30 * time.Second, Timeout: 2 * time.Second,
	})})

	details := externalDetails(t, ptr(c.Check(context.Background(), Target{})))
	if details[0].Denied || details[0].DenyReason != "" {
		t.Errorf("a network probe must carry no denial reason, got %+v", details[0])
	}
}

func ptr[T any](v T) *T { return &v }

func TestExternalCheckerNilAllowlistDeniesEverything(t *testing.T) {
	lis := newCountingListener(t)
	host, port := lis.hostPort(t)

	c := NewExternalChecker(nil, nil, time.Second)
	c.SetSpecs([]ExternalSpec{mustParseSpec(t, &ExternalSpecInput{
		Name: "no-gate", Address: host, Port: port, CheckType: "tcp",
		Interval: 30 * time.Second, Timeout: time.Second,
	})})

	res := c.Check(context.Background(), Target{})
	if res.Success {
		t.Fatal("a checker with no allowlist must deny, not fall through")
	}
	if lis.accept.Load() != 0 {
		t.Fatalf("nothing may be dialled without an allowlist, accepted %d", lis.accept.Load())
	}
}
