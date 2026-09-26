package agent

import (
	"context"
	"log/slog"
	"slices"
	"strings"
	"testing"

	pb "github.com/EsDmitrii/kconmon-ng/api/proto"
	"github.com/EsDmitrii/kconmon-ng/internal/checker"
	"github.com/EsDmitrii/kconmon-ng/internal/model"
)

const credURL = "https://probe:s3cret@example.com/health"

// A password in an http target URL is how a basic-auth endpoint is probed; it must not become a
// label value every scraper and dashboard can read.
func TestHTTPURLLabelRedactsThePassword(t *testing.T) {
	reg, handle := newTestRegistry(t)
	for _, u := range []string{credURL, "https://example.com/plain?x=1"} {
		handle(model.CheckResult{Type: model.CheckHTTP, Success: true, Source: "node-a",
			Details: []HTTPDetails{{URL: u, Method: "GET", StatusCode: 200, TotalTime: 1}}})
	}
	for _, family := range []string{"kconmon_ng_http_results_total", "kconmon_ng_http_total_duration_seconds"} {
		got := labelValues(t, reg, family, "url")
		if len(got) != 2 {
			t.Fatalf("%s url labels = %v, want 2 series", family, got)
		}
		for _, v := range got {
			if strings.Contains(v, "s3cret") {
				t.Errorf("%s carries the password in its url label: %q", family, v)
			}
		}
		if !slices.Contains(got, "https://probe:xxxxx@example.com/health") || !slices.Contains(got, "https://example.com/plain?x=1") {
			t.Errorf("%s url labels = %v, want the user kept, the password masked and plain URLs verbatim", family, got)
		}
	}
}

func TestCheckFailureLogRedactsThePassword(t *testing.T) {
	logs := &lockedBuffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	s := NewScheduler(checker.Target{NodeName: "node-a"}, nil)
	s.logFailure(&model.CheckResult{Type: model.CheckHTTP, Source: "node-a", Error: "HTTP check " + credURL + " failed"})
	out := logs.String()
	if strings.Contains(out, "s3cret") || !strings.Contains(out, "probe:xxxxx@example.com") {
		t.Fatalf("the failure log must mask the password and keep the rest:\n%s", out)
	}
}

// The on-demand result goes to the controller and on to the Console and the CLI.
func TestTaskResultRedactsThePassword(t *testing.T) {
	fc := &fakeChecker{name: model.CheckHTTP, result: model.CheckResult{
		Error:   "HTTP check " + credURL + ": unexpected status 500",
		Details: []HTTPDetails{{URL: credURL, Method: "GET", StatusCode: 500}},
	}}
	ex := NewTaskExecutor(map[model.CheckType]checker.Checker{model.CheckHTTP: fc}, nil,
		checker.Target{NodeName: "node-a"}, testOwnPorts, nil, 1, ExternalPolicy{})
	res := ex.executeOne(context.Background(), &pb.TaskRequest{TaskId: "t", CheckType: string(model.CheckHTTP)})
	if strings.Contains(res.GetError(), "s3cret") || strings.Contains(string(res.GetDetailsJson()), "s3cret") {
		t.Fatalf("the task result carries the password: error=%q details=%s", res.GetError(), res.GetDetailsJson())
	}
	if !strings.Contains(string(res.GetDetailsJson()), "probe:xxxxx@example.com") {
		t.Errorf("the redacted URL lost more than the password: %s", res.GetDetailsJson())
	}
}
