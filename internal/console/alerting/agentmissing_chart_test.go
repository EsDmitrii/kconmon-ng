package alerting

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// chartAgentsMissingExpr reads the chart's KconmonAgentsMissing expression out of _rules.tpl and
// folds it the way YAML's `>-` does, with the prefix substituted.
func chartAgentsMissingExpr(t *testing.T, prefix string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "charts", "kconmon-ng", "templates", "_rules.tpl"))
	if err != nil {
		t.Fatalf("read chart rules: %v", err)
	}
	src := string(raw)
	start := strings.Index(src, "- alert: KconmonAgentsMissing")
	if start < 0 {
		t.Fatal("KconmonAgentsMissing not found in _rules.tpl")
	}
	rest := src[start:]
	exprAt := strings.Index(rest, "expr: >-")
	forAt := strings.Index(rest, "\n  for:")
	if exprAt < 0 || forAt < exprAt {
		t.Fatal("KconmonAgentsMissing has no folded expr block followed by for:")
	}
	block := rest[exprAt+len("expr: >-") : forAt]
	folded := strings.Join(strings.Fields(block), " ")
	return strings.ReplaceAll(folded, "{{ $prefix }}", prefix)
}

// The console's agent-missing template is the chart's KconmonAgentsMissing offered as a console
// rule, so both must page on the same condition: the leader guard and the external-agent
// subtraction included.
func TestAgentMissingRendersTheChartRuleExpression(t *testing.T) {
	for _, prefix := range []string{MetricPrefix, "acme_net"} {
		want := chartAgentsMissingExpr(t, prefix)
		got, err := NewRenderer(prefix).Render(Rule{Kind: KindAgentMissing})
		if err != nil {
			t.Fatalf("Render(agent-missing, %s): %v", prefix, err)
		}
		if got != want {
			t.Errorf("agent-missing with prefix %s:\n got: %s\nwant: %s", prefix, got, want)
		}
	}
}
