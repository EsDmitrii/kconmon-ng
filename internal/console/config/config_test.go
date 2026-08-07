package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoadDefaultsWhenFileMissing(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "nope.yaml"))
	if err != nil {
		t.Fatalf("expected defaults, got error: %v", err)
	}
	if cfg.HTTPPort != 8080 {
		t.Errorf("HTTPPort default = %d, want 8080", cfg.HTTPPort)
	}
	if cfg.LogLevel != "info" || cfg.LogFormat != "json" {
		t.Errorf("log defaults = %q/%q, want info/json", cfg.LogLevel, cfg.LogFormat)
	}
	if cfg.MetricsPrefix != "kconmon_ng" {
		t.Errorf("MetricsPrefix default = %q, want kconmon_ng", cfg.MetricsPrefix)
	}
	if cfg.Auth.Mode != "anonymous" || cfg.Auth.Anonymous.Role != "viewer" {
		t.Errorf("auth defaults = %q/%q, want anonymous/viewer", cfg.Auth.Mode, cfg.Auth.Anonymous.Role)
	}
}

func TestLoadFromFileOverrides(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte("httpPort: 9000\nlogLevel: debug\nauth:\n  mode: anonymous\n  anonymous:\n    role: operator\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.HTTPPort != 9000 || cfg.LogLevel != "debug" || cfg.Auth.Anonymous.Role != "operator" {
		t.Errorf("overrides not applied: %+v", cfg)
	}
}

func TestValidateRejectsIncompleteOIDCMode(t *testing.T) {
	// auth.mode=oidc is supported since M3, but this file supplies none of
	// oidc's required fields (issuer, clientID, clientSecretFile,
	// redirectURL) and no database DSN, so it must still fail validation.
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte("auth:\n  mode: oidc\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(p); err == nil {
		t.Fatal("expected error for incomplete auth.mode=oidc config, got nil")
	}
}

// TestAnonymousDefaultsUnchanged is the M1/M2 regression: a defaulted Config
// (no config file at all) must still resolve to mode=anonymous/role=viewer
// and pass Validate() outright — the M3 auth matrix must not make the
// degraded-state default invalid.
func TestAnonymousDefaultsUnchanged(t *testing.T) {
	cfg := defaults()
	if cfg.Auth.Mode != "anonymous" || cfg.Auth.Anonymous.Role != "viewer" {
		t.Fatalf("auth defaults = %q/%q, want anonymous/viewer", cfg.Auth.Mode, cfg.Auth.Anonymous.Role)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("defaulted config must pass Validate(), got: %v", err)
	}
}

func TestValidateAnonymousModeRequiresRole(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte("auth:\n  mode: anonymous\n  anonymous:\n    role: \"\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(p); err == nil {
		t.Fatal("expected error for auth.anonymous.role empty, got nil")
	}
}

