package config

import (
	"slices"
	"strings"
	"testing"
)

// Who may name a client in X-Forwarded-For and who may assert an identity in header mode are two
// decisions. clientAddress.trustedProxyCIDRs takes the first; auth.header.trustedProxyCIDRs keeps the
// second, and still answers the first while the new list is empty, so an existing config reads the
// same client address as before.
func TestForwardingProxyCIDRsAreTheirOwnList(t *testing.T) {
	cfg, err := loadYAML(t, "auth:\n  mode: anonymous\n  header:\n    trustedProxyCIDRs: [\"10.0.0.5/32\"]\n")
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.ForwardingProxyCIDRs(); !slices.Equal(got, []string{"10.0.0.5/32"}) {
		t.Errorf("with clientAddress unset, ForwardingProxyCIDRs = %v, want the header list [10.0.0.5/32]", got)
	}

	cfg, err = loadYAML(t, "clientAddress:\n  trustedProxyCIDRs: [\"10.244.0.0/16\"]\n"+
		"auth:\n  mode: header\n  header:\n    trustedProxyCIDRs: [\"10.0.0.5/32\"]\n")
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.ForwardingProxyCIDRs(); !slices.Equal(got, []string{"10.244.0.0/16"}) {
		t.Errorf("ForwardingProxyCIDRs = %v, want clientAddress.trustedProxyCIDRs [10.244.0.0/16]", got)
	}
	if got := cfg.Auth.Header.TrustedProxyCIDRs; !slices.Equal(got, []string{"10.0.0.5/32"}) {
		t.Errorf("auth.header.trustedProxyCIDRs = %v, want it untouched [10.0.0.5/32]", got)
	}

	var none *Config
	if got := none.ForwardingProxyCIDRs(); got != nil {
		t.Errorf("a nil config forwards through %v, want nothing", got)
	}
}

func TestClientAddressTrustedProxyCIDRsAreValidated(t *testing.T) {
	_, err := loadYAML(t, "clientAddress:\n  trustedProxyCIDRs: [\"10.0.0.5\"]\n")
	if err == nil || !strings.Contains(err.Error(), "clientAddress.trustedProxyCIDRs") {
		t.Fatalf("error = %v, want one naming clientAddress.trustedProxyCIDRs", err)
	}

	// The client-address list never stands in for the identity list header mode requires.
	_, err = loadYAML(t, "clientAddress:\n  trustedProxyCIDRs: [\"10.244.0.0/16\"]\nauth:\n  mode: header\n")
	if err == nil || !strings.Contains(err.Error(), "auth.header.trustedProxyCIDRs") {
		t.Fatalf("header mode with only clientAddress.trustedProxyCIDRs: error = %v, want the header list required", err)
	}
}
