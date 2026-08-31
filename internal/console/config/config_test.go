package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
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
	// auth.mode=oidc is supported since , but this file supplies none of oidc's required fields
	// (issuer, clientID, clientSecretFile, redirectURL) and no database DSN.
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte("auth:\n  mode: oidc\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(p); err == nil {
		t.Fatal("expected error for incomplete auth.mode=oidc config, got nil")
	}
}

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

func TestLoadControllerGRPCAddrAndRedisDefaults(t *testing.T) {
	cfg, err := Load("/nonexistent/config.yaml")
	if err != nil {
		t.Fatalf("Load defaults: %v", err)
	}
	if cfg.Controller.GRPCAddr != "" {
		t.Errorf("controller.grpcAddr must default empty, got %q", cfg.Controller.GRPCAddr)
	}
	if cfg.Redis.DSN != "" || cfg.Redis.DSNFile != "" {
		t.Errorf("redis must default to no DSN, got %+v", cfg.Redis)
	}
	if cfg.Redis.DialTimeout != 5*time.Second {
		t.Errorf("redis.dialTimeout default: got %v", cfg.Redis.DialTimeout)
	}
}

func TestLoadRedisDSN(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.yaml")
	y := "redis:\n  dsn: \"redis://kconmon-ng-console-valkey:6379\"\n  dialTimeout: 2s\n" +
		"controller:\n  grpcAddr: \"kconmon-ng-controller:9090\"\n"
	if err := os.WriteFile(p, []byte(y), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Redis.DSN != "redis://kconmon-ng-console-valkey:6379" || cfg.Redis.DialTimeout != 2*time.Second {
		t.Errorf("redis config not applied: %+v", cfg.Redis)
	}
	if cfg.Controller.GRPCAddr != "kconmon-ng-controller:9090" {
		t.Errorf("controller.grpcAddr not applied: %q", cfg.Controller.GRPCAddr)
	}
}

func TestValidateRejectsBadRedisConfig(t *testing.T) {
	for _, tc := range []struct{ name, yaml string }{
		{"nonpositive dialTimeout", "redis:\n  dsn: \"redis://v:6379\"\n  dialTimeout: 0s\n"},
		{"both dsn and dsnFile", "redis:\n  dsn: \"redis://v:6379\"\n  dsnFile: /etc/x\n"},
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

func TestLoadRateLimitDefaults(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "nope.yaml"))
	if err != nil {
		t.Fatalf("Load defaults: %v", err)
	}
	if cfg.RateLimit.RunsPerMinute != 10 {
		t.Errorf("rateLimit.runsPerMinute default = %d, want 10", cfg.RateLimit.RunsPerMinute)
	}
	if cfg.RateLimit.LoginPerMinute != 5 {
		t.Errorf("rateLimit.loginPerMinute default = %d, want 5", cfg.RateLimit.LoginPerMinute)
	}
}

func TestValidateRateLimits(t *testing.T) {
	for _, tc := range []struct {
		name    string
		yaml    string
		wantErr bool
	}{
		{"zero disables both", "rateLimit:\n  runsPerMinute: 0\n  loginPerMinute: 0\n", false},
		{"positive overrides", "rateLimit:\n  runsPerMinute: 100\n  loginPerMinute: 20\n", false},
		{"negative runs rejected", "rateLimit:\n  runsPerMinute: -1\n", true},
		{"negative login rejected", "rateLimit:\n  loginPerMinute: -5\n", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(p, []byte(tc.yaml), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := Load(p)
			if tc.wantErr && err == nil {
				t.Fatal("expected a validation error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("expected the config to validate, got: %v", err)
			}
		})
	}
}

// TestLoadRateLimitZeroIsHonoredNotDefaulted guards the one way an explicit "off" could silently
// come back on.
func TestLoadRateLimitZeroIsHonoredNotDefaulted(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte("rateLimit:\n  runsPerMinute: 0\n  loginPerMinute: 0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.RateLimit.RunsPerMinute != 0 || cfg.RateLimit.LoginPerMinute != 0 {
		t.Errorf("explicit zeros = %d/%d, want 0/0 (an explicit disable must survive defaulting)",
			cfg.RateLimit.RunsPerMinute, cfg.RateLimit.LoginPerMinute)
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

// TestLoadMTREnrichmentDefaults pins the off-by-default posture of the enrichment block: the master
// gate is false.
func TestLoadMTREnrichmentDefaults(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "nope.yaml"))
	if err != nil {
		t.Fatalf("Load defaults: %v", err)
	}
	e := cfg.MTR.Enrichment
	if e.Enabled {
		t.Error("mtr.enrichment.enabled default = true, want false (M5 Decision 4: off by default)")
	}
	if e.RDNS.Enabled {
		t.Error("mtr.enrichment.rdns.enabled default = true, want false")
	}
	if e.RDNS.TimeoutMs != 500 {
		t.Errorf("mtr.enrichment.rdns.timeoutMs default = %d, want 500", e.RDNS.TimeoutMs)
	}
	if e.GeoIP.ASNPath != "" || e.GeoIP.CityPath != "" {
		t.Errorf("mtr.enrichment.geoip defaults = %q/%q, want empty (empty = source off)", e.GeoIP.ASNPath, e.GeoIP.CityPath)
	}
	if e.TTL != 24*time.Hour {
		t.Errorf("mtr.enrichment.ttl default = %v, want 24h", e.TTL)
	}
}

// TestLoadMTREnrichmentFromYAML proves the plan-verbatim block parses under
// KnownFields(true) -- every key spelled exactly as the plan and the chart
// spell it. A typo here is a boot failure, not a silently ignored knob.
func TestLoadMTREnrichmentFromYAML(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.yaml")
	const y = "mtr:\n" +
		"  enrichment:\n" +
		"    enabled: true\n" +
		"    rdns:\n" +
		"      enabled: true\n" +
		"      timeoutMs: 250\n" +
		"    geoip:\n" +
		"      asnPath: /geoip/GeoLite2-ASN.mmdb\n" +
		"      cityPath: /geoip/GeoLite2-City.mmdb\n" +
		"    ttl: 6h\n"
	if err := os.WriteFile(p, []byte(y), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	e := cfg.MTR.Enrichment
	if !e.Enabled || !e.RDNS.Enabled {
		t.Errorf("enabled/rdns.enabled = %v/%v, want true/true", e.Enabled, e.RDNS.Enabled)
	}
	if e.RDNS.TimeoutMs != 250 {
		t.Errorf("rdns.timeoutMs = %d, want 250", e.RDNS.TimeoutMs)
	}
	if e.GeoIP.ASNPath != "/geoip/GeoLite2-ASN.mmdb" || e.GeoIP.CityPath != "/geoip/GeoLite2-City.mmdb" {
		t.Errorf("geoip paths = %q/%q", e.GeoIP.ASNPath, e.GeoIP.CityPath)
	}
	if e.TTL != 6*time.Hour {
		t.Errorf("ttl = %v, want 6h", e.TTL)
	}
}

// TestValidateMTREnrichment is the fail-closed table.
func TestValidateMTREnrichment(t *testing.T) {
	const enabledPrefix = "mtr:\n  enrichment:\n    enabled: true\n"
	for _, tc := range []struct {
		name    string
		yaml    string
		wantErr bool
	}{
		{"disabled ignores every other knob", "mtr:\n  enrichment:\n    enabled: false\n    ttl: 0s\n    rdns:\n      enabled: true\n      timeoutMs: 0\n", false},
		{"enabled with rdns only", enabledPrefix + "    rdns:\n      enabled: true\n", false},
		{"enabled with asn only", enabledPrefix + "    geoip:\n      asnPath: /geoip/asn.mmdb\n", false},
		{"enabled with city only", enabledPrefix + "    geoip:\n      cityPath: /geoip/city.mmdb\n", false},
		{"enabled with every source off", enabledPrefix, true},
		{"zero ttl rejected", enabledPrefix + "    rdns:\n      enabled: true\n    ttl: 0s\n", true},
		{"negative ttl rejected", enabledPrefix + "    rdns:\n      enabled: true\n    ttl: -1h\n", true},
		{"zero rdns timeout rejected", enabledPrefix + "    rdns:\n      enabled: true\n      timeoutMs: 0\n", true},
		{"negative rdns timeout rejected", enabledPrefix + "    rdns:\n      enabled: true\n      timeoutMs: -1\n", true},
		{"rdns timeout ignored when rdns is off", enabledPrefix + "    rdns:\n      timeoutMs: 0\n    geoip:\n      asnPath: /geoip/asn.mmdb\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(p, []byte(tc.yaml), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := Load(p)
			if tc.wantErr && err == nil {
				t.Fatal("expected a validation error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("expected the config to validate, got: %v", err)
			}
		})
	}
}

// TestValidateMTREnrichmentAllSourcesOffNamesTheKnobs: the error an operator
// reads must say WHICH switches to flip, not just that something is wrong.
func TestValidateMTREnrichmentAllSourcesOffNamesTheKnobs(t *testing.T) {
	c := defaults()
	c.MTR.Enrichment.Enabled = true
	err := c.Validate()
	if err == nil {
		t.Fatal("expected an error when every enrichment source is off, got nil")
	}
	for _, knob := range []string{"mtr.enrichment.rdns.enabled", "mtr.enrichment.geoip.asnPath", "mtr.enrichment.geoip.cityPath"} {
		if !strings.Contains(err.Error(), knob) {
			t.Errorf("error should name %s, got: %v", knob, err)
		}
	}
}

// TestLoadKubernetesContextDefaults pins the block: the gate is off, the namespace is empty (=
// resolve at runtime).
func TestLoadKubernetesContextDefaults(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "nope.yaml"))
	if err != nil {
		t.Fatalf("Load defaults: %v", err)
	}
	k := cfg.KubernetesContext
	if k.Enabled {
		t.Error("kubernetesContext.enabled default = true, want false (M6 Decision 3: off by default)")
	}
	if k.Namespace != "" {
		t.Errorf("kubernetesContext.namespace default = %q, want empty (empty = POD_NAMESPACE)", k.Namespace)
	}
	if k.ResyncInterval != 10*time.Minute {
		t.Errorf("kubernetesContext.resyncInterval default = %v, want 10m", k.ResyncInterval)
	}
}

// TestLoadKubernetesContextFromYAML proves the plan-verbatim block parses under
// KnownFields(true) -- every key spelled exactly as the plan and the chart
// spell it.
func TestLoadKubernetesContextFromYAML(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.yaml")
	const y = "kubernetesContext:\n" +
		"  enabled: true\n" +
		"  namespace: kconmon-ng\n" +
		"  resyncInterval: 2m\n"
	if err := os.WriteFile(p, []byte(y), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	k := cfg.KubernetesContext
	if !k.Enabled {
		t.Error("kubernetesContext.enabled = false, want true")
	}
	if k.Namespace != "kconmon-ng" {
		t.Errorf("kubernetesContext.namespace = %q, want kconmon-ng", k.Namespace)
	}
	if k.ResyncInterval != 2*time.Minute {
		t.Errorf("kubernetesContext.resyncInterval = %v, want 2m", k.ResyncInterval)
	}
}

// TestValidateKubernetesContext is the fail-closed table: a cadence that would
// fire a relist storm is rejected when the reader is on, and ignored when it
// is off.
func TestValidateKubernetesContext(t *testing.T) {
	for _, tc := range []struct {
		name    string
		yaml    string
		wantErr bool
	}{
		{"disabled ignores the interval", "kubernetesContext:\n  enabled: false\n  resyncInterval: 0s\n", false},
		{"enabled with the default interval", "kubernetesContext:\n  enabled: true\n", false},
		{"zero interval rejected", "kubernetesContext:\n  enabled: true\n  resyncInterval: 0s\n", true},
		{"negative interval rejected", "kubernetesContext:\n  enabled: true\n  resyncInterval: -1m\n", true},
		{"empty namespace is legal when enabled", "kubernetesContext:\n  enabled: true\n  namespace: \"\"\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(p, []byte(tc.yaml), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := Load(p)
			if tc.wantErr && err == nil {
				t.Fatal("expected a validation error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("expected the config to validate, got: %v", err)
			}
		})
	}
}

// TestValidateKubernetesContextNamesTheKnob: the error an operator reads must
// say WHICH key to fix.
func TestValidateKubernetesContextNamesTheKnob(t *testing.T) {
	c := defaults()
	c.KubernetesContext.Enabled = true
	c.KubernetesContext.ResyncInterval = 0
	err := c.Validate()
	if err == nil {
		t.Fatal("expected an error for a zero resyncInterval, got nil")
	}
	if !strings.Contains(err.Error(), "kubernetesContext.resyncInterval") {
		t.Errorf("error should name kubernetesContext.resyncInterval, got: %v", err)
	}
}

// TestResolveNamespacePrecedence pins the three-step fallback the reader and the chart both depend
// on; a whitespace-only POD_NAMESPACE is treated as unset, the same way ResolveDSN trims its file.
func TestResolveNamespacePrecedence(t *testing.T) {
	for _, tc := range []struct {
		name  string
		field string
		env   string
		want  string
	}{
		{"explicit namespace wins", "kconmon-ng", "other-ns", "kconmon-ng"},
		{"env is the fallback", "", "kconmon-ng", "kconmon-ng"},
		{"env is trimmed", "", "  kconmon-ng \n", "kconmon-ng"},
		{"blank env falls through to default", "", "   ", "default"},
		{"nothing set falls back to default", "", "", "default"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(podNamespaceEnv, tc.env)
			k := KubernetesContextConfig{Namespace: tc.field}
			if got := k.ResolveNamespace(); got != tc.want {
				t.Errorf("ResolveNamespace() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestLoadAlertingDefaults pins the block's defaults: the gate is off, the namespace is empty (=
// resolve from POD_NAMESPACE).
func TestLoadAlertingDefaults(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "nope.yaml"))
	if err != nil {
		t.Fatalf("Load defaults: %v", err)
	}
	a := cfg.Alerting
	if a.Enabled {
		t.Error("alerting.enabled default = true, want false (M7 Decision 3: off by default)")
	}
	if a.Namespace != "" {
		t.Errorf("alerting.namespace default = %q, want empty (empty = POD_NAMESPACE)", a.Namespace)
	}
	if a.SyncInterval != 60*time.Second {
		t.Errorf("alerting.syncInterval default = %v, want 60s", a.SyncInterval)
	}
	if a.BundleName != "kconmon-ng-console-rules" {
		t.Errorf("alerting.bundleName default = %q, want kconmon-ng-console-rules", a.BundleName)
	}
}

// TestLoadAlertingFromYAML proves the block parses under KnownFields(true) --
// every key spelled exactly as the chart will spell it.
func TestLoadAlertingFromYAML(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.yaml")
	const y = "alerting:\n" +
		"  enabled: true\n" +
		"  namespace: kconmon-ng\n" +
		"  syncInterval: 30s\n" +
		"  bundleName: my-console-rules\n"
	if err := os.WriteFile(p, []byte(y), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	a := cfg.Alerting
	if !a.Enabled {
		t.Error("alerting.enabled = false, want true")
	}
	if a.Namespace != "kconmon-ng" {
		t.Errorf("alerting.namespace = %q, want kconmon-ng", a.Namespace)
	}
	if a.SyncInterval != 30*time.Second {
		t.Errorf("alerting.syncInterval = %v, want 30s", a.SyncInterval)
	}
	if a.BundleName != "my-console-rules" {
		t.Errorf("alerting.bundleName = %q, want my-console-rules", a.BundleName)
	}
}

// TestValidateAlerting is the fail-closed table: everything is checked only
// when the gate is on, and every name that would be rejected by the apiserver
// is rejected at boot instead.
func TestValidateAlerting(t *testing.T) {
	for _, tc := range []struct {
		name    string
		yaml    string
		wantErr bool
	}{
		{"disabled ignores everything", "alerting:\n  enabled: false\n  syncInterval: 0s\n  bundleName: \"\"\n", false},
		{"enabled with the defaults", "alerting:\n  enabled: true\n", false},
		{"zero interval rejected", "alerting:\n  enabled: true\n  syncInterval: 0s\n", true},
		{"negative interval rejected", "alerting:\n  enabled: true\n  syncInterval: -1m\n", true},
		{"empty bundleName rejected", "alerting:\n  enabled: true\n  bundleName: \"\"\n", true},
		{"uppercase bundleName rejected", "alerting:\n  enabled: true\n  bundleName: Rules\n", true},
		{"underscore in bundleName rejected", "alerting:\n  enabled: true\n  bundleName: my_rules\n", true},
		{"leading dash in bundleName rejected", "alerting:\n  enabled: true\n  bundleName: -rules\n", true},
		{"trailing dot in bundleName rejected", "alerting:\n  enabled: true\n  bundleName: rules.\n", true},
		{"dots and dashes accepted", "alerting:\n  enabled: true\n  bundleName: a.b-c9\n", false},
		{"empty namespace is legal when enabled", "alerting:\n  enabled: true\n  namespace: \"\"\n", false},
		{"uppercase namespace rejected", "alerting:\n  enabled: true\n  namespace: Kconmon\n", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(p, []byte(tc.yaml), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := Load(p)
			if tc.wantErr && err == nil {
				t.Fatal("expected a validation error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("expected the config to validate, got: %v", err)
			}
		})
	}
}

// TestValidateAlertingNamesTheKnob: every rejection an operator reads must say
// WHICH key to fix.
func TestValidateAlertingNamesTheKnob(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*Config)
		want   string
	}{
		{"interval", func(c *Config) { c.Alerting.SyncInterval = 0 }, "alerting.syncInterval"},
		{"bundle name", func(c *Config) { c.Alerting.BundleName = "" }, "alerting.bundleName"},
		{"bad bundle name", func(c *Config) { c.Alerting.BundleName = "NOPE" }, "alerting.bundleName"},
		{"bad namespace", func(c *Config) { c.Alerting.Namespace = "NOPE" }, "alerting.namespace"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := defaults()
			c.Alerting.Enabled = true
			tc.mutate(c)
			err := c.Validate()
			if err == nil {
				t.Fatal("expected a validation error, got nil")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error should name %s, got: %v", tc.want, err)
			}
		})
	}
}

// TestAlertingResolveNamespacePrecedence pins the same three-step fallback
// kubernetesContext carries, because the two MUST agree: the console's bundle
// belongs in the namespace the console runs in.
func TestAlertingResolveNamespacePrecedence(t *testing.T) {
	for _, tc := range []struct {
		name  string
		field string
		env   string
		want  string
	}{
		{"explicit namespace wins", "kconmon-ng", "other-ns", "kconmon-ng"},
		{"env is the fallback", "", "kconmon-ng", "kconmon-ng"},
		{"env is trimmed", "", "  kconmon-ng \n", "kconmon-ng"},
		{"blank env falls through to default", "", "   ", "default"},
		{"nothing set falls back to default", "", "", "default"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(podNamespaceEnv, tc.env)
			a := AlertingConfig{Namespace: tc.field}
			if got := a.ResolveNamespace(); got != tc.want {
				t.Errorf("ResolveNamespace() = %q, want %q", got, tc.want)
			}
			// The two blocks resolve identically, by construction.
			k := KubernetesContextConfig{Namespace: tc.field}
			if got := k.ResolveNamespace(); got != tc.want {
				t.Errorf("KubernetesContextConfig.ResolveNamespace() = %q, want %q", got, tc.want)
			}
		})
	}
}

// validWebhookKey is 32 bytes, base64. Written out rather than computed so the
// test states the shape an operator has to produce.
const validWebhookKey = "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8="

// The whole block is OPTIONAL: a console that never declares a webhook must
// not fail to start over a cipher it will never use (M6 Decision 4).
func TestLoadWebhookDefaults(t *testing.T) {
	cfg, err := Load("/nonexistent/config.yaml")
	if err != nil {
		t.Fatalf("Load defaults: %v", err)
	}
	if cfg.Webhooks.EncryptionKey != "" || cfg.Webhooks.EncryptionKeyFile != "" {
		t.Errorf("webhooks key must default empty, got %q %q",
			cfg.Webhooks.EncryptionKey, cfg.Webhooks.EncryptionKeyFile)
	}
	key, err := cfg.Webhooks.ResolveEncryptionKey()
	if err != nil {
		t.Fatalf("ResolveEncryptionKey with nothing set = %v, want no error", err)
	}
	if key != nil {
		t.Errorf("ResolveEncryptionKey with nothing set = %v, want nil (the keyless state)", key)
	}
}

// alertPollInterval lives under webhooks, not under alerting, because it is not a property of
// alerting at all.
func TestLoadWebhookAlertPollIntervalDefault(t *testing.T) {
	cfg, err := Load("/nonexistent/config.yaml")
	if err != nil {
		t.Fatalf("Load defaults: %v", err)
	}
	if cfg.Webhooks.AlertPollInterval != DefaultWebhookAlertPollInterval {
		t.Errorf("webhooks.alertPollInterval = %v, want the default %v",
			cfg.Webhooks.AlertPollInterval, DefaultWebhookAlertPollInterval)
	}
	if DefaultWebhookAlertPollInterval != 30*time.Second {
		t.Errorf("the default poll interval is %v, want 30s -- it is also the granularity of "+
			"every alert.resolved timestamp, so changing it is a contract change",
			DefaultWebhookAlertPollInterval)
	}
}

func TestLoadWebhookAlertPollIntervalFromYAML(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte("webhooks:\n  alertPollInterval: 90s\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Webhooks.AlertPollInterval != 90*time.Second {
		t.Errorf("webhooks.alertPollInterval = %v, want 90s", cfg.Webhooks.AlertPollInterval)
	}
}

// The interval is only load-bearing when BOTH gates are on.
func TestWebhookAlertPollIntervalIsValidatedOnlyWhenBothGatesAreOn(t *testing.T) {
	for _, tc := range []struct {
		name            string
		key             string
		alertingEnabled bool
		interval        time.Duration
		wantErr         bool
		wantErrMentions string
	}{
		{name: "both gates on, positive", key: validWebhookKey, alertingEnabled: true, interval: time.Minute},
		{
			name: "both gates on, zero", key: validWebhookKey, alertingEnabled: true, interval: 0,
			wantErr: true, wantErrMentions: "webhooks.alertPollInterval",
		},
		{
			name: "both gates on, negative", key: validWebhookKey, alertingEnabled: true, interval: -time.Second,
			wantErr: true, wantErrMentions: "webhooks.alertPollInterval",
		},
		{name: "no key, alerting on, zero", alertingEnabled: true, interval: 0},
		{name: "key, alerting off, zero", key: validWebhookKey, interval: 0},
		{name: "neither gate, zero", interval: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := WebhooksConfig{EncryptionKey: tc.key, AlertPollInterval: tc.interval}
			err := w.validate(tc.alertingEnabled)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("validate(%v) = nil, want an error", tc.alertingEnabled)
				}
				if !strings.Contains(err.Error(), tc.wantErrMentions) {
					t.Errorf("error must name the knob %q, got: %v", tc.wantErrMentions, err)
				}
				return
			}
			if err != nil {
				t.Errorf("validate(%v) = %v, want nil", tc.alertingEnabled, err)
			}
		})
	}
}

