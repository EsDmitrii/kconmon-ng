package httpapi

import "testing"

// The browser builds its PromQL against the default kconmon_ng_ prefix; the proxy renames metric
// names to the configured prefix and leaves everything that is not a metric name alone.
func TestRewriteMetricsPrefix(t *testing.T) {
	cases := []struct {
		name, prefix, in, want string
	}{
		{"default prefix is a no-op", "kconmon_ng",
			`rate(kconmon_ng_tcp_results_total[5m])`, `rate(kconmon_ng_tcp_results_total[5m])`},
		{"bare selector", "netmon", `kconmon_ng_pmtu_bytes`, `netmon_pmtu_bytes`},
		{"inside functions and aggregations", "netmon",
			`sum by (source_node, destination_node) (rate(kconmon_ng_tcp_results_total{result="fail"}[5m])) / sum by (source_node, destination_node) (rate(kconmon_ng_tcp_results_total[5m]))`,
			`sum by (source_node, destination_node) (rate(netmon_tcp_results_total{result="fail"}[5m])) / sum by (source_node, destination_node) (rate(netmon_tcp_results_total[5m]))`},
		{"string literals and label values stay", "netmon",
			`label_replace(kconmon_ng_udp_packet_loss_ratio{scope="kconmon_ng_x", k='kconmon_ng_y'}, "dst", "kconmon_ng_$1", "src", "(.*)")`,
			`label_replace(netmon_udp_packet_loss_ratio{scope="kconmon_ng_x", k='kconmon_ng_y'}, "dst", "kconmon_ng_$1", "src", "(.*)")`},
		{"escaped quote does not end the string", "netmon",
			`kconmon_ng_a{l="x\"kconmon_ng_b"} or kconmon_ng_c`, `netmon_a{l="x\"kconmon_ng_b"} or netmon_c`},
		{"raw string", "netmon", "kconmon_ng_a{l=`kconmon_ng_b`}", "netmon_a{l=`kconmon_ng_b`}"},
		{"label names in matchers and grouping stay", "netmon",
			`count by (kconmon_ng_rule_id) (ALERTS{kconmon_ng_rule_id!=""}) * on (kconmon_ng_rule_id) group_left (kconmon_ng_rule_id) kconmon_ng_up`,
			`count by (kconmon_ng_rule_id) (ALERTS{kconmon_ng_rule_id!=""}) * on (kconmon_ng_rule_id) group_left (kconmon_ng_rule_id) netmon_up`},
		{"grouping keywords are case-insensitive", "netmon",
			`sum BY(kconmon_ng_rule_id)(kconmon_ng_a)`, `sum BY(kconmon_ng_rule_id)(netmon_a)`},
		{"group_left without a list", "netmon",
			`kconmon_ng_a * on (node) group_left kconmon_ng_b`, `netmon_a * on (node) group_left netmon_b`},
		{"token boundary", "netmon",
			`my_kconmon_ng_x + kconmon_ngx + kconmon_ng + kconmon_ng:rec + 5kconmon_ng_z`,
			`my_kconmon_ng_x + kconmon_ngx + kconmon_ng + kconmon_ng:rec + 5kconmon_ng_z`},
		{"comments stay", "netmon", "kconmon_ng_a # kconmon_ng_b\n+ kconmon_ng_c", "netmon_a # kconmon_ng_b\n+ netmon_c"},
		{"a prefix that extends the default is not applied twice", "kconmon_ng_eu",
			`kconmon_ng_eu_tcp_results_total + kconmon_ng_tcp_results_total`,
			`kconmon_ng_eu_tcp_results_total + kconmon_ng_eu_tcp_results_total`},
		{"unterminated string is copied as is", "netmon", `kconmon_ng_a{l="kconmon_ng_b`, `netmon_a{l="kconmon_ng_b`},
		{"a prefix that is a string prefix of the default", "kconmon",
			`rate(kconmon_ng_tcp_results_total[5m]) + kconmon_tcp_results_total`,
			`rate(kconmon_tcp_results_total[5m]) + kconmon_tcp_results_total`},
		{"a one-letter prefix", "k", `kconmon_ng_up`, `k_up`},
		{"a prefix ending in an underscore", "kconmon_", `kconmon_ng_up`, `kconmon__up`},
		{"a prefix sharing the default's first letters", "kconmon_n", `kconmon_ng_up`, `kconmon_n_up`},
	}
	for _, c := range cases {
		if got := rewriteMetricsPrefix(c.in, c.prefix); got != c.want {
			t.Errorf("%s:\n got  %s\n want %s", c.name, got, c.want)
		}
	}
}
