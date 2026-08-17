package checker

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"net/http/httptrace"
	"regexp"
	"time"

	"github.com/EsDmitrii/kconmon-ng/internal/model"
)

type HTTPChecker struct {
	targets []HTTPCheckTarget
	timeout time.Duration
	// client verifies certificates; insecure is the opt-out, built only when some target asks for it.
	client   *http.Client
	insecure *http.Client
}

type HTTPCheckTarget struct {
	URL          string
	Method       string
	ExpectStatus int
	BodyPattern  *regexp.Regexp
	// InsecureSkipVerify opts THIS target out of certificate verification; see config.HTTPTarget.
	InsecureSkipVerify bool
}

func newHTTPCheckClient(timeout time.Duration, skipVerify bool) *http.Client {
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			DisableKeepAlives: true,
			TLSClientConfig: &tls.Config{
				MinVersion:         tls.VersionTLS12,
				InsecureSkipVerify: skipVerify, //nolint:gosec // G402: per-target opt-in, off by default
			},
		},
	}
}

/*
NewHTTPChecker builds the node-local HTTP checker.

Certificates are VERIFIED. One shared client used to set InsecureSkipVerify for every target with no
way to turn it on or off, so an https check could not fail on an expired certificate, a wrong
hostname or a swapped CA — it handshaked, got its 200, and reported success while exactly the
condition an operator adds an https check to notice went unseen. Nothing outside a code comment said
so, and the external HTTP checker defaulted the other way. A target that really does front a
self-signed certificate opts out per target.
*/
func NewHTTPChecker(timeout time.Duration, targets []HTTPCheckTarget) *HTTPChecker {
	c := &HTTPChecker{
		targets: targets,
		timeout: timeout,
		client:  newHTTPCheckClient(timeout, false),
	}
	for i := range targets {
		if targets[i].InsecureSkipVerify {
			c.insecure = newHTTPCheckClient(timeout, true)
			break
		}
	}
	return c
}

// clientFor picks the verifying client unless this target opted out.
func (c *HTTPChecker) clientFor(t *HTTPCheckTarget) *http.Client {
	if t.InsecureSkipVerify && c.insecure != nil {
		return c.insecure
	}
	return c.client
}

func (c *HTTPChecker) Name() model.CheckType {
	return model.CheckHTTP
}

func (c *HTTPChecker) Check(ctx context.Context, _ Target) model.CheckResult { //nolint:gocritic // hugeParam: Target is a VALUE by design -- a checker must not be able to mutate the caller's copy, and one 80-byte copy per probe is nothing next to the probe itself
	result := model.CheckResult{
		Type:      model.CheckHTTP,
		Timestamp: time.Now(),
	}

	allDetails := make([]model.HTTPDetails, 0, len(c.targets))
	var firstErr string

	for _, target := range c.targets {
		detail := c.checkOne(ctx, target)

		/* The STATUS is part of the answer, and it was not read at all: expectStatus was parsed from
		   config, validated by the chart's schema, copied into the checker — and then never consulted,
		   so a target configured to expect 200 reported success on a 301, a 404 or a 503. The rule is
		   the one the external HTTP checker already applies (external.go): an explicit expectStatus
		   must match exactly, and without one anything from 400 up is a failure.

		   The verdict is recorded ON THE DETAIL, per target, and NOT gated on firstErr: the metrics
		   handler used to re-derive the outcome from the status code alone, so an expectStatus
		   violation — the one thing expectStatus exists to catch — incremented the success counter. */
		var statusErr string
		if detail.StatusCode != 0 {
			if want := target.ExpectStatus; want != 0 {
				if detail.StatusCode != want {
					detail.StatusMismatch = true
					statusErr = fmt.Sprintf("HTTP check %s: unexpected status %d, want %d", target.URL, detail.StatusCode, want)
				}
			} else if detail.StatusCode >= http.StatusBadRequest {
				/* StatusMismatch is set HERE too, and that is what makes the metric agree with the
				   check. It used to be set only on the explicit-expectStatus branch, and the metrics
				   handler compensated with its own `StatusCode >= 400` term — which then had to be
				   removed, because it overrode the checker in the other direction (a target that ASKS
				   for a 401 is a success). With the term gone this branch was the only thing left
				   saying "no expectStatus, and this is a failure", and it said it to result.Error
				   alone: the probe failed while kconmon_ng_http_results_total counted it a success.
				   One flag, read by both. */
				detail.StatusMismatch = true
				statusErr = fmt.Sprintf("HTTP check %s: unexpected status %d", target.URL, detail.StatusCode)
			}
		}
		allDetails = append(allDetails, detail)

		if firstErr != "" {
			continue
		}
		switch {
		case detail.StatusCode == 0:
			firstErr = fmt.Sprintf("HTTP check %s failed", target.URL)
		case statusErr != "":
			firstErr = statusErr
		case detail.BodyMismatch:
			firstErr = fmt.Sprintf("HTTP check %s: body pattern mismatch", target.URL)
		}
	}

	if firstErr != "" {
		result.Error = firstErr
	} else {
		result.Success = true
	}

	if len(allDetails) > 0 {
		result.Duration = allDetails[0].TotalTime
		result.Details = allDetails
	}

	return result
}

