package checks

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/EsDmitrii/kconmon-ng/internal/console/controllerclient"
)

// lostResultCtrl answers the way controllerclient does when the controller accepted the dispatch and
// then dropped the connection without writing the result.
type lostResultCtrl struct{}

func (lostResultCtrl) Topology(context.Context) (*controllerclient.Topology, error) {
	return &controllerclient.Topology{}, nil
}

func (lostResultCtrl) Diagnose(context.Context, controllerclient.DiagnoseRequest, time.Duration) (json.RawMessage, error) {
	return nil, fmt.Errorf("controller diagnose: %w: the controller closed the connection after 30s "+
		"without answering a 1m50s dispatch; the check may have completed on the agent and its result is lost "+
		"(cut by the controller HTTP server, not by the check): EOF", controllerclient.ErrResultLost)
}

// TestDispatchPairSurfacesTheLostResultVerbatim: the run's recorded error is what the operator reads
// when an external MTR disappears, so it must carry the explanation, and it must NOT be filed as a
// check timeout -- nothing timed out, a finished answer was thrown away.
func TestDispatchPairSurfacesTheLostResultVerbatim(t *testing.T) {
	r := &Runner{ctrl: lostResultCtrl{}}
	pair := &Pair{Source: "kconmon-prod-m02", Destination: Destination{Kind: DestKindTarget, Name: "google-dns", Address: "8.8.8.8"}}
	spec := &Spec{Type: "mtr", Plane: "pod"}

	out := r.dispatchPair(context.Background(), pair, spec, 110*time.Second)

	if out.success {
		t.Fatal("a lost result is not a success")
	}
	if out.timedOut {
		t.Fatal("a dropped answer must not be recorded as a check timeout")
	}
	for _, want := range []string{"closed the connection", "result is lost", "controller HTTP server"} {
		if !strings.Contains(out.errStr, want) {
			t.Errorf("run error does not mention %q: %s", want, out.errStr)
		}
	}
	if strings.TrimSpace(out.errStr) == "EOF" {
		t.Fatal("run error degenerated back to a bare transport EOF")
	}
}
