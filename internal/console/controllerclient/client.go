// Package controllerclient is the Console's HTTP client for the controller's
// in-cluster API (topology snapshot, version/capability probe). A non-leader
// controller replica returns 503; the client retries with backoff.
package controllerclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"
)

// ErrUnavailable is returned when the controller kept answering 503
// (no leader reachable) after all retries.
var ErrUnavailable = errors.New("controller unavailable")

// Diagnose's sentinel errors, one per plain-text http.Error status the
// controller's POST /api/v1/diagnostics returns (internal/controller/diagnostics.go).
// 503 (non-leader) does not get one of its own -- it reuses ErrUnavailable via
// the same retry ladder getJSON uses.
var (
	ErrNoAgent      = errors.New("no agent on node")   // controller 404
	ErrDispatch     = errors.New("dispatch failed")    // controller 502
	ErrCheckTimeout = errors.New("dispatch timed out") // controller 504
	ErrBadRequest   = errors.New("invalid request")    // controller 400
	// ErrExternalUnsupported is the controller's 501: the SOURCE agent does
	// not advertise the "external-checks" capability, so it would silently
	// ignore the task's external_target rather than probe it
	// (internal/controller/diagnostics.go's capability gate). It is a distinct
	// sentinel, not a flavour of ErrDispatch, because it is the one dispatch
	// failure an operator fixes by rolling agents forward rather than by
	// looking at the network -- and because a run whose pairs all fail this
	// way is reporting an agent-version problem, not a connectivity one.
	ErrExternalUnsupported = errors.New("agent does not support external destinations") // controller 501
)

const (
	maxAttempts    = 3
	initialBackoff = 200 * time.Millisecond
	maxBodyBytes   = 4 << 20 // topology snapshots are small; 4 MiB is generous

	// diagnosticsTimeoutCap mirrors internal/controller/diagnostics.go's
	// maxDiagnosticsTimeout. The controller silently clamps ?timeout=
	// server-side; clamping it here too keeps this client's own wait bounded
	// by the same number instead of trusting an uncapped caller-supplied
	// value to agree with a cap it never sees.
	diagnosticsTimeoutCap = 120 * time.Second

	// diagnoseCtxSlack is added on top of the per-request timeout sent to the
	// controller (?timeout=) when bounding tryDiagnose's own context: the
	// controller enforces that timeout server-side and answers 504 once it
	// fires, so this client's wait must outlast it by enough to still
	// receive that 504 response (network RTT, response encode/decode)
	// instead of cancelling its own request out from under a server that was
	// about to answer correctly.
	diagnoseCtxSlack = 10 * time.Second
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
	base   string
	hc     *http.Client
	diagHC *http.Client
}