func TestValidateHeaderMode(t *testing.T) {
	for _, tc := range []struct {
		name    string
		yaml    string
		wantErr bool
	}{
		{
			name: "valid",
			yaml: "auth:\n  mode: header\n  header:\n    trustedProxyCIDRs: [\"10.0.0.0/8\"]\n",
		},
		{
			name:    "empty trustedProxyCIDRs rejected",
			yaml:    "auth:\n  mode: header\n",
			wantErr: true,
		},
		{
			name:    "non-CIDR entry rejected",
			yaml:    "auth:\n  mode: header\n  header:\n    trustedProxyCIDRs: [\"not-a-cidr\"]\n",
			wantErr: true,
		},
		{
			name:    "empty userHeader rejected",
			yaml:    "auth:\n  mode: header\n  header:\n    userHeader: \"\"\n    trustedProxyCIDRs: [\"10.0.0.0/8\"]\n",
			wantErr: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(p, []byte(tc.yaml), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := Load(p)
			if tc.wantErr && err == nil {
				t.Fatal("expected validation error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("expected no error, got %v", err)
			}
		})
	}
}

func TestValidateHeaderDefaults(t *testing.T) {
	cfg := defaults()
	if cfg.Auth.Header.UserHeader != "X-Remote-User" {
		t.Errorf("header.userHeader default = %q, want X-Remote-User", cfg.Auth.Header.UserHeader)
	}
	if cfg.Auth.Header.GroupsHeader != "X-Remote-Groups" {
		t.Errorf("header.groupsHeader default = %q, want X-Remote-Groups", cfg.Auth.Header.GroupsHeader)
	}
	if cfg.Auth.Header.GroupsDelimiter != "," {
		t.Errorf("header.groupsDelimiter default = %q, want ,", cfg.Auth.Header.GroupsDelimiter)
	}
}

func TestValidateLocalModeRequiresDatabase(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte("auth:\n  mode: local\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(p); err == nil {
		t.Fatal("expected error for auth.mode=local with database disabled, got nil")
	}
}

func TestValidateLocalModeWithDatabasePasses(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.yaml")
	y := "auth:\n  mode: local\ndatabase:\n  dsn: \"postgres://host/db\"\n"
	if err := os.WriteFile(p, []byte(y), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(p); err != nil {
		t.Fatalf("expected auth.mode=local with database dsn to pass, got: %v", err)
	}
}

func validOIDCYAML() string {
	return "auth:\n" +
		"  mode: oidc\n" +
		"  oidc:\n" +
		"    issuer: \"https://idp.example.com\"\n" +
		"    clientID: \"kconmon-console\"\n" +
		"    clientSecretFile: \"/run/secrets/oidc-client-secret\"\n" +
		"    redirectURL: \"https://console.example.com/api/v1/auth/oidc/callback\"\n" +
		"database:\n" +
		"  dsn: \"postgres://host/db\"\n"
}

func TestValidateOIDCModeValidPasses(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte(validOIDCYAML()), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(p); err != nil {
		t.Fatalf("expected valid oidc config to pass, got: %v", err)
	}
}

func TestValidateOIDCModeRequiredFields(t *testing.T) {
	for _, tc := range []struct {
		name string
		yaml string
	}{
		{
			name: "missing database DSN",
			yaml: "auth:\n  mode: oidc\n  oidc:\n    issuer: \"https://idp.example.com\"\n" +
				"    clientID: \"c\"\n    clientSecretFile: \"/f\"\n" +
				"    redirectURL: \"https://console.example.com/api/v1/auth/oidc/callback\"\n",
		},
		{
			name: "issuer not https",
			yaml: "auth:\n  mode: oidc\n  oidc:\n    issuer: \"http://idp.example.com\"\n" +
				"    clientID: \"c\"\n    clientSecretFile: \"/f\"\n" +
				"    redirectURL: \"https://console.example.com/api/v1/auth/oidc/callback\"\n" +
				"database:\n  dsn: \"postgres://host/db\"\n",
		},
		{
			name: "issuer trailing slash rejected",
			yaml: "auth:\n  mode: oidc\n  oidc:\n    issuer: \"https://idp.example.com/\"\n" +
				"    clientID: \"c\"\n    clientSecretFile: \"/f\"\n" +
				"    redirectURL: \"https://console.example.com/api/v1/auth/oidc/callback\"\n" +
				"database:\n  dsn: \"postgres://host/db\"\n",
		},
		{
			name: "missing clientID",
			yaml: "auth:\n  mode: oidc\n  oidc:\n    issuer: \"https://idp.example.com\"\n" +
				"    clientSecretFile: \"/f\"\n" +
				"    redirectURL: \"https://console.example.com/api/v1/auth/oidc/callback\"\n" +
				"database:\n  dsn: \"postgres://host/db\"\n",
		},
		{
			name: "missing clientSecretFile",
			yaml: "auth:\n  mode: oidc\n  oidc:\n    issuer: \"https://idp.example.com\"\n" +
				"    clientID: \"c\"\n" +
				"    redirectURL: \"https://console.example.com/api/v1/auth/oidc/callback\"\n" +
				"database:\n  dsn: \"postgres://host/db\"\n",
		},
		{
			name: "redirectURL not absolute",
			yaml: "auth:\n  mode: oidc\n  oidc:\n    issuer: \"https://idp.example.com\"\n" +
				"    clientID: \"c\"\n    clientSecretFile: \"/f\"\n" +
				"    redirectURL: \"/api/v1/auth/oidc/callback\"\n" +
				"database:\n  dsn: \"postgres://host/db\"\n",
		},
		{
			name: "redirectURL wrong path",
			yaml: "auth:\n  mode: oidc\n  oidc:\n    issuer: \"https://idp.example.com\"\n" +
				"    clientID: \"c\"\n    clientSecretFile: \"/f\"\n" +
				"    redirectURL: \"https://console.example.com/wrong\"\n" +
				"database:\n  dsn: \"postgres://host/db\"\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(p, []byte(tc.yaml), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(p); err == nil {
				t.Fatal("expected validation error, got nil")
			}
		})
	}
}

