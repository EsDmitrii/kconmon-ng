package checker

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/netip"
	"slices"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"

	"github.com/EsDmitrii/kconmon-ng/internal/model"
)

func TestDNSCheckerSuccess(t *testing.T) {
	c := NewDNSChecker([]string{"localhost"}, nil, 5*time.Second)
	result := c.Check(context.Background(), Target{})

	if !result.Success {
		t.Errorf("expected success resolving localhost, got error: %s", result.Error)
	}
	if result.Type != model.CheckDNS {
		t.Errorf("expected type DNS, got %s", result.Type)
	}
	if result.Duration <= 0 {
		t.Error("expected positive duration")
	}
}

func TestDNSCheckerMultipleHosts(t *testing.T) {
	c := NewDNSChecker([]string{"localhost", "localhost"}, nil, 5*time.Second)
	result := c.Check(context.Background(), Target{})

	if !result.Success {
		t.Errorf("expected success, got error: %s", result.Error)
	}

	details, ok := result.Details.([]model.DNSDetails)
	if !ok {
		t.Fatal("expected []DNSDetails")
	}
	if len(details) != 2 {
		t.Errorf("expected 2 results, got %d", len(details))
	}
}

func TestDNSCheckerUnresolvable(t *testing.T) {
	c := NewDNSChecker([]string{"this.host.definitely.does.not.exist.invalid"}, nil, 5*time.Second)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	result := c.Check(ctx, Target{})

	if result.Success {
		t.Error("expected failure for unresolvable host")
	}
	if result.Error == "" {
		t.Error("expected error message")
	}
}

func TestDNSCheckerEmptyHosts(t *testing.T) {
	c := NewDNSChecker(nil, nil, 5*time.Second)
	result := c.Check(context.Background(), Target{})

	if !result.Success {
		t.Error("expected success for empty hosts list")
	}
}

func TestDNSCheckerTimeoutPropagated(t *testing.T) {
	const customTimeout = 3 * time.Second
	c := NewDNSChecker([]string{"localhost"}, nil, customTimeout)
	if c.timeout != customTimeout {
		t.Errorf("expected timeout %v, got %v", customTimeout, c.timeout)
	}
}

/* ── the configured timeout has to bound the lookup ──────────────────────── */

/*
 * checkers.dns.timeout used to be applied to exactly one thing: the UDP dial on the explicit-resolver
 * path, which is connectionless and returns immediately. On the default path it was not applied at
 * all, so the real bound was /etc/resolv.conf — with kubelet's ndots:5 and three search domains,
 * about forty seconds for a check configured to give up after one.
 */
func TestDNSCheckerHonoursItsOwnTimeout(t *testing.T) {
	// A resolver that never answers: the only thing that can end this lookup is the checker's bound.
	blocked := func(ctx context.Context, _ string) ([]netip.Addr, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}

	c := &DNSChecker{hosts: []string{"nowhere.invalid"}, timeout: 200 * time.Millisecond}

	start := time.Now()
	_, err := c.lookupHost(context.Background(), "nowhere.invalid", "test", blocked)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("lookup against a resolver that never answers returned no error")
	}
	if elapsed > 2*time.Second {
		t.Errorf("lookup took %s against a 200ms timeout — the bound is not being applied", elapsed)
	}
}

/* ── a specific resolver is asked the name, not the pod's search list ──── */

// startRecordingDNS answers A for probeName only (NXDOMAIN for anything else) and records every
// question name it is asked.
func startRecordingDNS(t *testing.T, probeName string) (addr string, questions func() []string) {
	t.Helper()
	lc := net.ListenConfig{}
	conn, err := lc.ListenPacket(context.Background(), "udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	var mu sync.Mutex
	var seen []string
	go func() {
		buf := make([]byte, 512)
		for {
			n, from, err := conn.ReadFrom(buf)
			if err != nil {
				return
			}
			var msg dnsmessage.Message
			if msg.Unpack(buf[:n]) != nil || len(msg.Questions) != 1 {
				continue
			}
			q := msg.Questions[0]
			mu.Lock()
			seen = append(seen, q.Name.String())
			mu.Unlock()

			msg.Response, msg.RecursionAvailable = true, true
			switch {
			case q.Name.String() != probeName+".":
				msg.RCode = dnsmessage.RCodeNameError
			case q.Type == dnsmessage.TypeA:
				msg.Answers = []dnsmessage.Resource{{
					Header: dnsmessage.ResourceHeader{Name: q.Name, Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET, TTL: 1},
					Body:   &dnsmessage.AResource{A: [4]byte{192, 0, 2, 7}},
				}}
			}
			out, err := msg.Pack()
			if err != nil {
				continue
			}
			_, _ = conn.WriteTo(out, from)
		}
	}()
	return conn.LocalAddr().String(), func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), seen...)
	}
}

