package httpapi_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/EsDmitrii/kconmon-ng/internal/console/config"
	"github.com/EsDmitrii/kconmon-ng/internal/console/controllerclient"
	"github.com/EsDmitrii/kconmon-ng/internal/console/httpapi"
	"github.com/EsDmitrii/kconmon-ng/internal/console/metrics"
	"github.com/EsDmitrii/kconmon-ng/internal/console/store"
)

// fakeTopologyHistory returns one canned snapshot (or one canned error) and
// records the instant it was asked about, so the tests can pin BOTH the
// response shape and that the handler passed the parsed timestamp through
// unmodified.
type fakeTopologyHistory struct {
	snap   store.TopologySnapshot
	err    error
	askedA []time.Time
}

func (f *fakeTopologyHistory) TopologyAt(_ context.Context, at time.Time) (store.TopologySnapshot, error) {
	f.askedAt(at)
	if f.err != nil {
		return store.TopologySnapshot{}, f.err
	}
	return f.snap, nil
}

func (f *fakeTopologyHistory) askedAt(at time.Time) { f.askedA = append(f.askedA, at) }

// newTopologyServer builds a server with a controller (so the LIVE half stays
// exercised) and the given history dependency -- pass nil to get the
// database-disabled composition.
func newTopologyServer(t *testing.T, ctrlURL string, history httpapi.TopologyHistory) *httpapi.Server {
	t.Helper()
	cfg, err := config.Load("/nonexistent")
	if err != nil {
		t.Fatal(err)
	}
	reg := prometheus.NewRegistry()
	m := metrics.New(cfg.MetricsPrefix, reg)
	var ctrl *controllerclient.Client
	if ctrlURL != "" {
		ctrl = controllerclient.New(ctrlURL, 2*time.Second)
	}
	ui := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("spa")) })
	return httpapi.NewServer(httpapi.Deps{
		Config: cfg, Metrics: m, PromRegistry: reg, UI: ui,
		Controller: ctrl, TopologyHistory: history,
	})
}

// seededHistory is the snapshot every happy-path test folds to: two nodes, two
// agents, a retention floor well before the instants asked about.
//
// It carries zones because the M7 controller attributes them, which is what
// lets a reconstructed map draw the same zone lanes as the live one. The two
// unfoldable events are the pre-M7 rows still inside the same retention
// window -- a real upgraded console's history is exactly this mixture.
func seededHistory() *fakeTopologyHistory {
	return &fakeTopologyHistory{snap: store.TopologySnapshot{
		Nodes: []store.TopologyNode{
			{Name: "node-a", Zone: "zone-a", Ready: true},
			{Name: "node-b", Zone: "zone-b", Ready: true},
		},
		Agents: []store.TopologyAgent{
			{ID: "agent-a", NodeName: "node-a", Zone: "zone-a"},
			{ID: "agent-b", NodeName: "node-b", Zone: "zone-b"},
		},
		LastChange:       time.Date(2026, 8, 5, 9, 0, 0, 0, time.UTC),
		OldestRetained:   time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC),
		EventsFolded:     7,
		UnfoldableEvents: 2,
	}}
}

// TestTopologyLivePassthroughIsUnchangedByTheAtParam is the regression that
// guards the pre-M5 contract: with no ?at=, the body must be BYTE-identical to
// what the controller proxy produced before this endpoint learned about time,
// and the history dependency must not be consulted at all.
func TestTopologyLivePassthroughIsUnchangedByTheAtParam(t *testing.T) {
	ctrl := fakeController(t)
	defer ctrl.Close()
	history := seededHistory()
	srv := newTopologyServer(t, ctrl.URL, history)

	const wantBody = `{"nodes":[{"name":"n1","zone":"z1","ready":true}],"agents":[],` +
		`"timestamp":"2026-01-01T00:00:00Z"}` + "\n"

	for _, path := range []string{
		"/api/v1/topology",
		// An empty ?at= is what an untouched form field submits, and it asks
		// no historical question -- it must stay on the live path.
		"/api/v1/topology?at=",
	} {
		rec := do(t, srv, http.MethodGet, path, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status %d: %s", path, rec.Code, rec.Body)
		}
		if got := rec.Body.String(); got != wantBody {
			t.Errorf("%s: body = %q, want the live passthrough byte-for-byte %q", path, got, wantBody)
		}
		if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
			t.Errorf("%s: Content-Type = %q, want application/json", path, ct)
		}
	}
	if len(history.askedA) != 0 {
		t.Errorf("the live path consulted the event store %d times, want 0", len(history.askedA))
	}
}

