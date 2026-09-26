package config

import (
	"reflect"
	"sort"
	"strings"
)

// Diff lists the dotted YAML keys whose values differ between old and next, one per leaf field; a
// list (dns hosts, http targets, CIDRs) is one leaf. The result is sorted.
func Diff(old, next *Config) []string {
	var keys []string
	diffValue("", reflect.ValueOf(*old), reflect.ValueOf(*next), &keys)
	sort.Strings(keys)
	return keys
}

func diffValue(prefix string, a, b reflect.Value, keys *[]string) {
	if a.Kind() != reflect.Struct {
		if !reflect.DeepEqual(a.Interface(), b.Interface()) {
			*keys = append(*keys, prefix)
		}
		return
	}
	t := a.Type()
	for i := range t.NumField() {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		name, _, _ := strings.Cut(f.Tag.Get("yaml"), ",")
		if name == "" || name == "-" {
			name = f.Name
		}
		key := name
		if prefix != "" {
			key = prefix + "." + name
		}
		diffValue(key, a.Field(i), b.Field(i), keys)
	}
}

// KeyMatches reports whether key is one of patterns, where a pattern ending in "." names a whole
// subtree ("checkers." matches "checkers.tcp.interval").
func KeyMatches(key string, patterns ...string) bool {
	for _, p := range patterns {
		if strings.HasSuffix(p, ".") {
			if strings.HasPrefix(key, p) {
				return true
			}
		} else if key == p {
			return true
		}
	}
	return false
}
