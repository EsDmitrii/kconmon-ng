// Package promql is the Console's guarded, read-only Prometheus HTTP API client.
package promql

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Guard violations and upstream failures.
var (
	ErrRangeTooLarge    = errors.New("range exceeds maximum")
	ErrResponseTooLarge = errors.New("prometheus response exceeds size cap")
	ErrBadRequest       = errors.New("invalid query parameters")
)

// UpstreamError carries a Prometheus 4xx/5xx response (e.g. PromQL parse
// errors) so the API layer can forward status and body to the browser.
type UpstreamError struct {
	Status int
	Body   json.RawMessage
}

func (e *UpstreamError) Error() string {
	return fmt.Sprintf("prometheus returned %d: %s", e.Status, e.Body)
}

// Guards bounds every query the console proxies.
type Guards struct {
	QueryTimeout     time.Duration
	MaxRange         time.Duration
	MaxResponseBytes int64
}

// Client talks to one Prometheus base URL.
type Client struct {
	base   string
	guards Guards
	hc     *http.Client
}

// New returns a guarded client for baseURL (no trailing slash).
func New(baseURL string, g Guards) *Client {
	return &Client{base: baseURL, guards: g, hc: &http.Client{Timeout: g.QueryTimeout}}
}

// Query runs an instant query. A zero ts means "now" (the time param is omitted).
func (c *Client) Query(ctx context.Context, query string, ts time.Time) (json.RawMessage, error) {
	form := url.Values{"query": {query}}
	if !ts.IsZero() {
		form.Set("time", strconv.FormatFloat(float64(ts.UnixMilli())/1000, 'f', 3, 64))
	}
	return c.post(ctx, "/api/v1/query", form)
}

// QueryRange runs a range query with range/step guards applied before any
// network call.
func (c *Client) QueryRange(ctx context.Context, query string, start, end time.Time, step time.Duration) (json.RawMessage, error) {
	if step <= 0 || !end.After(start) {
		return nil, fmt.Errorf("step=%v start=%v end=%v: %w", step, start, end, ErrBadRequest)
	}
	if end.Sub(start) > c.guards.MaxRange {
		return nil, fmt.Errorf("range %v > max %v: %w", end.Sub(start), c.guards.MaxRange, ErrRangeTooLarge)
	}
	form := url.Values{
		"query": {query},
		"start": {strconv.FormatFloat(float64(start.UnixMilli())/1000, 'f', 3, 64)},
		"end":   {strconv.FormatFloat(float64(end.UnixMilli())/1000, 'f', 3, 64)},
		"step":  {strconv.FormatFloat(step.Seconds(), 'f', 3, 64)},
	}
	return c.post(ctx, "/api/v1/query_range", form)
}

// Alerts reads Prometheus's CURRENT alert set (/api/v1/alerts) verbatim; the envelope is returned
// unre-shaped, exactly like Query's.
func (c *Client) Alerts(ctx context.Context) (json.RawMessage, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/api/v1/alerts", http.NoBody)
	if err != nil {
		return nil, err
	}
	return c.do(req, "/api/v1/alerts")
}

func (c *Client) post(ctx context.Context, path string, form url.Values) (json.RawMessage, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+path, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return c.do(req, path)
}

// do issues req and applies the response guards. Shared by post and Alerts so
// the size cap and the UpstreamError shape have exactly one implementation --
// a second copy is how a new call quietly loses a guard.
func (c *Client) do(req *http.Request, path string) (json.RawMessage, error) {
	resp, err := c.hc.Do(req) //nolint:gosec // G704: base URL is operator config, not user input
	if err != nil {
		return nil, fmt.Errorf("prometheus %s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	// Read one byte past the cap to distinguish "exactly at cap" from "over".
	body, err := io.ReadAll(io.LimitReader(resp.Body, c.guards.MaxResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("prometheus %s read: %w", path, err)
	}
	if int64(len(body)) > c.guards.MaxResponseBytes {
		return nil, fmt.Errorf("%s: %w", path, ErrResponseTooLarge)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, &UpstreamError{Status: resp.StatusCode, Body: body}
	}
	return body, nil
}