// TestTopologyAtFoldsAndMarksTheResponseHistorical pins the whole 200 body,
// including the three honesty counters and the timestamp/asOf split.
func TestTopologyAtFoldsAndMarksTheResponseHistorical(t *testing.T) {
	ctrl := fakeController(t)
	defer ctrl.Close()
	history := seededHistory()
	srv := newTopologyServer(t, ctrl.URL, history)

	rec := do(t, srv, http.MethodGet, "/api/v1/topology?at=2026-08-05T12:00:00Z", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}

	var got struct {
		Nodes []struct {
			Name  string `json:"name"`
			Zone  string `json:"zone"`
			Ready bool   `json:"ready"`
		} `json:"nodes"`
		Agents []struct {
			ID       string `json:"id"`
			NodeName string `json:"nodeName"`
			PodIP    string `json:"podIP"`
			Zone     string `json:"zone"`
		} `json:"agents"`
		Timestamp        time.Time `json:"timestamp"`
		Historical       bool      `json:"historical"`
		AsOf             time.Time `json:"asOf"`
		EventsFolded     int       `json:"eventsFolded"`
		UnfoldableEvents int       `json:"unfoldableEvents"`
		Truncated        bool      `json:"truncated"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal %s: %v", rec.Body, err)
	}

	if len(got.Nodes) != 2 || got.Nodes[0].Name != "node-a" || !got.Nodes[0].Ready {
		t.Errorf("nodes = %+v, want the folded pair", got.Nodes)
	}
	// Zone is attributed, so it must reach the response under the SAME key the
	// live passthrough uses -- one frontend type reads both bodies, and a
	// reconstruction that dropped the zone would collapse every node into one
	// nameless lane on the map.
	if got.Nodes[0].Zone != "zone-a" || got.Nodes[1].Zone != "zone-b" {
		t.Errorf("node zones = %q/%q, want zone-a/zone-b from the folded events",
			got.Nodes[0].Zone, got.Nodes[1].Zone)
	}
	// podIP is the one field no event records at any version: still empty,
	// never guessed.
	if len(got.Agents) != 2 || got.Agents[0].PodIP != "" {
		t.Errorf("agents = %+v, want podIP empty: it is not reconstructible", got.Agents)
	}
	if got.Agents[0].Zone != "zone-a" {
		t.Errorf("agent zone = %q, want zone-a", got.Agents[0].Zone)
	}
	if got.Agents[0].ID != "agent-a" || got.Agents[0].NodeName != "node-a" {
		t.Errorf("agent[0] = %+v, want agent-a on node-a", got.Agents[0])
	}
	if !got.Historical {
		t.Error("historical = false, want true -- this is how a client tells a fold from the live snapshot")
	}
	if want := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC); !got.AsOf.Equal(want) {
		t.Errorf("asOf = %v, want the requested instant %v", got.AsOf, want)
	}
	if want := time.Date(2026, 8, 5, 9, 0, 0, 0, time.UTC); !got.Timestamp.Equal(want) {
		t.Errorf("timestamp = %v, want the last folded change %v (NOT asOf)", got.Timestamp, want)
	}
	if got.EventsFolded != 7 || got.UnfoldableEvents != 2 || got.Truncated {
		t.Errorf("counters = %d/%d/%v, want 7/2/false", got.EventsFolded, got.UnfoldableEvents, got.Truncated)
	}

	if len(history.askedA) != 1 {
		t.Fatalf("store consulted %d times, want 1", len(history.askedA))
	}
	if want := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC); !history.askedA[0].Equal(want) {
		t.Errorf("store asked about %v, want the parsed param %v", history.askedA[0], want)
	}
}

// TestTopologyAtEmptyFoldStillServes200 covers the fold that legitimately
// reconstructs nothing: an instant inside retention where no subject had been
// registered yet is an EMPTY topology, not an error, and nodes/agents must be
// JSON arrays rather than null.
func TestTopologyAtEmptyFoldStillServes200(t *testing.T) {
	history := &fakeTopologyHistory{snap: store.TopologySnapshot{
		Nodes:            []store.TopologyNode{},
		Agents:           []store.TopologyAgent{},
		OldestRetained:   time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC),
		EventsFolded:     3,
		UnfoldableEvents: 3,
	}}
	srv := newTopologyServer(t, "", history)

	rec := do(t, srv, http.MethodGet, "/api/v1/topology?at=2026-08-02T00:00:00Z", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	const wantBody = `{"nodes":[],"agents":[],"timestamp":"2026-08-02T00:00:00Z","historical":true,` +
		`"asOf":"2026-08-02T00:00:00Z","eventsFolded":3,"unfoldableEvents":3,"truncated":false}` + "\n"
	if got := rec.Body.String(); got != wantBody {
		t.Errorf("body = %q, want %q", got, wantBody)
	}
}

// timelineHistory answers each instant with the membership that held at it --
// the attributed-history behaviour the M7 controller finally makes possible.
// The fold itself is proven over real records in the store package
// (TestFoldTopologyPrefixesGiveTheMembershipAtEachInstant); what belongs HERE
// is that the handler asks about the instant it was given and serves back
// whatever that instant folded to, unmixed between requests.
type timelineHistory struct {
	byInstant map[time.Time]store.TopologySnapshot
	askedA    []time.Time
}

func (h *timelineHistory) TopologyAt(_ context.Context, at time.Time) (store.TopologySnapshot, error) {
	h.askedA = append(h.askedA, at)
	snap, ok := h.byInstant[at.UTC()]
	if !ok {
		return store.TopologySnapshot{}, errors.New("test timeline has no entry for that instant")
	}
	return snap, nil
}

// TestTopologyAtWalksAJoinAndALeaveAcrossThreeInstants is the carry's payoff at
// the API surface: node-b joins, then leaves, and ?at= before / between /
// after must show three different node sets instead of the empty one every
// instant returned while the controller published unattributed events.
func TestTopologyAtWalksAJoinAndALeaveAcrossThreeInstants(t *testing.T) {
	floor := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	t1 := time.Date(2026, 8, 5, 10, 0, 0, 0, time.UTC) // node-b joins
	t2 := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC) // node-b leaves

	nodeA := store.TopologyNode{Name: "node-a", Zone: "zone-a", Ready: true}
	nodeB := store.TopologyNode{Name: "node-b", Zone: "zone-b", Ready: true}
	snap := func(last time.Time, folded int, nodes ...store.TopologyNode) store.TopologySnapshot {
		return store.TopologySnapshot{
			Nodes: nodes, Agents: []store.TopologyAgent{}, LastChange: last,
			OldestRetained: floor, EventsFolded: folded,
		}
	}

	before := t1.Add(-time.Hour)
	between := t1.Add(time.Hour)
	after := t2.Add(time.Hour)

	history := &timelineHistory{byInstant: map[time.Time]store.TopologySnapshot{
		before:  snap(floor, 1, nodeA),
		between: snap(t1, 2, nodeA, nodeB),
		after:   snap(t2, 3, nodeA),
	}}
	srv := newTopologyServer(t, "", history)

	for _, tc := range []struct {
		name  string
		at    time.Time
		nodes []string
	}{
		{"before the join", before, []string{"node-a"}},
		{"between the join and the leave", between, []string{"node-a", "node-b"}},
		{"after the leave", after, []string{"node-a"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := do(t, srv, http.MethodGet,
				"/api/v1/topology?at="+url.QueryEscape(tc.at.Format(time.RFC3339)), "")
			if rec.Code != http.StatusOK {
				t.Fatalf("status %d: %s", rec.Code, rec.Body)
			}

			var got struct {
				Nodes []struct {
					Name string `json:"name"`
					Zone string `json:"zone"`
				} `json:"nodes"`
				Historical       bool `json:"historical"`
				UnfoldableEvents int  `json:"unfoldableEvents"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatalf("unmarshal %s: %v", rec.Body, err)
			}

			names := make([]string, 0, len(got.Nodes))
			for _, n := range got.Nodes {
				names = append(names, n.Name)
			}
			if strings.Join(names, ",") != strings.Join(tc.nodes, ",") {
				t.Errorf("nodes at %v = %v, want %v", tc.at, names, tc.nodes)
			}
			if !got.Historical {
				t.Error("historical = false on an ?at= response")
			}
			// The whole point of attribution: a non-empty set with NOTHING
			// counted as unfoldable. An empty set plus a big counter is the
			// pre-M7 answer, and it must not come back.
			if got.UnfoldableEvents != 0 {
				t.Errorf("unfoldableEvents = %d, want 0 for fully attributed history", got.UnfoldableEvents)
			}
		})
	}

	if len(history.askedA) != 3 {
		t.Errorf("store consulted %d times, want once per instant", len(history.askedA))
	}
}