// New returns a client for baseURL (no trailing slash) with a per-request
// timeout for Topology/Version (the polling/probe calls hc serves).
//
// Diagnose deliberately does NOT share hc: hc.Timeout is sized for a quick
// topology/version poll (config.go's controller.timeout default is 10s) and
// would silently cap every diagnostics dispatch at that same ceiling no
// matter what timeout the caller passes Diagnose or the ?timeout= the
// controller itself is told to honour (up to diagnosticsTimeoutCap, 120s) --
// an http.Client.Timeout applies to the whole round trip regardless of the
// context deadline passed alongside it, so the shorter of the two always
// wins. diagHC carries no Timeout of its own; tryDiagnose bounds each
// request purely through its own context (timeout + diagnoseCtxSlack),
// scoped to that one call, so it can never be a tighter, invisible ceiling
// under a caller-supplied timeout the way a shared client-wide Timeout would
// be.
func New(baseURL string, timeout time.Duration) *Client {
	return &Client{base: baseURL, hc: &http.Client{Timeout: timeout}, diagHC: &http.Client{}}
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

// DiagnoseRequest mirrors the controller's diagnosticsRequest body exactly
// (internal/controller/diagnostics.go).
type DiagnoseRequest struct {
	Source      string `json:"source"`
	Destination string `json:"destination"`
	Type        string `json:"type"`
	Plane       string `json:"plane"`
	// DestinationKind is "" (which the controller reads as "node", the only
	// value that existed before M4) or "external". DestinationAddress carries
	// the address an external destination is probed at; Destination stays the
	// metric-safe NAME either way -- the address never becomes an identifier
	// downstream (internal/controller/diagnostics.go's destName comment).
	//
	// Both are `omitempty` on purpose, and that is a compatibility guarantee,
	// not a formatting preference: a node dispatch must serialize to exactly
	// the four-field body M3 sent, byte for byte, so a controller that
	// predates these fields sees no change whatsoever from a Console that has
	// them. checks' own dispatch tests assert those bytes.
	DestinationKind    string `json:"destinationKind,omitempty"`
	DestinationAddress string `json:"destinationAddress,omitempty"`
}

// DestinationKindExternal is DiagnoseRequest.DestinationKind's non-node value,
// mirroring internal/controller/diagnostics.go's own destinationKindExternal.
// A deliberate copy, not an import: this package must not depend on
// internal/controller.
const DestinationKindExternal = "external"

// Diagnose posts to the controller's on-demand diagnostics endpoint and
// returns the agent's model.CheckResult verbatim, exactly as the controller's
// handler wrote it. timeout is sent as ?timeout=<seconds> and is clamped to
// [1s, diagnosticsTimeoutCap] (120s) both here and by the controller
// (maxDiagnosticsTimeout) -- a sub-second timeout is rounded UP to 1s rather
// than truncated to 0 by the ?timeout=<seconds> encoding (a literal
// "timeout=0" would mean "no timeout" to a server that does not itself
// distinguish "caller sent zero" from "caller sent nothing"). So this
// client's own wait and the server's cap cannot disagree on the SIZE of the
// bound -- and, since Client.New, they no longer can on ENFORCEMENT either:
// tryDiagnose bounds this call by its own context (timeout +
// diagnoseCtxSlack), not by the shared, unrelated hc.Timeout New() also
// configures for Topology/Version, so a short controller.timeout config
// value can no longer silently cap a longer diagnose timeout out from under
// the caller (see New's doc comment).
//
// 503 (non-leader) reuses getJSON's retry ladder: up to maxAttempts,
// doubling backoff, ErrUnavailable once exhausted. 502 and 504 do NOT
// retry -- a dispatch that reached an agent and then failed or timed out
// must not be silently re-run against a cluster the operator is diagnosing.
// 400, 404 and 501 are single-shot request-shape/topology/capability errors
// that a retry cannot fix either -- retrying a 501 in particular cannot make
// an old agent grow the external-checks capability.
func (c *Client) Diagnose(ctx context.Context, req DiagnoseRequest, timeout time.Duration) (json.RawMessage, error) { //nolint:gocritic // hugeParam: DiagnoseRequest is a value-type request payload, mirroring the controller's own diagnosticsRequest, and is named in checks' controllerAPI interface -- every fake would have to change with it
	switch {
	case timeout <= 0, timeout > diagnosticsTimeoutCap:
		timeout = diagnosticsTimeoutCap
	case timeout < time.Second:
		// int(timeout.Seconds()) below truncates toward zero -- anything under
		// 1s would otherwise be encoded as the literal string "timeout=0".
		timeout = time.Second
	}

	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("controller diagnose: encode request: %w", err)
	}

	backoff := initialBackoff
	for attempt := 1; ; attempt++ {
		data, status, err := c.tryDiagnose(ctx, body, timeout)
		switch {
		case err == nil && status == http.StatusOK:
			return data, nil
		case status == http.StatusServiceUnavailable && attempt < maxAttempts:
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(backoff):
			}
			backoff *= 2
		case status == http.StatusServiceUnavailable:
			return nil, fmt.Errorf("controller diagnose after %d attempts: %w", attempt, ErrUnavailable)
		case status == http.StatusBadRequest:
			return nil, fmt.Errorf("controller diagnose: %w: %s", ErrBadRequest, bytes.TrimSpace(data))
		case status == http.StatusNotFound:
			return nil, fmt.Errorf("controller diagnose: %w: %s", ErrNoAgent, bytes.TrimSpace(data))
		case status == http.StatusBadGateway:
			return nil, fmt.Errorf("controller diagnose: %w: %s", ErrDispatch, bytes.TrimSpace(data))
		case status == http.StatusGatewayTimeout:
			return nil, fmt.Errorf("controller diagnose: %w: %s", ErrCheckTimeout, bytes.TrimSpace(data))
		case status == http.StatusNotImplemented:
			return nil, fmt.Errorf("controller diagnose: %w: %s", ErrExternalUnsupported, bytes.TrimSpace(data))
		case err != nil:
			return nil, fmt.Errorf("controller diagnose: %w", err)
		default:
			return nil, fmt.Errorf("controller diagnose: unexpected status %d", status)
		}
	}
}