func TestValidateOIDCDefaults(t *testing.T) {
	cfg := defaults()
	if len(cfg.Auth.OIDC.Scopes) != 4 {
		t.Fatalf("oidc.scopes default = %v, want 4 entries", cfg.Auth.OIDC.Scopes)
	}
	wantScopes := []string{"openid", "profile", "email", "groups"}
	for i, s := range wantScopes {
		if cfg.Auth.OIDC.Scopes[i] != s {
			t.Errorf("oidc.scopes[%d] = %q, want %q", i, cfg.Auth.OIDC.Scopes[i], s)
		}
	}
	if cfg.Auth.OIDC.UsernameClaim != "preferred_username" {
		t.Errorf("oidc.usernameClaim default = %q, want preferred_username", cfg.Auth.OIDC.UsernameClaim)
	}
	if cfg.Auth.OIDC.GroupsClaim != "groups" {
		t.Errorf("oidc.groupsClaim default = %q, want groups", cfg.Auth.OIDC.GroupsClaim)
	}
}

func TestValidateSessionDefaults(t *testing.T) {
	cfg := defaults()
	if cfg.Auth.Session.TTL != 12*time.Hour {
		t.Errorf("session.ttl default = %v, want 12h", cfg.Auth.Session.TTL)
	}
	if cfg.Auth.Session.CookieName != "__Host-kconmon_session" {
		t.Errorf("session.cookieName default = %q, want __Host-kconmon_session", cfg.Auth.Session.CookieName)
	}
	if !cfg.Auth.Session.Secure {
		t.Error("session.secure default = false, want true")
	}
}

func TestValidateSessionTTLMustBePositive(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte("auth:\n  session:\n    ttl: 0s\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(p); err == nil {
		t.Fatal("expected error for auth.session.ttl=0, got nil")
	}
}

func TestValidateHostCookiePrefixRequiresSecure(t *testing.T) {
	for _, tc := range []struct {
		name    string
		yaml    string
		wantErr bool
	}{
		{
			name:    "__Host- prefix with secure=false rejected",
			yaml:    "auth:\n  session:\n    cookieName: \"__Host-kconmon_session\"\n    secure: false\n",
			wantErr: true,
		},
		{
			name: "__Host- prefix with secure=true (default) passes",
			yaml: "auth:\n  session:\n    cookieName: \"__Host-kconmon_session\"\n",
		},
		{
			name: "non-__Host- cookie name with secure=false passes",
			yaml: "auth:\n  session:\n    cookieName: \"kconmon_session\"\n    secure: false\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(p, []byte(tc.yaml), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := Load(p)
			if tc.wantErr && err == nil {
				t.Fatal("expected validation error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("expected no error, got %v", err)
			}
		})
	}
}

