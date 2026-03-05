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
	client  *http.Client
}

type HTTPCheckTarget struct {
	URL          string
	Method       string
	ExpectStatus int
	BodyPattern  *regexp.Regexp
}

func NewHTTPChecker(timeout time.Duration, targets []HTTPCheckTarget) *HTTPChecker {
	return &HTTPChecker{
		targets: targets,
		timeout: timeout,
		client: &http.Client{
			Timeout: timeout,
			Transport: &http.Transport{
				DisableKeepAlives: true,
				TLSClientConfig: &tls.Config{
					MinVersion:         tls.VersionTLS12,
					InsecureSkipVerify: true, //nolint:gosec // intentional for internal cluster endpoints
				},
			},
		},
	}
}

func (c *HTTPChecker) Name() model.CheckType {
	return model.CheckHTTP
}

func (c *HTTPChecker) Check(ctx context.Context, _ Target) model.CheckResult {
	result := model.CheckResult{
		Type:      model.CheckHTTP,
		Timestamp: time.Now(),
	}

	allDetails := make([]model.HTTPDetails, 0, len(c.targets))
	var firstErr string

	for _, target := range c.targets {
		detail := c.checkOne(ctx, target)
		allDetails = append(allDetails, detail)

		if detail.StatusCode == 0 && firstErr == "" {
			firstErr = fmt.Sprintf("HTTP check %s failed", target.URL)
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
		},
		GotConn: func(_ httptrace.GotConnInfo) {
			gotConn = time.Now()
		},
		GotFirstResponseByte: func() {
			if !gotConn.IsZero() {
				detail.TTFBTime = time.Since(gotConn)
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
	resp, err := c.client.Do(req) //nolint:gosec // G704: SSRF by design — checker probes known pod IPs
	detail.TotalTime = time.Since(totalStart)

	if err != nil {
		return detail
	}
	defer func() { _ = resp.Body.Close() }()

	detail.StatusCode = resp.StatusCode

	if target.BodyPattern != nil {
		body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20)) // 1MB limit
		if err == nil && !target.BodyPattern.Match(body) {
			detail.StatusCode = -1
		}
	} else {
		_, _ = io.Copy(io.Discard, resp.Body)
	}

	return detail
}
