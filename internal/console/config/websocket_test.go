package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func loadYAML(t *testing.T, body string) (*Config, error) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return Load(p)
}

// The socket caps are on out of the box: a console with no websocket block still refuses a flood.
func TestLoadWebSocketLimitDefaults(t *testing.T) {
	cfg, err := loadYAML(t, "")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	ws := cfg.WebSocket
	if ws.MaxConnections != 1024 || ws.MaxConnectionsPerAddress != 256 || ws.MaxConnectionsPerSubject != 32 {
		t.Errorf("websocket defaults = %+v, want maxConnections 1024, perAddress 256, perSubject 32", ws)
	}
}

func TestValidateWebSocketLimits(t *testing.T) {
	for _, tc := range []struct {
		name, yaml string
		wantErr    string
	}{
		{"zero turns a cap off", "websocket:\n  maxConnections: 0\n  maxConnectionsPerAddress: 0\n  maxConnectionsPerSubject: 0\n", ""},
		{"overrides", "websocket:\n  maxConnections: 5000\n  maxConnectionsPerAddress: 1000\n  maxConnectionsPerSubject: 8\n", ""},
		{"negative total", "websocket:\n  maxConnections: -1\n", "websocket.maxConnections"},
		{"negative per address", "websocket:\n  maxConnectionsPerAddress: -1\n", "websocket.maxConnectionsPerAddress"},
		{"negative per subject", "websocket:\n  maxConnectionsPerSubject: -1\n", "websocket.maxConnectionsPerSubject"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadYAML(t, tc.yaml)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("expected the config to validate, got: %v", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Fatalf("error = %v, want one naming %s", err, tc.wantErr)
			}
		})
	}
}

/*
The trusted-proxy list decides which forwarding header clientIP believes, and it does so in every
auth mode (rate limits, the socket caps, the audit address). Outside header mode an invalid entry
used to be dropped without a word, so the operator's proxy was never trusted and every client behind
it shared one address.
*/
func TestTrustedProxyCIDRsAreValidatedInEveryMode(t *testing.T) {
	const dsn = "database:\n  dsn: \"postgres://host/db\"\n"
	const oidc = "  oidc:\n    issuer: \"https://idp.example.com\"\n    clientID: \"c\"\n    clientSecretFile: \"/f\"\n" +
		"    redirectURL: \"https://console.example.com/api/v1/auth/oidc/callback\"\n"
	for _, tc := range []struct{ mode, extraAuth, rest string }{
		{"anonymous", "", ""},
		{"header", "", ""},
		{"local", "", dsn},
		{"oidc", oidc, dsn},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			body := "auth:\n  mode: " + tc.mode + "\n" + tc.extraAuth +
				"  header:\n    trustedProxyCIDRs: [\"10.0.0.0/8\", \"not-a-cidr\"]\n" + tc.rest
			_, err := loadYAML(t, body)
			if err == nil || !strings.Contains(err.Error(), "auth.header.trustedProxyCIDRs") {
				t.Fatalf("error = %v, want one naming auth.header.trustedProxyCIDRs", err)
			}
		})
	}
	cfg, err := loadYAML(t, "auth:\n  mode: anonymous\n  header:\n    trustedProxyCIDRs: [\"10.0.0.0/8\"]\n")
	if err != nil {
		t.Fatalf("a valid list outside header mode was refused: %v", err)
	}
	if got := cfg.Auth.Header.TrustedProxyCIDRs; len(got) != 1 || got[0] != "10.0.0.0/8" {
		t.Errorf("trustedProxyCIDRs = %v, want [10.0.0.0/8]", got)
	}
}