// The cross-block rule has to be reachable through the top-level Validate, not
// only through the block's own method -- that is the entry point cmd/console
// actually calls.
func TestConfigValidateCatchesANonPositiveAlertPollInterval(t *testing.T) {
	cfg := defaults()
	cfg.Webhooks.EncryptionKey = validWebhookKey
	cfg.Alerting.Enabled = true
	cfg.Webhooks.AlertPollInterval = 0
	err := cfg.Validate()
	if err == nil {
		t.Fatal("Validate accepted a zero alertPollInterval with both gates on")
	}
	if !strings.Contains(err.Error(), "webhooks.alertPollInterval") {
		t.Errorf("error must name the knob, got: %v", err)
	}
}

func TestLoadWebhooksFromYAML(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte("webhooks:\n  encryptionKey: \""+validWebhookKey+"\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	key, err := cfg.Webhooks.ResolveEncryptionKey()
	if err != nil {
		t.Fatalf("ResolveEncryptionKey: %v", err)
	}
	if len(key) != webhookKeyLen {
		t.Errorf("resolved key is %d bytes, want %d", len(key), webhookKeyLen)
	}
}

// A key that is CONFIGURED but malformed is the opposite of the keyless state:
// a wrong Secret, caught at boot rather than at the first webhook an operator
// tries to create.
func TestValidateRejectsAMalformedWebhookKey(t *testing.T) {
	for _, tc := range []struct {
		name, key string
	}{
		{"not base64", "not-base64-!!!"},
		{"16 bytes", "AAECAwQFBgcICQoLDA0ODw=="},
		{"24 bytes", "AAECAwQFBgcICQoLDA0ODxAREhMUFRYX"},
		{"33 bytes", "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8g"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := defaults()
			c.Webhooks.EncryptionKey = tc.key
			err := c.Validate()
			if err == nil {
				t.Fatalf("Validate with key %q = nil, want an error", tc.key)
			}
			if !strings.Contains(err.Error(), "webhooks.encryptionKey") {
				t.Errorf("error should name webhooks.encryptionKey, got: %v", err)
			}
			// The error explains a SECRET's shape, so it must not repeat the
			// value -- that would put a nearly-correct key in a log line.
			if strings.Contains(err.Error(), tc.key) {
				t.Errorf("error echoes the configured key value: %v", err)
			}
		})
	}
}