// tryDiagnose issues one POST /api/v1/diagnostics attempt and returns the
// response body (capped to maxBodyBytes, exactly like tryOnce) alongside the
// status code, regardless of whether that status is 200 -- Diagnose needs
// the plain-text http.Error body to build its wrapped sentinel errors.
//
// The request is bounded by a context derived here (timeout + diagnoseCtxSlack),
// not by a client-wide http.Client.Timeout -- see New's and Diagnose's doc
// comments for why diagHC deliberately carries none of its own -- and issued
// on diagHC, never the shared hc Topology/Version use, so this bound is the
// only one in effect.
func (c *Client) tryDiagnose(ctx context.Context, body []byte, timeout time.Duration) (data []byte, status int, err error) {
	ctx, cancel := context.WithTimeout(ctx, timeout+diagnoseCtxSlack)
	defer cancel()

	url := c.base + "/api/v1/diagnostics?timeout=" + strconv.Itoa(int(timeout.Seconds()))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.diagHC.Do(req) //nolint:gosec // G704: base URL is operator config, not user input
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = resp.Body.Close() }()

	data, err = io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("read body: %w", err)
	}
	return data, resp.StatusCode, nil
}

// ExternalTarget is one continuous probe's destination, mirroring the
// controller's externalTargetJSON (internal/controller/external.go) and, through
// it, pb.ExternalTarget. Port 0 means "the check type's default" -- the proto's
// own convention, not a missing value.
type ExternalTarget struct {
	Name    string `json:"name"`
	Kind    string `json:"kind"`
	Address string `json:"address"`
	Port    uint32 `json:"port"`
}

// ExternalCheckSpec is one continuous external check assigned to one agent,
// mirroring the controller's externalCheckSpecJSON field for field. DefinitionID
// is correlation only and NEVER becomes a metric label (pb.ExternalCheckSpec's
// own comment); Params is the definition's opaque params object, forwarded to
// the agent for its own check-type validation and left `omitempty` so a
// paramless spec serializes without a null.
//
// CheckType is restricted to tcp|icmp|dns|http by the CONTROLLER (400 on
// anything else, and the 400 rejects the WHOLE PUT), which is why the caller
// filters before it gets here rather than discovering it on the wire.
type ExternalCheckSpec struct {
	DefinitionID string          `json:"definitionId"`
	Target       ExternalTarget  `json:"target"`
	CheckType    string          `json:"checkType"`
	IntervalNs   int64           `json:"intervalNs"`
	TimeoutNs    int64           `json:"timeoutNs"`
	Params       json.RawMessage `json:"params,omitempty"`
}

// externalChecksRequest is the PUT body: the WHOLE desired state, never a
// delta. An agent absent from the map, or present with an empty list, has no
// checks -- both spellings converge on the controller pushing that agent an
// empty assignment.
type externalChecksRequest struct {
	Agents map[string][]ExternalCheckSpec `json:"agents"`
}