// With a search list in resolv.conf (a pod: ndots:5 and three cluster domains) a relative name is
// first sent with each suffix: the resolver under test learns the cluster's domains and the probe
// times several round trips. A single-label name is expanded under any search list, ndots:1 included.
func assertOnlyAbsoluteQuestions(t *testing.T, questions []string, name string) {
	t.Helper()
	if len(questions) == 0 {
		t.Fatal("the resolver was never asked")
	}
	for _, q := range questions {
		if q != name+"." {
			t.Fatalf("the resolver was asked %q; questions %v, want only %q", q, questions, name+".")
		}
	}
}

func TestDNSCheckerExplicitResolverIsAskedTheNameOnly(t *testing.T) {
	const name = "kconmon-probe"
	addr, questions := startRecordingDNS(t, name)

	res := NewDNSChecker([]string{name}, []string{addr}, 2*time.Second).Check(context.Background(), Target{})
	if !res.Success {
		t.Fatalf("lookup through the recording resolver failed: %s", res.Error)
	}
	assertOnlyAbsoluteQuestions(t, questions(), name)
	details, ok := res.Details.([]model.DNSDetails)
	if !ok || len(details) != 1 || details[0].Host != name {
		t.Errorf("details = %+v, want the configured host %q as written", res.Details, name)
	}
}

func TestExternalLookupIsAskedTheNameOnly(t *testing.T) {
	const name = "kconmon-probe"
	addr, questions := startRecordingDNS(t, name)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := defaultExternalLookup(ctx, addr, name); err != nil {
		t.Fatalf("lookup through the recording resolver failed: %v", err)
	}
	assertOnlyAbsoluteQuestions(t, questions(), name)
}

// A name is asked rooted exactly once, whether or not it came rooted; an IP literal is its own answer
// and asks nothing; an empty name is an error.
func TestQueryResolverRootsTheNameAndAnswersALiteral(t *testing.T) {
	const name = "kconmon-probe"
	addr, questions := startRecordingDNS(t, name)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	for _, in := range []string{name, name + "."} {
		if _, err := queryResolver(ctx, addr, in); err != nil {
			t.Fatalf("queryResolver(%q) failed: %v", in, err)
		}
	}
	for _, literal := range []string{"192.0.2.1", "2001:db8::1", "fe80::1%eth0"} {
		ips, err := queryResolver(ctx, addr, literal)
		if err != nil || len(ips) != 1 || ips[0] != netip.MustParseAddr(literal) {
			t.Errorf("queryResolver(%q) = %v, %v, want the literal itself", literal, ips, err)
		}
	}
	asked := questions()
	assertOnlyAbsoluteQuestions(t, asked, name)
	if len(asked) != 4 {
		t.Errorf("the resolver was asked %v, want A and AAAA for each of the two names and nothing for a literal", asked)
	}

	if _, err := queryResolver(ctx, addr, ""); err == nil {
		t.Error("queryResolver of an empty name succeeded")
	}
}

/* ── an explicit resolver is asked, even for a name /etc/hosts pins ────── */

/*
net.Resolver answers a name listed in /etc/hosts from the file without sending a packet, whatever its
Dial: a checked name the node or the pod pins there read green through a dead resolver. The explicit
path asks the resolver for every name, so the answer to localhost, which every hosts file maps to
loopback, is the resolver's 192.0.2.7.
*/
func TestDNSCheckerExplicitResolverAnswersForAHostsFileName(t *testing.T) {
	addr, questions := startRecordingDNS(t, "localhost")

	res := NewDNSChecker([]string{"localhost"}, []string{addr}, 2*time.Second).Check(context.Background(), Target{})
	if !res.Success {
		t.Fatalf("lookup through the recording resolver failed: %s", res.Error)
	}
	assertOnlyAbsoluteQuestions(t, questions(), "localhost")
	details, _ := res.Details.([]model.DNSDetails)
	if len(details) != 1 || len(details[0].ResolvedIPs) != 1 || !details[0].ResolvedIPs[0].Equal(net.IPv4(192, 0, 2, 7)) {
		t.Errorf("details = %+v, want the resolver's answer 192.0.2.7", res.Details)
	}
}