// TestTopologyAtPreM7HistoryStillFoldsToTheHonestEmpty keeps the OTHER half of
// the contract pinned. Attribution does not rewrite rows already on disk: an
// instant whose window holds only pre-M7 events folds to an empty node set
// with every row counted unfoldable, and that must stay a 200 carrying the
// counter -- it is what the UI's honest-empty note quotes.
func TestTopologyAtPreM7HistoryStillFoldsToTheHonestEmpty(t *testing.T) {
	history := &fakeTopologyHistory{snap: store.TopologySnapshot{
		Nodes:            []store.TopologyNode{},
		Agents:           []store.TopologyAgent{},
		OldestRetained:   time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC),
		EventsFolded:     417,
		UnfoldableEvents: 417,
	}}
	srv := newTopologyServer(t, "", history)

	rec := do(t, srv, http.MethodGet, "/api/v1/topology?at=2026-08-02T00:00:00Z", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), `"unfoldableEvents":417`) {
		t.Errorf("body = %s, want the unfoldable counter reported verbatim", rec.Body)
	}
	if !strings.Contains(rec.Body.String(), `"nodes":[]`) {
		t.Errorf("body = %s, want an empty nodes ARRAY, never null", rec.Body)
	}
}

// TestTopologyAtGarbageIs400 walks the malformed-param matrix. Note the last
// case: an RFC3339 value is required, so a bare date is refused rather than
// silently read as midnight UTC.
func TestTopologyAtGarbageIs400(t *testing.T) {
	srv := newTopologyServer(t, "", seededHistory())

	for _, raw := range []string{
		"banana",
		"2026-08-05",
		"1754308800",
		"2026-08-05 12:00:00",
		"2026-13-45T99:99:99Z",
	} {
		rec := do(t, srv, http.MethodGet, "/api/v1/topology?at="+url.QueryEscape(raw), "")
		if rec.Code != http.StatusBadRequest {
			t.Errorf("at=%q: status %d, want 400", raw, rec.Code)
		}
		if ct := rec.Header().Get("Content-Type"); ct != "application/problem+json" {
			t.Errorf("at=%q: Content-Type = %q, want application/problem+json", raw, ct)
		}
	}
}

