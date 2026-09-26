package httpapi

import (
	"strings"
)

// defaultMetricsPrefix is the prefix the web console builds its PromQL with.
const defaultMetricsPrefix = "kconmon_ng"

// rewriteMetricsPrefix renames metric names that carry the default kconmon_ng_ prefix to prefix, so
// the browser's queries find the series agents export under a custom config.metricsPrefix. String
// literals, comments, label matchers and grouping label lists are copied untouched: label names such
// as kconmon_ng_rule_id keep the default on every install.
func rewriteMetricsPrefix(query, prefix string) string {
	from := defaultMetricsPrefix + "_"
	if prefix == "" || prefix == defaultMetricsPrefix || !strings.Contains(query, from) {
		return query
	}
	// Only a prefix that extends the default (kconmon_ng_eu) can already be on a kconmon_ng_ token; a
	// shorter one (kconmon) matches every default name as a plain string prefix.
	custom := ""
	if strings.HasPrefix(prefix+"_", from) {
		custom = prefix + "_"
	}
	var b strings.Builder
	b.Grow(len(query))
	braces := 0          // depth inside {...} label matchers
	labelList := false   // inside the (...) of by/without/on/ignoring/group_left/group_right
	pendingList := false // the last token was one of those keywords; a "(" next opens a label list
	for i := 0; i < len(query); {
		c := query[i]
		switch {
		case c == '"' || c == '\'' || c == '`':
			j := skipPromQLString(query, i)
			b.WriteString(query[i:j])
			i, pendingList = j, false
			continue
		case c == '#':
			j := strings.IndexByte(query[i:], '\n')
			if j < 0 {
				j = len(query) - i
			}
			b.WriteString(query[i : i+j])
			i += j
			continue
		case isPromQLIdentByte(c):
			j := i + 1
			for j < len(query) && isPromQLIdentByte(query[j]) {
				j++
			}
			tok := query[i:j]
			if braces == 0 && !labelList && strings.HasPrefix(tok, from) && (custom == "" || !strings.HasPrefix(tok, custom)) {
				tok = prefix + tok[len(defaultMetricsPrefix):]
			}
			b.WriteString(tok)
			i, pendingList = j, braces == 0 && isGroupingKeyword(tok)
			continue
		case c == '{':
			braces++
		case c == '}':
			if braces > 0 {
				braces--
			}
		case c == '(':
			labelList = labelList || pendingList
		case c == ')':
			labelList = false
		}
		if c != ' ' && c != '\t' && c != '\n' && c != '\r' {
			pendingList = false
		}
		b.WriteByte(c)
		i++
	}
	return b.String()
}

// skipPromQLString returns the index just past the string literal that opens at query[start]; an
// unterminated literal runs to the end. Backquoted strings have no escapes.
func skipPromQLString(query string, start int) int {
	quote := query[start]
	for i := start + 1; i < len(query); i++ {
		switch query[i] {
		case '\\':
			if quote != '`' {
				i++
			}
		case quote:
			return i + 1
		}
	}
	return len(query)
}

func isPromQLIdentByte(c byte) bool {
	return c == '_' || c == ':' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}

func isGroupingKeyword(tok string) bool {
	switch strings.ToLower(tok) {
	case "by", "without", "on", "ignoring", "group_left", "group_right":
		return true
	}
	return false
}