// A resolver that never answers fails the check and the external probe, whatever the hosts file says.
func TestDeadResolverFailsAHostsFileName(t *testing.T) {
	silent, err := (&net.ListenConfig{}).ListenPacket(context.Background(), "udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = silent.Close() })
	addr := silent.LocalAddr().String()

	res := NewDNSChecker([]string{"localhost"}, []string{addr}, 200*time.Millisecond).Check(context.Background(), Target{})
	if res.Success {
		t.Errorf("the check through a resolver that never answers succeeded: %+v", res.Details)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if ips, err := defaultExternalLookup(ctx, addr, "localhost"); err == nil {
		t.Errorf("the external lookup through a resolver that never answers returned %v", ips)
	}
}

// dnsQuestion is one question a test resolver was asked, and how.
type dnsQuestion struct {
	network   string
	qtype     dnsmessage.Type
	recursion bool
}

/*
startTruncatingDNS answers every UDP question with the TC bit and no records, and the same question
over TCP on the same port with name's A and AAAA records, the way a resolver answers a record set
too large for a datagram.
*/
func startTruncatingDNS(t *testing.T, name string) (addr string, asked func() []dnsQuestion) {
	t.Helper()
	lc := net.ListenConfig{}
	var udp net.PacketConn
	var tcp net.Listener
	for range 10 {
		var err error
		if udp, err = lc.ListenPacket(context.Background(), "udp", "127.0.0.1:0"); err != nil {
			t.Fatal(err)
		}
		if tcp, err = lc.Listen(context.Background(), "tcp", udp.LocalAddr().String()); err == nil {
			break
		}
		_ = udp.Close()
		tcp = nil
	}
	if tcp == nil {
		t.Fatal("no port free for both UDP and TCP")
	}
	t.Cleanup(func() { _ = udp.Close(); _ = tcp.Close() })

	var mu sync.Mutex
	var seen []dnsQuestion
	answer := func(network string, query []byte) []byte {
		var msg dnsmessage.Message
		if msg.Unpack(query) != nil || len(msg.Questions) != 1 {
			return nil
		}
		q := msg.Questions[0]
		mu.Lock()
		seen = append(seen, dnsQuestion{network: network, qtype: q.Type, recursion: msg.RecursionDesired})
		mu.Unlock()
		msg.Response, msg.Additionals = true, nil
		switch {
		case network == "udp":
			msg.Truncated = true
		case q.Name.String() != name+".":
			msg.RCode = dnsmessage.RCodeNameError
		case q.Type == dnsmessage.TypeA:
			msg.Answers = []dnsmessage.Resource{{
				Header: dnsmessage.ResourceHeader{Name: q.Name, Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET, TTL: 1},
				Body:   &dnsmessage.AResource{A: [4]byte{192, 0, 2, 7}},
			}}
		case q.Type == dnsmessage.TypeAAAA:
			msg.Answers = []dnsmessage.Resource{{
				Header: dnsmessage.ResourceHeader{Name: q.Name, Type: dnsmessage.TypeAAAA, Class: dnsmessage.ClassINET, TTL: 1},
				Body:   &dnsmessage.AAAAResource{AAAA: netip.MustParseAddr("2001:db8::7").As16()},
			}}
		}
		out, _ := msg.Pack()
		return out
	}
	go func() {
		buf := make([]byte, 512)
		for {
			n, from, err := udp.ReadFrom(buf)
			if err != nil {
				return
			}
			if out := answer("udp", buf[:n]); out != nil {
				_, _ = udp.WriteTo(out, from)
			}
		}
	}()
	go func() {
		for {
			conn, err := tcp.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = conn.Close() }()
				var size [2]byte
				if _, err := io.ReadFull(conn, size[:]); err != nil {
					return
				}
				query := make([]byte, binary.BigEndian.Uint16(size[:]))
				if _, err := io.ReadFull(conn, query); err != nil {
					return
				}
				out := answer("tcp", query)
				_, _ = conn.Write(append(binary.BigEndian.AppendUint16(nil, uint16(len(out))), out...))
			}()
		}
	}()
	return udp.LocalAddr().String(), func() []dnsQuestion {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(seen)
	}
}

// Both record types are asked with recursion desired, and a truncated UDP answer is asked again
// over TCP.
func TestQueryResolverRetriesATruncatedAnswerOverTCP(t *testing.T) {
	const name = "dual.test"
	addr, asked := startTruncatingDNS(t, name)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	ips, err := queryResolver(ctx, addr, name)
	if err != nil {
		t.Fatalf("lookup failed: %v", err)
	}
	want := []netip.Addr{netip.MustParseAddr("192.0.2.7"), netip.MustParseAddr("2001:db8::7")}
	if !slices.Equal(ips, want) {
		t.Errorf("addresses = %v, want %v", ips, want)
	}
	questions := asked()
	for _, network := range []string{"udp", "tcp"} {
		for _, qtype := range []dnsmessage.Type{dnsmessage.TypeA, dnsmessage.TypeAAAA} {
			if !slices.Contains(questions, dnsQuestion{network: network, qtype: qtype, recursion: true}) {
				t.Errorf("no %v question over %s with recursion desired; asked %+v", qtype, network, questions)
			}
		}
	}

	_, err = queryResolver(ctx, addr, "missing.test")
	if dnsErr, ok := errors.AsType[*net.DNSError](err); !ok || !dnsErr.IsNotFound {
		t.Errorf("an NXDOMAIN name gave %v, want a not-found DNS error", err)
	}
}