// TestTopologyAtInTheFutureIs400 refuses a question whose answer does not
// exist yet, rather than clamping to now and answering a different one.
func TestTopologyAtInTheFutureIs400(t *testing.T) {
	history := seededHistory()
	srv := newTopologyServer(t, "", history)

	at := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	rec := do(t, srv, http.MethodGet, "/api/v1/topology?at="+at, "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400", rec.Code)
	}
	if len(history.askedA) != 0 {
		t.Error("a future instant reached the store; it must be refused before any query")
	}
}

// TestTopologyAtBeforeRetentionIs422 is the honest-answer case, and it must
// name the knob an operator would turn.
func TestTopologyAtBeforeRetentionIs422(t *testing.T) {
	history := seededHistory() // OldestRetained = 2026-08-01
	srv := newTopologyServer(t, "", history)

	rec := do(t, srv, http.MethodGet, "/api/v1/topology?at=2026-07-01T00:00:00Z", "")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status %d, want 422: %s", rec.Code, rec.Body)
	}
	var p struct {
		Detail string `json:"detail"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatalf("unmarshal problem: %v", err)
	}
	if !strings.Contains(p.Detail, "console.database.retentionDays") {
		t.Errorf("detail = %q, want it to name console.database.retentionDays", p.Detail)
	}
}

// TestTopologyAtWithNoRetainedEventsIs422 is the same refusal for a database
// that is configured but has ingested nothing: an empty table cannot answer a
// question about the past either, and pretending it means "empty cluster"
// would be the lie this whole endpoint exists to avoid.
func TestTopologyAtWithNoRetainedEventsIs422(t *testing.T) {
	history := &fakeTopologyHistory{snap: store.TopologySnapshot{
		Nodes:  []store.TopologyNode{},
		Agents: []store.TopologyAgent{},
	}}
	srv := newTopologyServer(t, "", history)

	rec := do(t, srv, http.MethodGet, "/api/v1/topology?at=2026-08-05T12:00:00Z", "")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status %d, want 422: %s", rec.Code, rec.Body)
	}
}

// TestTopologyAtWithoutADatabaseIs503 pins the asymmetry the whole param
// matrix rests on: the SAME server answers the live route 200 and ?at= 503.
func TestTopologyAtWithoutADatabaseIs503(t *testing.T) {
	ctrl := fakeController(t)
	defer ctrl.Close()
	srv := newTopologyServer(t, ctrl.URL, nil)

	if rec := do(t, srv, http.MethodGet, "/api/v1/topology", ""); rec.Code != http.StatusOK {
		t.Fatalf("live topology with no database = %d, want 200 -- it is controller-backed", rec.Code)
	}

	rec := do(t, srv, http.MethodGet, "/api/v1/topology?at=2026-08-05T12:00:00Z", "")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status %d, want 503: %s", rec.Code, rec.Body)
	}
	var p struct {
		Detail string `json:"detail"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatalf("unmarshal problem: %v", err)
	}
	if !strings.Contains(p.Detail, "console.database.mode") {
		t.Errorf("detail = %q, want it to name console.database.mode", p.Detail)
	}
}

// TestTopologyAtStoreFailureIs502 keeps the driver error out of the body.
func TestTopologyAtStoreFailureIs502(t *testing.T) {
	history := &fakeTopologyHistory{err: errors.New("dial tcp 10.0.0.1:5432: connection refused")}
	srv := newTopologyServer(t, "", history)

	rec := do(t, srv, http.MethodGet, "/api/v1/topology?at=2026-08-05T12:00:00Z", "")
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status %d, want 502", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "10.0.0.1") {
		t.Errorf("body leaked the upstream address: %s", rec.Body)
	}
}

// TestTopologyAtTruncatedFoldSaysSo: a fold that hit its row limit is missing
// its newest events, and the response must admit that rather than look
// authoritative.
func TestTopologyAtTruncatedFoldSaysSo(t *testing.T) {
	history := seededHistory()
	history.snap.Truncated = true
	srv := newTopologyServer(t, "", history)

	rec := do(t, srv, http.MethodGet, "/api/v1/topology?at=2026-08-05T12:00:00Z", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	var got struct {
		Truncated bool `json:"truncated"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !got.Truncated {
		t.Error("truncated = false, want true")
	}
}