// ExternalChecksResult is the controller's 200 body. Changed is 0 for a
// retried identical PUT (the endpoint is idempotent by construction), and
// Unknown lists the agent IDs the controller's registry does not know -- a
// warning, not a failure, because the Console's topology view can legitimately
// lag the registry.
type ExternalChecksResult struct {
	Agents  int      `json:"agents"`
	Changed int      `json:"changed"`
	Unknown []string `json:"unknown"`
}

// PutExternalChecks replaces the controller's ENTIRE continuous external-check
// assignment state with agents.
//
// It rides the same retry ladder Topology/Version/Diagnose use for 503: a
// non-leader controller replica cannot fan out, so it answers 503, and
// retrying is safe here for the same reason it is safe for Diagnose's
// dispatch-free 503 -- the PUT is absolute and idempotent, so landing on the
// leader on the second attempt produces exactly the state the first attempt
// wanted. Exhausted retries return ErrUnavailable.
//
// A 400 is ErrBadRequest and is NOT retried: it means a spec in this body is
// malformed (in practice an ineligible checkType), which no number of attempts
// can fix, and -- because the controller rejects the whole body, not the
// offending spec -- it means NOTHING was applied. The caller must therefore
// treat it exactly like any other failed PUT: not as a partial success.
func (c *Client) PutExternalChecks(ctx context.Context, agents map[string][]ExternalCheckSpec) (*ExternalChecksResult, error) {
	if agents == nil {
		// An explicit empty object, never a JSON null: the controller reads
		// "agents":{} as "no agent has any check" and sweeps every assignment,
		// which is the honest meaning of an empty desired state.
		agents = map[string][]ExternalCheckSpec{}
	}
	body, err := json.Marshal(externalChecksRequest{Agents: agents})
	if err != nil {
		return nil, fmt.Errorf("controller external-checks: encode request: %w", err)
	}

	backoff := initialBackoff
	for attempt := 1; ; attempt++ {
		data, status, err := c.tryPutExternalChecks(ctx, body)
		switch {
		case err == nil && status == http.StatusOK:
			var out ExternalChecksResult
			if decErr := json.Unmarshal(data, &out); decErr != nil {
				return nil, fmt.Errorf("controller external-checks: decode response: %w", decErr)
			}
			return &out, nil
		case status == http.StatusServiceUnavailable && attempt < maxAttempts:
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(backoff):
			}
			backoff *= 2
		case status == http.StatusServiceUnavailable:
			return nil, fmt.Errorf("controller external-checks after %d attempts: %w", attempt, ErrUnavailable)
		case status == http.StatusBadRequest:
			return nil, fmt.Errorf("controller external-checks: %w: %s", ErrBadRequest, bytes.TrimSpace(data))
		case err != nil:
			return nil, fmt.Errorf("controller external-checks: %w", err)
		default:
			return nil, fmt.Errorf("controller external-checks: unexpected status %d", status)
		}
	}
}

// tryPutExternalChecks issues one PUT attempt and returns the body (capped to
// maxBodyBytes, exactly like tryOnce/tryDiagnose) alongside the status, so
// PutExternalChecks can build its wrapped sentinel errors from the plain-text
// http.Error body.
//
// It runs on the shared hc, not diagHC: this is a small control-plane write on
// the same latency budget as a topology poll, so controller.timeout is the
// right ceiling for it -- unlike a diagnostics dispatch, nothing here waits on
// an agent's probe.
func (c *Client) tryPutExternalChecks(ctx context.Context, body []byte) (data []byte, status int, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, c.base+"/api/v1/external-checks", bytes.NewReader(body))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.hc.Do(req) //nolint:gosec // G704: base URL is operator config, not user input
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = resp.Body.Close() }()

	data, err = io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("read body: %w", err)
	}
	return data, resp.StatusCode, nil
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