func TestValidateRejectsBothWebhookKeyAndKeyFile(t *testing.T) {
	c := defaults()
	c.Webhooks.EncryptionKey = validWebhookKey
	c.Webhooks.EncryptionKeyFile = "/etc/kconmon/webhook.key"
	err := c.Validate()
	if err == nil {
		t.Fatal("expected an error when both webhooks.encryptionKey and encryptionKeyFile are set")
	}
	if !strings.Contains(err.Error(), "not both") {
		t.Errorf("error should say the two are mutually exclusive, got: %v", err)
	}
}

// The dsnFile pattern: the mounted file WINS, its contents are trimmed, and a
// missing or malformed file is an error rather than a silent fall-through to
// the keyless state.
func TestResolveEncryptionKeyFilePrecedenceAndErrors(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "webhook.key")
	if err := os.WriteFile(good, []byte("  "+validWebhookKey+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	short := filepath.Join(dir, "short.key")
	if err := os.WriteFile(short, []byte("AAECAwQFBgcICQoLDA0ODw=="), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Run("file wins over the inline value", func(t *testing.T) {
		// Both set is refused by Validate, but ResolveEncryptionKey is also
		// reachable from a hand-built struct, and there it must not be a coin
		// flip which one applies.
		w := WebhooksConfig{EncryptionKey: "not-base64-!!!", EncryptionKeyFile: good}
		key, err := w.ResolveEncryptionKey()
		if err != nil {
			t.Fatalf("ResolveEncryptionKey: %v", err)
		}
		if len(key) != webhookKeyLen {
			t.Errorf("resolved key is %d bytes, want %d from the FILE", len(key), webhookKeyLen)
		}
	})

	t.Run("file contents are trimmed", func(t *testing.T) {
		w := WebhooksConfig{EncryptionKeyFile: good}
		if _, err := w.ResolveEncryptionKey(); err != nil {
			t.Errorf("a key file with surrounding whitespace failed: %v", err)
		}
	})

	t.Run("unreadable file is an error", func(t *testing.T) {
		w := WebhooksConfig{EncryptionKeyFile: filepath.Join(dir, "nope.key")}
		if _, e := w.ResolveEncryptionKey(); e == nil {
			t.Error("a missing key file resolved to no error -- it must not fall through to keyless")
		}
	})

	t.Run("wrong length in the file is an error naming the file knob", func(t *testing.T) {
		w := WebhooksConfig{EncryptionKeyFile: short}
		_, e := w.ResolveEncryptionKey()
		if e == nil {
			t.Fatal("a 16-byte key file resolved without error")
		}
		if !strings.Contains(e.Error(), "webhooks.encryptionKeyFile") {
			t.Errorf("error should name webhooks.encryptionKeyFile, got: %v", e)
		}
	})

	t.Run("an empty file is the keyless state", func(t *testing.T) {
		empty := filepath.Join(dir, "empty.key")
		if err := os.WriteFile(empty, []byte("\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		w := WebhooksConfig{EncryptionKeyFile: empty}
		key, e := w.ResolveEncryptionKey()
		if e != nil || key != nil {
			t.Errorf("empty key file = (%v, %v), want (nil, nil)", key, e)
		}
	})
}

// --- valkey auth (2.0.0) ---------------------------------------------------
//
// console.valkey.mode=dependency/external point at a Valkey that almost always
// requires a password (bitnami's subchart enables requirepass by default), so
// the config has to be able to carry one -- from a FILE, exactly like the DSN.

func TestRedisResolveDSNReadsTheFile(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "redis-dsn")
	if err := os.WriteFile(f, []byte("redis://:s3cr3t@valkey:6379\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	v := RedisConfig{DSNFile: f}
	got, err := v.ResolveDSN()
	if err != nil {
		t.Fatalf("ResolveDSN: %v", err)
	}
	if got != "redis://:s3cr3t@valkey:6379" {
		t.Errorf("dsn = %q (trailing newline must be trimmed)", got)
	}
}

func TestRedisResolveDSNEmptyWithoutAFile(t *testing.T) {
	v := RedisConfig{}
	got, err := v.ResolveDSN()
	if err != nil || got != "" {
		t.Errorf("no dsn and no dsnFile must mean the in-process bus, got %q err %v", got, err)
	}
}

func TestRedisResolveDSNUnreadableFileIsAnError(t *testing.T) {
	v := RedisConfig{DSNFile: filepath.Join(t.TempDir(), "nope")}
	if _, err := v.ResolveDSN(); err == nil {
		t.Error("an unreadable dsnFile must be an error, not a silent fallback to the in-process bus")
	}
}

func TestRedisInlineDSNAndFileTogetherAreRejected(t *testing.T) {
	v := RedisConfig{DSN: "redis://v:6379", DSNFile: "/x"}
	if err := v.validate(); err == nil {
		t.Error("dsn and dsnFile together must be rejected, exactly like the database's pair")
	}
}

func TestRedisDSNFileParsesFromYAML(t *testing.T) {
	// The console rejects unknown fields, so the key has to exist in the struct.
	var c Config
	if err := yaml.Unmarshal([]byte("redis:\n  dsnFile: /etc/p\n"), &c); err != nil {
		t.Fatalf("redis.dsnFile must be a known field: %v", err)
	}
	if c.Redis.DSNFile != "/etc/p" {
		t.Errorf("dsnFile = %q", c.Redis.DSNFile)
	}
}

func TestSweeperConfigDefaultsAndValidation(t *testing.T) {
	// Defaults: off, with every budget pre-filled so enabling is a one-line change.
	cfg, err := Load(filepath.Join(t.TempDir(), "nope.yaml"))
	if err != nil {
		t.Fatalf("Load defaults: %v", err)
	}
	if cfg.Sweeper.Enabled {
		t.Error("sweeper.enabled default = true, want false")
	}
	if cfg.Sweeper.Interval != time.Minute || cfg.Sweeper.CheckType != "tcp" || cfg.Sweeper.Timeout != 5*time.Second {
		t.Errorf("sweeper defaults = %v/%q/%v, want 1m/tcp/5s", cfg.Sweeper.Interval, cfg.Sweeper.CheckType, cfg.Sweeper.Timeout)
	}

	// A disabled sweeper never validates its knobs — the scheduler's own rule.
	off := SweeperConfig{}
	if err := off.validate(); err != nil {
		t.Errorf("disabled sweeper rejected: %v", err)
	}

	for name, bad := range map[string]SweeperConfig{
		"sub-floor interval": {Enabled: true, Interval: time.Second, CheckType: "tcp", Timeout: time.Second},
		"mtr check type":     {Enabled: true, Interval: time.Minute, CheckType: "mtr", Timeout: time.Second},
		"zero timeout":       {Enabled: true, Interval: time.Minute, CheckType: "tcp"},
	} {
		if err := bad.validate(); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	good := SweeperConfig{Enabled: true, Interval: time.Minute, CheckType: "icmp", Timeout: 2 * time.Second}
	if err := good.validate(); err != nil {
		t.Errorf("valid sweeper rejected: %v", err)
	}
}