func TestValidateDefaultRole(t *testing.T) {
	for _, tc := range []struct {
		name    string
		yaml    string
		wantErr bool
	}{
		{name: "empty passes", yaml: "auth:\n  defaultRole: \"\"\n"},
		{name: "viewer passes", yaml: "auth:\n  defaultRole: \"viewer\"\n"},
		{name: "operator passes", yaml: "auth:\n  defaultRole: \"operator\"\n"},
		{name: "alert-editor passes", yaml: "auth:\n  defaultRole: \"alert-editor\"\n"},
		{name: "admin passes", yaml: "auth:\n  defaultRole: \"admin\"\n"},
		{name: "typo rejected", yaml: "auth:\n  defaultRole: \"viewr\"\n", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(p, []byte(tc.yaml), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := Load(p)
			if tc.wantErr && err == nil {
				t.Fatal("expected validation error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("expected no error, got %v", err)
			}
		})
	}
}

func TestValidateUnknownAuthModeRejected(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte("auth:\n  mode: bogus\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(p); err == nil {
		t.Fatal("expected error for unknown auth.mode, got nil")
	}
}

func TestValidateRejectsBadPort(t *testing.T) {
	c := &Config{HTTPPort: 0, LogLevel: "info", LogFormat: "json", MetricsPrefix: "kconmon_ng", Auth: AuthConfig{Mode: "anonymous"}}
	if err := c.Validate(); err == nil {
		t.Fatal("expected error for httpPort 0")
	}
}

func TestLoadControllerPrometheusDefaults(t *testing.T) {
	cfg, err := Load("/nonexistent/config.yaml")
	if err != nil {
		t.Fatalf("Load defaults: %v", err)
	}
	if cfg.Controller.URL != "" || cfg.Prometheus.URL != "" {
		t.Errorf("controller/prometheus URLs must default empty, got %q %q", cfg.Controller.URL, cfg.Prometheus.URL)
	}
	if cfg.Controller.Timeout != 10*time.Second {
		t.Errorf("controller.timeout default: got %v", cfg.Controller.Timeout)
	}
	if cfg.Prometheus.QueryTimeout != 30*time.Second || cfg.Prometheus.MaxRange != 24*time.Hour || cfg.Prometheus.MaxResponseBytes != 8<<20 {
		t.Errorf("prometheus defaults wrong: %+v", cfg.Prometheus)
	}
}

func TestValidateRejectsBadURLs(t *testing.T) {
	for _, tc := range []struct{ name, yaml string }{
		{"controller not http", "controller:\n  url: \"ftp://x\"\n"},
		{"prometheus not absolute", "prometheus:\n  url: \"prometheus:9090\"\n"},
		{"nonpositive timeout", "controller:\n  url: \"http://c:8080\"\n  timeout: -1s\n"},
		{"nonpositive maxRange", "prometheus:\n  url: \"http://p:9090\"\n  maxRange: 0s\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			p := filepath.Join(dir, "config.yaml")
			if err := os.WriteFile(p, []byte(tc.yaml), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(p); err == nil {
				t.Fatal("expected validation error, got nil")
			}
		})
	}
}

func TestLoadControllerGRPCAddrAndValkeyDefaults(t *testing.T) {
	cfg, err := Load("/nonexistent/config.yaml")
	if err != nil {
		t.Fatalf("Load defaults: %v", err)
	}
	if cfg.Controller.GRPCAddr != "" {
		t.Errorf("controller.grpcAddr must default empty, got %q", cfg.Controller.GRPCAddr)
	}
	if cfg.Valkey.Address != "" {
		t.Errorf("valkey.address must default empty, got %q", cfg.Valkey.Address)
	}
	if cfg.Valkey.DialTimeout != 5*time.Second {
		t.Errorf("valkey.dialTimeout default: got %v", cfg.Valkey.DialTimeout)
	}
}

func TestLoadValkeyAddress(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.yaml")
	y := "valkey:\n  address: \"kconmon-ng-console-valkey:6379\"\n  dialTimeout: 2s\n" +
		"controller:\n  grpcAddr: \"kconmon-ng-controller:9090\"\n"
	if err := os.WriteFile(p, []byte(y), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Valkey.Address != "kconmon-ng-console-valkey:6379" || cfg.Valkey.DialTimeout != 2*time.Second {
		t.Errorf("valkey config not applied: %+v", cfg.Valkey)
	}
	if cfg.Controller.GRPCAddr != "kconmon-ng-controller:9090" {
		t.Errorf("controller.grpcAddr not applied: %q", cfg.Controller.GRPCAddr)
	}
}

func TestValidateRejectsBadValkeyAddress(t *testing.T) {
	for _, tc := range []struct{ name, yaml string }{
		{"missing port", "valkey:\n  address: \"not-a-host-port\"\n"},
		{"nonpositive dialTimeout", "valkey:\n  address: \"v:6379\"\n  dialTimeout: 0s\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(p, []byte(tc.yaml), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(p); err == nil {
				t.Fatal("expected validation error, got nil")
			}
		})
	}
}

func TestLoadRejectsUnknownKeys(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte("httpPrt: 9000\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(p); err == nil {
		t.Fatal("expected error for unknown key httpPrt, got nil")
	}
}

func TestLoadDatabaseDefaults(t *testing.T) {
	cfg, err := Load("/nonexistent/config.yaml")
	if err != nil {
		t.Fatalf("Load defaults: %v", err)
	}
	if cfg.Database.DSN != "" || cfg.Database.DSNFile != "" {
		t.Errorf("database dsn/dsnFile must default empty, got %q %q", cfg.Database.DSN, cfg.Database.DSNFile)
	}
	if cfg.Database.MaxConns != 10 {
		t.Errorf("database.maxConns default = %d, want 10", cfg.Database.MaxConns)
	}
	if cfg.Database.ConnectTimeout != 10*time.Second {
		t.Errorf("database.connectTimeout default = %v, want 10s", cfg.Database.ConnectTimeout)
	}
	if !cfg.Database.MigrateOnStart {
		t.Error("database.migrateOnStart default = false, want true")
	}
	if cfg.Database.RetentionDays != 90 {
		t.Errorf("database.retentionDays default = %d, want 90", cfg.Database.RetentionDays)
	}
}

