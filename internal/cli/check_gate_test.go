package cli

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	pb "github.com/EsDmitrii/kconmon-ng/api/proto"
	"github.com/EsDmitrii/kconmon-ng/internal/controller"
	"github.com/EsDmitrii/kconmon-ng/internal/metrics"
	"github.com/EsDmitrii/kconmon-ng/internal/model"
)

// refusingDispatcher answers every task the way an agent without that checker does.
type refusingDispatcher struct{ dispatched bool }

func (d *refusingDispatcher) Dispatch(_ context.Context, _ string, req *pb.TaskRequest) (*pb.TaskResult, error) {
	d.dispatched = true
	msg := `check type "` + req.GetCheckType() + `" not enabled on this agent`
	return &pb.TaskResult{Success: false, Error: msg, DetailsJson: []byte(`{"success":false,"error":"` + strings.ReplaceAll(msg, `"`, `\"`) + `"}`)}, nil
}

// A check type the source agent does not run is "could not ask" (exit 1), not a failed network check
// (exit 2), for every type. The default chart leaves http off, so this is what
// `kubectl kconmon check a b --type http` meets on a fresh install.
func TestCheckCommandTypeTheAgentDoesNotRunExit1(t *testing.T) {
	defaultCaps := []string{"plane:tcp", "plane:udp", "plane:icmp", "plane:pmtu", "plane:dns", "plane:mtr"}
	for _, tc := range []struct {
		checkType string
		caps      []string
	}{
		{"http", defaultCaps},
		{"udp", []string{"plane:tcp", "plane:icmp", "plane:pmtu", "plane:dns", "plane:mtr"}},
		{"dns", []string{"plane:tcp", "plane:udp", "plane:icmp", "plane:pmtu", "plane:mtr"}},
		{"pmtu", []string{"plane:tcp", "plane:udp", "plane:icmp", "plane:dns", "plane:mtr"}},
	} {
		t.Run(tc.checkType, func(t *testing.T) {
			reg := controller.NewRegistry(30 * time.Second)
			reg.Register(model.AgentInfo{ID: "a1", NodeName: "node-1", Capabilities: tc.caps})
			reg.Register(model.AgentInfo{ID: "a2", NodeName: "node-2", Capabilities: tc.caps})
			disp := &refusingDispatcher{}
			m := metrics.NewPrometheusMetrics("test", prometheus.NewRegistry())
			mux := http.NewServeMux()
			mux.Handle("POST /api/v1/diagnostics", controller.NewDiagnosticsHandler(reg, disp, m, false, nil, nil))

			out, stderr, code := runCLI(t, mux, "check", "node-1", "node-2", "--type", tc.checkType)

			if code != exitError {
				t.Fatalf("exit=%d, want 1 (the agent does not run %s probes)\nout=%s\nerr=%s", code, tc.checkType, out, stderr)
			}
			if strings.Contains(out, "FAIL") {
				t.Errorf("an agent that does not run the check must not print a FAIL verdict:\n%s", out)
			}
			if disp.dispatched {
				t.Error("the controller dispatched a task the source agent does not advertise")
			}
		})
	}
}