func (c *HTTPChecker) checkOne(ctx context.Context, target HTTPCheckTarget) model.HTTPDetails {
	detail := model.HTTPDetails{
		URL:    target.URL,
		Method: target.Method,
	}

	if detail.Method == "" {
		detail.Method = http.MethodGet
	}

	var (
		dnsStart     time.Time
		connectStart time.Time
		tlsStart     time.Time
		gotConn      time.Time
	)

	trace := &httptrace.ClientTrace{
		DNSStart: func(_ httptrace.DNSStartInfo) {
			dnsStart = time.Now()
		},
		DNSDone: func(_ httptrace.DNSDoneInfo) {
			detail.DNSTime = time.Since(dnsStart)
			// Which phases RAN, so the metrics handler can tell "0 ms" from "never happened". An
			// IP-literal URL resolves nothing and a plain-http one shakes no hands; observing their
			// zeroes reported measurements that were never taken.
			detail.DNSTimed = true
		},
		ConnectStart: func(_, _ string) {
			connectStart = time.Now()
		},
		ConnectDone: func(_, _ string, err error) {
			if err == nil {
				detail.ConnectTime = time.Since(connectStart)
			}
		},
		TLSHandshakeStart: func() {
			tlsStart = time.Now()
		},
		TLSHandshakeDone: func(_ tls.ConnectionState, _ error) {
			detail.TLSTime = time.Since(tlsStart)
			detail.TLSTimed = true
		},
		GotConn: func(_ httptrace.GotConnInfo) {
			gotConn = time.Now()
		},
		GotFirstResponseByte: func() {
			if !gotConn.IsZero() {
				detail.TTFBTime = time.Since(gotConn)
				detail.TTFBTimed = true
			}
		},
	}

	req, err := http.NewRequestWithContext(httptrace.WithClientTrace(ctx, trace), detail.Method, target.URL, http.NoBody)
	if err != nil {
		return detail
	}
	req.Header.Set("User-Agent", "kconmon-ng")
	req.Header.Set("Connection", "close")

	totalStart := time.Now()
	resp, err := c.clientFor(&target).Do(req) //nolint:gosec // G704: SSRF by design — checker probes known pod IPs
	detail.TotalTime = time.Since(totalStart)

	if err != nil {
		return detail
	}
	defer func() { _ = resp.Body.Close() }()

	detail.StatusCode = resp.StatusCode

	if target.BodyPattern != nil {
		body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20)) // 1MB limit
		if err == nil && !target.BodyPattern.Match(body) {
			detail.BodyMismatch = true
		}
	} else {
		_, _ = io.Copy(io.Discard, resp.Body)
	}

	return detail
}