func TestLoadDatabaseFromYAML(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.yaml")
	y := "database:\n" +
		"  dsn: \"postgres://kconmon@db:5432/kconmon?sslmode=require\"\n" +
		"  maxConns: 4\n" +
		"  connectTimeout: 3s\n" +
		"  migrateOnStart: false\n" +
		"  retentionDays: 30\n"
	if err := os.WriteFile(p, []byte(y), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Database.DSN != "postgres://kconmon@db:5432/kconmon?sslmode=require" {
		t.Errorf("database.dsn not applied: %q", cfg.Database.DSN)
	}
	if cfg.Database.MaxConns != 4 {
		t.Errorf("database.maxConns not applied: %d", cfg.Database.MaxConns)
	}
	if cfg.Database.ConnectTimeout != 3*time.Second {
		t.Errorf("database.connectTimeout not applied: %v", cfg.Database.ConnectTimeout)
	}
	if cfg.Database.MigrateOnStart {
		t.Error("database.migrateOnStart not applied: still true")
	}
	if cfg.Database.RetentionDays != 30 {
		t.Errorf("database.retentionDays not applied: %d", cfg.Database.RetentionDays)
	}
}

func TestValidateRejectsNonPostgresDSN(t *testing.T) {
	for _, tc := range []struct {
		name    string
		dsn     string
		wantErr bool
	}{
		{"mysql scheme", "mysql://x/y", true},
		{"not a url at all", "not a url at all", true},
		{"postgres scheme ok", "postgres://host/db", false},
		{"postgresql scheme ok", "postgresql://host/db", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "config.yaml")
			y := "database:\n  dsn: \"" + tc.dsn + "\"\n"
			if err := os.WriteFile(p, []byte(y), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := Load(p)
			if tc.wantErr && err == nil {
				t.Fatalf("expected validation error for dsn %q, got nil", tc.dsn)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("expected no error for dsn %q, got %v", tc.dsn, err)
			}
		})
	}
}

func TestValidateRejectsPasswordInInlineDSN(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte("database:\n  dsn: \"postgres://u:hunter2@db/x\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Load(p)
	if err == nil {
		t.Fatal("expected error for dsn with embedded password, got nil")
	}
	if !strings.Contains(err.Error(), "database.dsnFile") {
		t.Errorf("error should mention database.dsnFile, got: %v", err)
	}
}

func TestValidateRejectsBadDatabaseNumbers(t *testing.T) {
	for _, tc := range []struct {
		name    string
		yaml    string
		wantErr bool
	}{
		{"maxConns zero", "database:\n  maxConns: 0\n", true},
		{"connectTimeout zero", "database:\n  connectTimeout: 0s\n", true},
		{"retentionDays negative", "database:\n  retentionDays: -1\n", true},
		{"retentionDays zero passes", "database:\n  retentionDays: 0\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(p, []byte(tc.yaml), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := Load(p)
			if tc.wantErr && err == nil {
				t.Fatal("expected validation error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("expected no error, got %v", err)
			}
		})
	}
}

func TestValidateRejectsBothDSNAndDSNFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.yaml")
	y := "database:\n  dsn: \"postgres://host/db\"\n  dsnFile: \"/run/secrets/dsn\"\n"
	if err := os.WriteFile(p, []byte(y), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(p); err == nil {
		t.Fatal("expected error when both database.dsn and database.dsnFile are set, got nil")
	}
}

func TestResolveDSNReadsFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "dsn")
	if err := os.WriteFile(p, []byte("postgres://host/db\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	d := &DatabaseConfig{DSNFile: p}
	got, err := d.ResolveDSN()
	if err != nil {
		t.Fatalf("ResolveDSN: %v", err)
	}
	if got != "postgres://host/db" {
		t.Errorf("ResolveDSN() = %q, want trimmed %q", got, "postgres://host/db")
	}

	d2 := &DatabaseConfig{DSNFile: filepath.Join(dir, "missing")}
	_, err = d2.ResolveDSN()
	if err == nil {
		t.Fatal("expected error for missing dsnFile, got nil")
	}
	if !strings.Contains(err.Error(), d2.DSNFile) {
		t.Errorf("error should mention path %q, got: %v", d2.DSNFile, err)
	}
}

func TestResolveDSNEmptyWhenUnset(t *testing.T) {
	d := &DatabaseConfig{}
	got, err := d.ResolveDSN()
	if err != nil {
		t.Fatalf("ResolveDSN: %v", err)
	}
	if got != "" {
		t.Errorf("ResolveDSN() = %q, want empty", got)
	}
}
