package checker

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

// ednsUDPSize is the UDP answer size a query advertises, the one Go's own resolver asks for.
const ednsUDPSize = 1232

var (
	errNoSuchHost = errors.New("no such host")
	// errTruncated: the answer did not fit in a UDP datagram, so the question goes again over TCP.
	errTruncated = errors.New("truncated answer")
)

/*
queryResolver asks the resolver at server for name's A and AAAA records itself. A net.Resolver answers
a name listed in /etc/hosts from the file without sending a packet, whatever its Dial, so it cannot
tell a dead resolver from a name the node (hostNetwork, a bare-host agent) or the pod (hostAliases)
pins there. An IP literal is its own answer.
*/
func queryResolver(ctx context.Context, server, name string) ([]netip.Addr, error) {
	if ip, err := netip.ParseAddr(name); err == nil {
		return []netip.Addr{ip}, nil
	}
	// dnsmessage packs only rooted names, and a rooted name is asked as written.
	if !strings.HasSuffix(name, ".") {
		name += "."
	}
	qname, err := dnsmessage.NewName(name)
	if err != nil {
		return nil, &net.DNSError{Err: err.Error(), Name: name, Server: server}
	}

	var addrs [2][]netip.Addr
	var errs [2]error
	var wg sync.WaitGroup
	for i, qtype := range [2]dnsmessage.Type{dnsmessage.TypeA, dnsmessage.TypeAAAA} {
		wg.Go(func() { addrs[i], errs[i] = ask(ctx, server, &qname, qtype) })
	}
	wg.Wait()

	if all := slices.Concat(addrs[0], addrs[1]); len(all) > 0 {
		return all, nil
	}
	err = cmp.Or(errs[0], errs[1], errNoSuchHost)
	return nil, &net.DNSError{
		Err:        err.Error(),
		Name:       name,
		Server:     server,
		IsNotFound: errors.Is(err, errNoSuchHost),
		IsTimeout:  errors.Is(err, os.ErrDeadlineExceeded),
		UnwrapErr:  err,
	}
}

// ask sends one question with recursion desired, over UDP and again over TCP if the answer was cut.
func ask(ctx context.Context, server string, name *dnsmessage.Name, qtype dnsmessage.Type) ([]netip.Addr, error) {
	var id [2]byte
	_, _ = rand.Read(id[:]) // crypto/rand.Read never returns an error
	msg := dnsmessage.Message{
		Header:    dnsmessage.Header{ID: binary.BigEndian.Uint16(id[:]), RecursionDesired: true},
		Questions: []dnsmessage.Question{{Name: *name, Type: qtype, Class: dnsmessage.ClassINET}},
	}
	var opt dnsmessage.ResourceHeader
	if err := opt.SetEDNS0(ednsUDPSize, dnsmessage.RCodeSuccess, false); err != nil {
		return nil, err
	}
	msg.Additionals = []dnsmessage.Resource{{Header: opt, Body: &dnsmessage.OPTResource{}}}
	query, err := msg.Pack()
	if err != nil {
		return nil, err
	}

	addrs, err := exchange(ctx, "udp", server, query)
	if errors.Is(err, errTruncated) {
		addrs, err = exchange(ctx, "tcp", server, query)
	}
	return addrs, err
}

// exchange sends query to server over network and returns the addresses its answer holds.
func exchange(ctx context.Context, network, server string, query []byte) ([]netip.Addr, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, network, server)
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close() }()
	stop := context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Unix(1, 0)) })
	defer stop()

	roundTrip := udpRoundTrip
	if network == "tcp" {
		roundTrip = tcpRoundTrip
	}
	answer, err := roundTrip(conn, query)
	if err != nil {
		return nil, err
	}

	var p dnsmessage.Parser
	h, err := p.Start(answer)
	switch {
	case err != nil:
		return nil, err
	case !h.Response || h.ID != binary.BigEndian.Uint16(query):
		return nil, errors.New("the answer does not match the question")
	case h.Truncated:
		return nil, errTruncated
	case h.RCode == dnsmessage.RCodeNameError:
		return nil, errNoSuchHost
	case h.RCode != dnsmessage.RCodeSuccess:
		return nil, fmt.Errorf("resolver answered %s", strings.TrimPrefix(h.RCode.String(), "RCode"))
	}
	return answerAddrs(&p)
}

func udpRoundTrip(conn net.Conn, query []byte) ([]byte, error) {
	if _, err := conn.Write(query); err != nil {
		return nil, err
	}
	buf := make([]byte, ednsUDPSize)
	n, err := conn.Read(buf)
	return buf[:n], err
}

// tcpRoundTrip frames the question and its answer with the two-byte length DNS over TCP carries.
func tcpRoundTrip(conn net.Conn, query []byte) ([]byte, error) {
	framed := binary.BigEndian.AppendUint16(make([]byte, 0, 2+len(query)), uint16(len(query))) //nolint:gosec // G115: a packed question is far below 64 KiB
	if _, err := conn.Write(append(framed, query...)); err != nil {
		return nil, err
	}
	var size [2]byte
	if _, err := io.ReadFull(conn, size[:]); err != nil {
		return nil, err
	}
	answer := make([]byte, binary.BigEndian.Uint16(size[:]))
	_, err := io.ReadFull(conn, answer)
	return answer, err
}

// answerAddrs collects the A and AAAA records of the answer section; a CNAME chain comes with them.
func answerAddrs(p *dnsmessage.Parser) ([]netip.Addr, error) {
	if err := p.SkipAllQuestions(); err != nil {
		return nil, err
	}
	var addrs []netip.Addr
	for {
		h, err := p.AnswerHeader()
		if errors.Is(err, dnsmessage.ErrSectionDone) {
			return addrs, nil
		}
		if err != nil {
			return nil, err
		}
		switch {
		case h.Class == dnsmessage.ClassINET && h.Type == dnsmessage.TypeA:
			r, err := p.AResource()
			if err != nil {
				return nil, err
			}
			addrs = append(addrs, netip.AddrFrom4(r.A))
		case h.Class == dnsmessage.ClassINET && h.Type == dnsmessage.TypeAAAA:
			r, err := p.AAAAResource()
			if err != nil {
				return nil, err
			}
			addrs = append(addrs, netip.AddrFrom16(r.AAAA))
		default:
			if err := p.SkipAnswer(); err != nil {
				return nil, err
			}
		}
	}
}
