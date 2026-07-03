package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/EsDmitrii/kconmon-ng/internal/model"
)

// VersionInfo is the /api/v1/version response shape.
type VersionInfo struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
}

// shortCallTimeout bounds the quick, fixed-cost endpoints (topology, agents,
// version) that do not take a user-supplied --timeout.
const shortCallTimeout = 30 * time.Second

// diagnosticsDeadlineSlack is added on top of the caller's requested
// diagnostics/mtr timeout to give the controller room to respond after its
// own (server-capped) timeout elapses, before the client gives up. It is a
// package variable (not a const) so tests can shrink it and stay fast.
var diagnosticsDeadlineSlack = 10 * time.Second

// serverTimeoutCap mirrors the controller's own cap on the ?timeout= value
// (see internal/cli/check.go flag help / controller docs).
const serverTimeoutCap = 120 * time.Second

// maxDiagnosticsClientTimeout is the hard ceiling for the client-side
// diagnostics deadline: the controller caps its own timeout at
// serverTimeoutCap, so nothing the client waits for should exceed that plus
// slack.
func maxDiagnosticsClientTimeout() time.Duration {
	return serverTimeoutCap + diagnosticsDeadlineSlack
}

// Client talks to the controller HTTP API over an already-established base URL
// (typically a local port-forward endpoint). It is deliberately transport
// agnostic so command logic can be exercised against an httptest.Server.
type Client struct {
	baseURL string
	// http is used for the short, fixed-cost calls (topology/agents/version);
	// its Timeout is a hard cap independent of ctx.
	http *http.Client
	// longCall is used for diagnostics/mtr, whose deadline varies with the
	// caller-supplied --timeout; it carries no fixed Timeout of its own so the
	// per-request context deadline set in Diagnostics is what governs it.
	longCall *http.Client
}

// NewClient builds a Client for the given base URL (e.g. http://127.0.0.1:34567).
func NewClient(baseURL string) *Client {
	return &Client{
		baseURL:  strings.TrimRight(baseURL, "/"),
		http:     &http.Client{Timeout: shortCallTimeout},
		longCall: &http.Client{},
	}
}

// apiError reports a non-2xx HTTP response with the server's message trimmed
// to a single readable line.
type apiError struct {
	status int
	body   string
}

func (e *apiError) Error() string {
	msg := strings.TrimSpace(e.body)
	if msg == "" {
		return fmt.Sprintf("controller returned HTTP %d", e.status)
	}
	// Collapse to the first line; http.Error bodies are plain text.
	if i := strings.IndexByte(msg, '\n'); i >= 0 {
		msg = msg[:i]
	}
	return fmt.Sprintf("controller returned HTTP %d: %s", e.status, msg)
}

// Topology fetches the current topology snapshot.
func (c *Client) Topology(ctx context.Context) (*model.TopologySnapshot, []byte, error) {
	raw, err := c.get(ctx, "/api/v1/topology")
	if err != nil {
		return nil, nil, err
	}
	var snap model.TopologySnapshot
	if err := json.Unmarshal(raw, &snap); err != nil {
		return nil, raw, fmt.Errorf("decoding topology response: %w", err)
	}
	return &snap, raw, nil
}

// Version fetches the controller version.
func (c *Client) Version(ctx context.Context) (*VersionInfo, []byte, error) {
	raw, err := c.get(ctx, "/api/v1/version")
	if err != nil {
		return nil, nil, err
	}
	var v VersionInfo
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, raw, fmt.Errorf("decoding version response: %w", err)
	}
	return &v, raw, nil
}

// DiagnosticsRequest is the POST /api/v1/diagnostics body.
type DiagnosticsRequest struct {
	Source      string `json:"source"`
	Destination string `json:"destination"`
	Type        string `json:"type"`
	Plane       string `json:"plane,omitempty"`
}

// Diagnostics runs a one-shot check. timeout, when > 0, is passed to the
// controller as ?timeout=<seconds> (the controller caps it at 120s). The
// client itself enforces a hard deadline of timeout+10s (capped at 130s) so a
// wedged controller or dropped port-forward cannot hang the CLI forever.
func (c *Client) Diagnostics(ctx context.Context, req DiagnosticsRequest, timeout time.Duration) (*model.CheckResult, []byte, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, nil, fmt.Errorf("encoding request: %w", err)
	}

	path := "/api/v1/diagnostics"
	if timeout > 0 {
		secs := int(timeout.Round(time.Second) / time.Second)
		if secs < 1 {
			secs = 1
		}
		path += "?timeout=" + strconv.Itoa(secs)
	}

	clientDeadline := timeout + diagnosticsDeadlineSlack
	if timeout <= 0 || clientDeadline > maxDiagnosticsClientTimeout() {
		clientDeadline = maxDiagnosticsClientTimeout()
	}
	ctx, cancel := context.WithTimeout(ctx, clientDeadline)
	defer cancel()

	raw, err := c.doRequest(ctx, c.longCall, http.MethodPost, path, body)
	if err != nil {
		return nil, nil, err
	}
	var res model.CheckResult
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, raw, fmt.Errorf("decoding diagnostics response: %w", err)
	}
	return &res, raw, nil
}

// get performs a short-call GET, bounded by c.http's fixed shortCallTimeout.
func (c *Client) get(ctx context.Context, path string) ([]byte, error) {
	return c.doRequest(ctx, c.http, http.MethodGet, path, nil)
}

// doRequest builds and executes a request against the given http.Client,
// which callers pick based on whether the fixed short-call timeout or a
// caller-supplied context deadline should govern.
func (c *Client) doRequest(ctx context.Context, hc *http.Client, method, path string, body []byte) ([]byte, error) {
	var bodyReader io.Reader = http.NoBody
	if body != nil {
		bodyReader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, bodyReader)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading response body: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &apiError{status: resp.StatusCode, body: string(raw)}
	}
	return raw, nil
}
