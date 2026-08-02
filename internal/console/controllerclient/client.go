// Package controllerclient is the Console's HTTP client for the controller's
// in-cluster API (topology snapshot, version/capability probe). A non-leader
// controller replica returns 503; the client retries with backoff.
package controllerclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// ErrUnavailable is returned when the controller kept answering 503
// (no leader reachable) after all retries.
var ErrUnavailable = errors.New("controller unavailable")

const (
	maxAttempts    = 3
	initialBackoff = 200 * time.Millisecond
	maxBodyBytes   = 4 << 20 // topology snapshots are small; 4 MiB is generous
)

// Node mirrors docs/api.md GET /api/v1/topology "nodes" entries.
type Node struct {
	Name  string `json:"name"`
	Zone  string `json:"zone"`
	Ready bool   `json:"ready"`
}

// Agent mirrors docs/api.md GET /api/v1/topology "agents" entries.
type Agent struct {
	ID       string `json:"id"`
	NodeName string `json:"nodeName"`
	PodIP    string `json:"podIP"`
	Zone     string `json:"zone"`
}

// Topology is the controller's topology snapshot.
type Topology struct {
	Nodes     []Node    `json:"nodes"`
	Agents    []Agent   `json:"agents"`
	Timestamp time.Time `json:"timestamp"`
}

// Version is the controller's build identity, used for capability detection.
type Version struct {
	Version      string   `json:"version"`
	Commit       string   `json:"commit"`
	Capabilities []string `json:"capabilities"`
}

// HasCapability reports whether name is present in Capabilities. Safe to call
// on a nil Capabilities slice (a controller that predates capability flags) —
// always returns false in that case.
func (v *Version) HasCapability(name string) bool {
	for _, c := range v.Capabilities {
		if c == name {
			return true
		}
	}
	return false
}

// Client talks to one controller Service base URL.
type Client struct {
	base string
	hc   *http.Client
}

// New returns a client for baseURL (no trailing slash) with a per-request timeout.
func New(baseURL string, timeout time.Duration) *Client {
	return &Client{base: baseURL, hc: &http.Client{Timeout: timeout}}
}

// Topology fetches the live topology snapshot, retrying 503 (non-leader).
func (c *Client) Topology(ctx context.Context) (*Topology, error) {
	var t Topology
	if err := c.getJSON(ctx, "/api/v1/topology", &t); err != nil {
		return nil, err
	}
	return &t, nil
}

// Version fetches the controller version, retrying 503 (non-leader).
func (c *Client) Version(ctx context.Context) (*Version, error) {
	var v Version
	if err := c.getJSON(ctx, "/api/v1/version", &v); err != nil {
		return nil, err
	}
	return &v, nil
}

func (c *Client) getJSON(ctx context.Context, path string, out any) error {
	backoff := initialBackoff
	for attempt := 1; ; attempt++ {
		status, err := c.tryOnce(ctx, path, out)
		switch {
		case err == nil && status == http.StatusOK:
			return nil
		case status == http.StatusServiceUnavailable && attempt < maxAttempts:
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(backoff):
			}
			backoff *= 2
		case status == http.StatusServiceUnavailable:
			return fmt.Errorf("controller %s after %d attempts: %w", path, attempt, ErrUnavailable)
		case err != nil:
			return fmt.Errorf("controller %s: %w", path, err)
		default:
			return fmt.Errorf("controller %s: unexpected status %d", path, status)
		}
	}
}

func (c *Client) tryOnce(ctx context.Context, path string, out any) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, http.NoBody)
	if err != nil {
		return 0, err
	}
	resp, err := c.hc.Do(req) //nolint:gosec // G704: base URL is operator config, not user input
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxBodyBytes))
		return resp.StatusCode, nil
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxBodyBytes)).Decode(out); err != nil {
		return resp.StatusCode, fmt.Errorf("decode: %w", err)
	}
	return resp.StatusCode, nil
}
