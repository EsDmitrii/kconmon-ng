package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/EsDmitrii/kconmon-ng/internal/console/controllerclient"
)

type fixedTopology struct{ topo *controllerclient.Topology }

func (f fixedTopology) Topology(context.Context) (*controllerclient.Topology, error) {
	return f.topo, nil
}

/*
checkers.icmp.enabled=false fleet-wide: the agents re-advertise without plane:icmp at once, but the
ICMP counters keep the cells green for the whole rate window and then the matrix goes empty, which the
page read as "no probe data yet". The matrix follows the advertised planes instead.
*/
func TestMatrixFollowsTheAdvertisedPlanes(t *testing.T) {
	stale := `{"status":"success","data":{"resultType":"vector","result":[` +
		`{"metric":{"source_node":"a","destination_node":"b"},"value":[1767225600,"0"]},` +
		`{"metric":{"source_node":"b","destination_node":"a"},"value":[1767225600,"0"]}]}}`
	empty := `{"status":"success","data":{"resultType":"vector","result":[]}}`
	for name, tc := range map[string]struct {
		prom      string
		wantNodes []string
		wantCells int
	}{
		"stale counters after the switch-off": {prom: stale, wantNodes: []string{"a", "b"}, wantCells: 0},
		"no series left":                      {prom: empty, wantNodes: []string{"a", "b"}, wantCells: 0},
	} {
		t.Run(name, func(t *testing.T) {
			promSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.Contains(r.FormValue("query"), "_results_total") {
					_, _ = w.Write([]byte(tc.prom))
					return
				}
				_, _ = w.Write([]byte(empty))
			}))
			t.Cleanup(promSrv.Close)
			s := newMatrixServer(t, promSrv.URL)
			s.topology = fixedTopology{topo: &controllerclient.Topology{Agents: []controllerclient.Agent{
				{ID: "a", NodeName: "a", Capabilities: []string{"plane:tcp", "plane:udp"}},
				{ID: "b", NodeName: "b", Capabilities: []string{"plane:tcp", "plane:udp"}},
			}}}

			w := doRequest(t, s, http.MethodGet, "/api/v1/matrix?protocol=icmp", nil, nil)
			if w.Code != http.StatusOK {
				t.Fatalf("matrix = %d %s", w.Code, w.Body)
			}
			var got struct {
				Nodes []string          `json:"nodes"`
				Cells []json.RawMessage `json:"cells"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(got.Nodes, tc.wantNodes) || len(got.Cells) != tc.wantCells {
				t.Errorf("matrix = nodes %v, %d cells; want nodes %v, %d cells", got.Nodes, len(got.Cells), tc.wantNodes, tc.wantCells)
			}

			// The planes that ARE advertised are untouched.
			w = doRequest(t, s, http.MethodGet, "/api/v1/matrix?protocol=tcp", nil, nil)
			if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if tc.prom == stale && len(got.Cells) != 2 {
				t.Errorf("tcp matrix has %d cells, want both measured pairs", len(got.Cells))
			}
		})
	}
}
