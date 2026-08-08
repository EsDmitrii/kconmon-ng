package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	appconfig "github.com/EsDmitrii/kconmon-ng/internal/config"
	"github.com/EsDmitrii/kconmon-ng/internal/console/authn"
	"github.com/EsDmitrii/kconmon-ng/internal/console/authz"
	"github.com/EsDmitrii/kconmon-ng/internal/console/cache"
	"github.com/EsDmitrii/kconmon-ng/internal/console/checks"
	"github.com/EsDmitrii/kconmon-ng/internal/console/config"
	"github.com/EsDmitrii/kconmon-ng/internal/console/controllerclient"
	"github.com/EsDmitrii/kconmon-ng/internal/console/events"
	"github.com/EsDmitrii/kconmon-ng/internal/console/httpapi"
	"github.com/EsDmitrii/kconmon-ng/internal/console/metrics"
	"github.com/EsDmitrii/kconmon-ng/internal/console/promql"
	"github.com/EsDmitrii/kconmon-ng/internal/console/push"
	"github.com/EsDmitrii/kconmon-ng/internal/console/scheduler"
	"github.com/EsDmitrii/kconmon-ng/internal/console/store"
	"github.com/EsDmitrii/kconmon-ng/internal/console/ui"
	"github.com/EsDmitrii/kconmon-ng/internal/console/ws"
	"github.com/prometheus/client_golang/prometheus"
)

// rbacPolicyRefreshInterval is how often cmd/console re-reads the roles
// table and applies it to the live authz.Policy via Reload. Role BINDINGS
// need no such ticker -- httpapi's resolveRoles re-reads role_bindings on
// every request already, through roleResolver below. A custom role's
// PERMISSION SET is different: authz.Policy caches it, so without this
// ticker an admin editing a custom role's permissions through
// POST /api/v1/rbac/roles would need a console restart to take effect
// (task-18-brief.md: "pick the ticker, it is ten lines").
const rbacPolicyRefreshInterval = 60 * time.Second

// eventSink adapts a store.EventStore to events.EventSink. Neither package may
// import the other (store must not import events; events must not import
// store, per ADR-001's "store is the only pgx importer" plus the events
// package's own no-store-dependency rule), so the LiveEvent -> EventRecord
// translation lives here — the same import-cycle shape httpapi.RealtimeStatus
// already resolves for the realtime health bit. Zero new package edges.
type eventSink struct {
	store store.EventStore
}

// InsertEvent implements events.EventSink by projecting the live, browser-facing
// shape onto the durable one and delegating to the store.
func (s eventSink) InsertEvent(ctx context.Context, ev events.LiveEvent) (bool, error) { //nolint:gocritic // hugeParam: events.EventSink is the pinned interface signature, value semantics intentional
	return s.store.InsertEvent(ctx, store.EventRecord{
		EventSeq:  int64(ev.Seq), //nolint:gosec // Seq is a controller-assigned monotonic sequence, always >= 0
		EventTime: ev.Timestamp,
		Type:      ev.Type,
		Severity:  ev.Severity,
		Scope:     ev.Scope,
		Summary:   ev.Summary,
		Details:   ev.Details,
	})
}

// roleResolver adapts store.RoleStore's subject-scoped binding lookup
// (ListBindingsForSubject) to httpapi.RoleResolver's narrower RolesFor(ctx,
// authz.Subject) seam -- the same "small local adapter, not a store method
// httpapi depends on directly" shape eventSink above already uses for
// events.EventSink. Re-queried on EVERY request (resolveRoles,
// httpapi/middleware_auth.go), never cached: that is what lets a role
// BINDING created or removed through the RBAC API take effect on the very
// next request, with no restart and no ticker -- unlike a changed
// PERMISSION SET on a custom role, which authz.Policy does cache (see
// refreshCustomRoles below).
type roleResolver struct {
	db *store.DB
}

func (r roleResolver) RolesFor(ctx context.Context, s authz.Subject) ([]string, error) { //nolint:gocritic // hugeParam: authz.Subject is a value type by design, matching httpapi.RoleResolver's signature verbatim
	bindings, err := r.db.ListBindingsForSubject(ctx, s.ID, s.Groups)
	if err != nil {
		return nil, err
	}
	roles := make([]string, len(bindings))
	for i, b := range bindings {
		roles[i] = b.RoleName
	}
	return roles, nil
}

// pushInterval is how often each snapshot pusher recomputes and broadcasts when
// nothing nudges it. It matches the frontend's MATRIX_POLL_MS/TOPOLOGY_POLL_MS
// (both 15s), so a browser on the WebSocket path never sees staler data than one
// on the M1 polling path. Not operator-tunable in M2: the nudge relay already
// covers the latency case that matters (topology changes).
const pushInterval = 15 * time.Second

func main() {
	configPath := os.Getenv("KCONMON_NG_CONSOLE_CONFIG")
	if configPath == "" {
		configPath = "/etc/kconmon-ng-console/config.yaml"
	}

	cfg, err := config.Load(configPath)
	if err != nil {
		slog.Error("failed to load console config", "error", err)
		os.Exit(1)
	}

	appconfig.SetupLogger(cfg.LogLevel, cfg.LogFormat)

	slog.Info("kconmon-ng console starting", //nolint:gosec // G706: values come from the local config file, structured slog fields
		"version", appconfig.Version,
		"commit", appconfig.Commit,
		"buildDate", appconfig.BuildDate,
		"authMode", cfg.Auth.Mode,
	)
	if cfg.Auth.Mode == "anonymous" {
		slog.Warn("console is running in ANONYMOUS auth mode — do not use in production", //nolint:gosec // G706: role comes from the local config file, structured slog field
			"role", cfg.Auth.Anonymous.Role)
	}

	promReg := prometheus.NewRegistry()
	m := metrics.New(cfg.MetricsPrefix, promReg)

	uiHandler, err := ui.Handler()
	if err != nil {
		slog.Error("failed to initialize embedded UI", "error", err)
		os.Exit(1)
	}

	var ctrl *controllerclient.Client
	if cfg.Controller.URL != "" {
		ctrl = controllerclient.New(cfg.Controller.URL, cfg.Controller.Timeout)
	} else {
		slog.Warn("controller.url not set — topology API disabled (503)")
	}
	var prom *promql.Client
	if cfg.Prometheus.URL != "" {
		prom = promql.New(cfg.Prometheus.URL, promql.Guards{
			QueryTimeout:     cfg.Prometheus.QueryTimeout,
			MaxRange:         cfg.Prometheus.MaxRange,
			MaxResponseBytes: cfg.Prometheus.MaxResponseBytes,
		})
	} else {
		slog.Warn("prometheus.url not set — matrix and PromQL APIs disabled (503)")
	}

	// Two contexts on purpose. rootCtx is cancelled by SIGINT/SIGTERM and governs
	// the HTTP server only. bgCtx governs the realtime pipeline and is cancelled
	// by hand AFTER the HTTP server has finished shutting down — see the shutdown
	// block at the bottom of main.
	rootCtx, stopSignals := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	bgCtx, stopBackground := context.WithCancel(context.Background())

	bus, closeBus := newBus(bgCtx, cfg)

	// dsn resolution errors are fatal (unlike the Valkey fallback above): a
	// dsnFile that cannot be read means the Secret mount is wrong, not that the
	// operator meant to run without a database.
	dsn, err := cfg.Database.ResolveDSN()
	if err != nil {
		slog.Error("failed to resolve database DSN", "error", err)
		os.Exit(1)
	}

	// db stays nil for the entire process when database.dsn/dsnFile are unset:
	// that is the documented degraded path (M1/M2 surface unaffected), not an
	// error. When it IS configured, a failure to open it is fatal — Task 2's
	// decision, no silent fallback to "no database" the way Valkey has one.
	var db *store.DB
	if dsn == "" {
		slog.Info("database not configured — event history, run history and the audit log are disabled; the M1/M2 surface is unaffected")
	} else {
		db, err = store.Open(bgCtx, dsn, cfg.Database.MaxConns, cfg.Database.ConnectTimeout, cfg.Database.MigrateOnStart)
		if err != nil {
			slog.Error("failed to open database", "error", err)
			os.Exit(1)
		}
		// Here, while db is still owned by this goroutine and before anything
		// else can hold it — SetMetrics is an unsynchronized field write.
		db.SetMetrics(m)
	}

	// --- Auth (M3 Phase B/C) ------------------------------------------------
	//
	// cache.KV backs SessionStore and OIDCAuthenticator's login-flow state
	// (oidcstate:{state}). Built from the SAME bus newBus already dialled --
	// never a second Valkey connection -- so it inherits newBus's own
	// Valkey-unreachable fallback: cache.NewInProcessKV() whenever bus is not
	// a *cache.ValkeyBus. That fallback is single-replica-only for sessions,
	// same ADR-002 shape as realtime fan-out; the Helm session guard
	// (templates/console/configmap.yaml) refuses auth.mode=local|oidc with
	// console.valkey.mode=disabled and console.replicas>1 for exactly this
	// reason.
	var kv cache.KV
	if vb, ok := bus.(*cache.ValkeyBus); ok {
		kv = cache.NewValkeyKVFromBus(vb)
	} else {
		kv = cache.NewInProcessKV()
	}
	sessions := authn.NewSessionStore(kv, cfg.Auth.Session.TTL)

	// authenticator is built per cfg.Auth.Mode. The default case below is
	// unreachable through config.Load (Config.Validate already restricts
	// Mode to anonymous|local|header|oidc) but is required by the switch's
	// own well-formedness -- and it fails CLOSED (exit 1): reaching it means
	// validation was bypassed, and a composition root must not answer that
	// by serving unauthenticated reads.
	var authenticator authn.Authenticator
	var oidcDep httpapi.OIDCFlow
	switch cfg.Auth.Mode {
	case "anonymous":
		authenticator = authn.NewAnonymous(cfg.Auth.Anonymous.Role)
	case "local":
		// db is guaranteed non-nil here: config.Validate's validateAuth
		// requires a resolved database DSN for auth.mode=local, and an
		// unopenable database above is already fatal (os.Exit(1)).
		authenticator = authn.NewLocal(db, sessions, cfg.Auth.Session.CookieName)
	case "header":
		authenticator = authn.NewHeader(cfg.Auth.Header)
	case "oidc":
		clientSecret, err := readSecretFile(cfg.Auth.OIDC.ClientSecretFile)
		if err != nil {
			slog.Error("failed to read auth.oidc.clientSecretFile", "error", err)
			os.Exit(1)
		}
		oidcAuth, err := authn.NewOIDC(bgCtx, cfg.Auth.OIDC, clientSecret, sessions, kv, cfg.Auth.Session.CookieName)
		if err != nil {
			slog.Error("failed to initialize OIDC authenticator (provider discovery)", "error", err, "issuer", cfg.Auth.OIDC.Issuer) //nolint:gosec // G706: issuer is operator config, structured slog field
			os.Exit(1)
		}
		authenticator, oidcDep = oidcAuth, oidcAuth
	default:
		// Unreachable through config.Load (the validator rejects unknown
		// modes), so reaching it means validation was bypassed — and a
		// composition root must fail closed, not fall back to serving
		// unauthenticated reads (the same direction OIDC discovery failure
		// takes above).
		slog.Error("unknown auth.mode — config validation was bypassed, refusing to start", //nolint:gosec // G706: mode is operator config, structured slog field
			"mode", cfg.Auth.Mode)
		os.Exit(1)
	}
	if db != nil {
		// PATs work in every mode (SECURITY.md §10.1): wrapping is ONE
		// composition applied regardless of auth.mode, not a branch per
		// mode above. WithOwnerDisabledCheck(db) is what closes the
		// disabling-a-user-does-not-invalidate-their-PATs gap (store/auth.go's
		// TokenStore doc comment): it costs one extra GetUserByID lookup per
		// token-authenticated request, gated behind db != nil the same way
		// every other database-backed dependency here is, since there is no
		// UserStore to check against when database.mode=disabled.
		authenticator = authn.NewTokenFallback(db, authenticator, authn.WithOwnerDisabledCheck(db))
	}

	// Deps that only make sense with a database: role bindings, local users,
	// the audit log, and the RBAC/token admin APIs all live in PostgreSQL.
	// *store.DB satisfies httpapi.Auditor, httpapi.RoleAdmin, httpapi.TokenAdmin
	// and authn.UserStore structurally (no adapter needed for those four --
	// only RolesFor, above, needs one); assigning it only inside this
	// db != nil branch is what avoids hanging a nil *store.DB off a
	// non-nil interface value (the same typed-nil-interface trap eventsDep
	// avoids further down).
	var rolesDep httpapi.RoleResolver
	var usersDep authn.UserStore
	var auditDep httpapi.Auditor
	var rbacDep httpapi.RoleAdmin
	var tokensDep httpapi.TokenAdmin
	// targetsDep has NO non-database fallback, unlike runnerDep below:
	// targets are persisted configuration (Decision 13), so with
	// database.mode=disabled all five /api/v1/targets routes answer 503
	// rather than accepting writes that would vanish on the next restart.
	var targetsDep httpapi.TargetService
	// definitionsDep/schedulesDep share targetsDep's exact posture: persisted
	// configuration, no in-memory fallback (Decision 13) -- nil means every
	// /api/v1/checks and /api/v1/schedules route answers 503. Caught missing
	// by the M4 final-gate browser smoke: the handlers, tests and OpenAPI
	// entries all existed while this wiring did not, so a real console
	// answered 503 with the database fully configured.
	var definitionsDep httpapi.DefinitionService
	var schedulesDep httpapi.ScheduleService
	policy := authz.NewPolicy(nil)
	if db != nil {
		rolesDep = roleResolver{db: db}
		usersDep = db
		auditDep = db
		rbacDep = db
		tokensDep = db
		targetsDep = db
		definitionsDep = db
		schedulesDep = db

		if cfg.Auth.Mode == "local" {
			bootstrapLocalAdmin(bgCtx, db, cfg.Auth.Local)
		}

		// Custom roles are loaded once here, at boot, then kept fresh by the
		// rbac-policy-refresh ticker spawned below (task-18-brief.md item 4).
		// A read failure here is NOT fatal: built-in roles are compiled in
		// (Decision 7) and the refresh ticker retries within 60s — the same
		// warn-and-keep policy the ticker itself uses. Exiting would take an
		// anonymous-mode console down over a table it barely uses.
		custom, err := loadCustomRoles(bgCtx, db)
		if err != nil {
			slog.Warn("failed to load custom RBAC roles at boot — starting with built-in roles only, the refresh ticker will retry", "error", err)
			custom = nil
		}
		policy = authz.NewPolicy(custom)
	}

	// The Hub exists unconditionally: snapshot topics (topology, matrix:*) are
	// pushed locally and do not depend on the event pipeline, so GET /ws is
	// useful even when realtime event ingestion is off.
	hub := ws.NewHub(bus, m)

	grpcAddr := cfg.Controller.GRPCAddr
	switch {
	case grpcAddr == "":
		slog.Warn("controller.grpcAddr not set — realtime event ingestion disabled, /ws serves snapshots only")
	case ctrl == nil:
		slog.Warn("controller.grpcAddr is set but controller.url is empty — realtime event ingestion disabled " +
			"(the capability precheck needs the controller HTTP API)")
		grpcAddr = ""
	}
	// eventStore is nil unless db != nil. It is reused for both the ingester's
	// persistence sink and httpapi's read-only EventLister dependency, so a
	// live event is written through the exact same store a browser reads back.
	var eventStore store.EventStore
	var ingesterOpts []events.Option
	if db != nil {
		eventStore = store.NewEventStore(db, m)
		// WithEventSink(nil) must never be passed: a non-nil events.EventSink
		// interface holding a nil eventSink would panic on first InsertEvent
		// call. Building opts conditionally, rather than always passing
		// WithEventSink(eventSink{...}), is what keeps that impossible.
		ingesterOpts = append(ingesterOpts, events.WithEventSink(eventSink{store: eventStore}))
	}
	ingester := events.NewIngester(ctrl, grpcAddr, bus, m, ingesterOpts...)

	var nudgers []push.Nudger
	var matrixPusher *push.MatrixPusher
	if prom != nil {
		matrixPusher = push.NewMatrixPusher(prom, hub, cfg.MetricsPrefix, pushInterval, m)
		nudgers = append(nudgers, matrixPusher)
	}
	var topologyPusher *push.TopologyPusher
	if ctrl != nil {
		topologyPusher = push.NewTopologyPusher(ctrl, hub, pushInterval, m)
		nudgers = append(nudgers, topologyPusher)
	}

	// eventsDep starts as Deps{}'s ordinary zero value — a genuine nil
	// EventLister interface — and is assigned only inside the db != nil
	// branch. Assigning a nil *store.DB (or any nil concrete pointer) directly
	// to an interface field instead would produce a typed-nil interface that
	// compares != nil, so httpapi's "s.events == nil" 503 gate would then call
	// ListEvents on a nil receiver instead of answering 503.
	var eventsDep httpapi.EventLister
	if db != nil {
		eventsDep = eventStore
	}

	// runner executes on-demand diagnostics runs (Task 22/23): constructed
	// only when ctrl != nil, since there is no meaningful run path without a
	// controller to dispatch each pair's check to (httpapi answers 503 on
	// all three /api/v1/runs* routes when runnerDep stays the nil interface
	// value below — same convention as eventsDep just above). Its Store is
	// *store.DB when a database is configured, or checks.NewMemoryStore()
	// otherwise (Decision 15: runs still work with the database disabled,
	// just not durably — only the most recent 50 stay retrievable — and that
	// is already honestly labelled via GET /api/v1/config's existing
	// "database":{"configured"} signal, the same one handleEvents/handleAudit
	// gate on, with no runs-specific field needed).
	var runner *checks.Runner
	if ctrl != nil {
		var runStore checks.Store
		if db != nil {
			runStore = db
		} else {
			runStore = checks.NewMemoryStore()
		}
		runner = checks.NewRunner(ctrl, hub, bus, runStore, m)
	}
	var runnerDep httpapi.RunService
	if runner != nil {
		runnerDep = runner
	}

	srv := httpapi.NewServer(httpapi.Deps{
		Config: cfg, Metrics: m, PromRegistry: promReg, UI: uiHandler,
		Controller: ctrl, Prometheus: prom, Hub: hub, Realtime: ingester,
		Events:        eventsDep,
		Authenticator: authenticator, Policy: policy, Roles: rolesDep, Sessions: sessions,
		Users: usersDep, OIDC: oidcDep,
		Audit: auditDep, RBAC: rbacDep, Tokens: tokensDep,
		Runner: runnerDep, Targets: targetsDep,
		Definitions: definitionsDep, Schedules: schedulesDep,
		// The SAME kv SessionStore and the OIDC state stash ride on, so the
		// rate limiter is cluster-wide exactly when Valkey is (and
		// per-replica, weaker but never stronger, when it is not).
		KV: kv,
	})

	var wg sync.WaitGroup
	spawn := func(component string, run func(context.Context)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			run(bgCtx)
			slog.Debug("realtime component stopped", "component", component)
		}()
	}

	spawn("ws-hub", hub.Run)
	spawn("event-ingester", ingester.Run)
	if matrixPusher != nil {
		spawn("matrix-pusher", matrixPusher.Run)
	}
	if topologyPusher != nil {
		spawn("topology-pusher", topologyPusher.Run)
	}
	if len(nudgers) > 0 {
		spawn("nudge-relay", func(ctx context.Context) { push.RunNudgeRelay(ctx, bus, nudgers...) })
	}
	if db != nil {
		retention := time.Duration(cfg.Database.RetentionDays) * 24 * time.Hour
		spawn("retention-pruner", store.NewPruner(db, retention, m).Run)
		// Unlike the pruner, this one runs whatever retention is set to: the
		// pool gauge describes the pool, not the retention policy.
		spawn("store-pool-stats", store.NewPoolStatsPoller(db, m).Run)
		spawn("rbac-policy-refresh", func(ctx context.Context) { refreshCustomRoles(ctx, db, policy) })
	}

	// The schedule loop is opt-in (console.scheduler.enabled, default false)
	// and needs BOTH a database -- check_schedules and the cross-replica
	// advisory lock live there -- and a runner, since a fired schedule becomes
	// an ordinary diagnostics run. A misconfiguration here warns and leaves
	// the loop off rather than refusing to boot: everything else the console
	// serves still works, and taking the whole deployment down over a feature
	// flag would be the worse failure. It is spawned through the same `spawn`
	// helper as the pruner, so bgCtx cancellation stops it and wg.Wait covers
	// its exit in the shutdown sequence below -- and because the advisory lock
	// is taken and released PER TICK, a replica stopped mid-tick costs the
	// fleet one tick, never a wedged lock.
	switch {
	case !cfg.Scheduler.Enabled:
	case db == nil:
		slog.Warn("scheduler.enabled is set but no database is configured — the schedule loop is off " +
			"(check schedules and its cross-replica advisory lock both live in PostgreSQL)")
	case runner == nil:
		slog.Warn("scheduler.enabled is set but controller.url is empty — the schedule loop is off " +
			"(a fired schedule is an ordinary diagnostics run, which needs a controller to dispatch to)")
	default:
		spawn("check-scheduler", scheduler.New(scheduler.Deps{
			Lock: db, Store: db, Runner: runner, Topology: ctrl,
			Metrics: m, Interval: cfg.Scheduler.TickInterval,
		}).Run)
		// The continuous external-check reconciler rides the SAME gate, the
		// same cadence and the same advisory lock key as the schedule loop,
		// and that is deliberate on all three counts.
		//
		// Same gate: it consumes kind='continuous' check_schedules rows, the
		// half of that table the scheduler deliberately skips (see
		// scheduler.fireOne), so "schedules are being acted on" is one switch,
		// not two an operator can set inconsistently. Same requirements too --
		// no database means no continuous definitions to read and no
		// cross-replica lock to take, so there is nothing for it to do, which
		// is why it is wired inside this arm rather than beside it.
		//
		// Same cadence: both are seconds-scale polls whose whole design
		// assumes a short tick (the lock is taken and released per tick), and
		// a second interval knob would be one more number to keep consistent
		// with the first for no behavioural gain.
		//
		// Same key: scheduler.LockKey is passed in rather than read from this
		// package, because internal/console/scheduler imports
		// internal/console/checks and the reconciler lives in the latter -- see
		// checks.ReconcilerDeps.LockKey. Sharing it is what keeps N replicas
		// from issuing N identical PUTs per interval; the cost is that the two
		// ticks serialize against each other, which checks.Reconciler's own doc
		// comment explains is acceptable at this cadence.
		spawn("external-check-reconciler", checks.NewReconciler(checks.ReconcilerDeps{
			Lock: db, Store: db, Topology: ctrl, Controller: ctrl,
			Metrics: m, Interval: cfg.Scheduler.TickInterval, LockKey: scheduler.LockKey,
		}).Run)
		slog.Info("schedule loop enabled", "tickInterval", cfg.Scheduler.TickInterval)
	}

	srv.SetReady(true)

	runErr := srv.Run(rootCtx)

	// Shutdown, in this order and no other:
	//  1. srv.Run has already returned, so the listener is closed and
	//     http.Server.Shutdown has drained the in-flight plain HTTP requests. No
	//     new WebSocket upgrade can arrive from here on.
	//  2. stopBackground stops the ingester, the pushers, the nudge relay and the
	//     Hub. Hijacked WebSocket connections are not tracked by
	//     http.Server.Shutdown, so this — not step 1 — is what releases them.
	//  3. wg.Wait guarantees nobody is inside Bus.Publish any more. It covers
	//     ONLY the components spawned above (hub.Run, ingester, pushers, relay)
	//     — the per-client read/write pumps (ws/conn.go) are tracked by nothing
	//     and are abandoned at process exit, so the ~2×writeWait (≈20s) linger a
	//     writePump may need against a wedged peer is best-effort (peer may see
	//     an RST instead of a 1001 close frame), not a budget honored here.
	//     wg.Wait itself returns promptly: every waited component exits on ctx
	//     cancellation.
	//  4. closeBus releases the Valkey client last. Closing it earlier would make
	//     in-flight publishes fail against a closed client and log errors during
	//     an otherwise clean shutdown.
	//  5. db.Close, when a database is configured, comes AFTER closeBus for the
	//     same reason closeBus comes after wg.Wait: both the retention pruner
	//     (spawned above, waited on by wg like every other background
	//     component) and the ingester's event sink run queries against the
	//     pool, so closing it any earlier would fail in-flight inserts/deletes
	//     during an otherwise clean shutdown.
	//
	// runner.Wait sits between stopBackground and wg.Wait, on its own budget:
	// a run's execution context is deliberately NOT derived from bgCtx
	// (checks.Runner.Start's doc comment), so stopBackground does not, by
	// itself, wait for or cancel any run in flight — this is what gives one
	// a bounded (10s) chance to finish, and its terminal store write and WS
	// frames to land, before the process exits, instead of a rolling update
	// silently truncating a diagnostic without a trace. It is independent of
	// wg (which tracks the hub/ingester/pushers/pruner, none of which a run
	// depends on), so its position relative to wg.Wait does not matter
	// causally — placed first since it is what stopBackground's own log line
	// below is announcing.
	slog.Info("http server stopped, draining realtime pipeline")
	stopBackground()
	if runner != nil {
		waitCtx, waitCancel := context.WithTimeout(context.Background(), 10*time.Second)
		runner.Wait(waitCtx)
		waitCancel()
	}
	wg.Wait()
	closeBus()
	// The in-process KV runs its own TTL-sweep goroutine; the Valkey-backed
	// KV shares the bus client closeBus just closed and has nothing of its
	// own to stop.
	if ipkv, ok := kv.(*cache.InProcessKV); ok {
		ipkv.Close()
	}
	if db != nil {
		db.Close()
	}
	stopSignals()
	slog.Info("console shutdown complete")

	if runErr != nil {
		slog.Error("console exited with error", "error", runErr)
		os.Exit(1)
	}
}

// readSecretFile reads a file-mounted secret (an OIDC client secret, a
// bootstrap admin password) and trims surrounding whitespace, mirroring
// DatabaseConfig.ResolveDSN's own "file-mounted, trimmed" convention for
// database.dsnFile. Every auth secret is a file path, never an inline
// config value: KCONMON_NG_CONSOLE_CONFIG stays the only env var, and no
// chart value ever carries secret material.
func readSecretFile(path string) (string, error) {
	data, err := os.ReadFile(path) //nolint:gosec // path comes from operator config (a mounted Secret), not user input -- same trust boundary as config.Load's own os.ReadFile
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

// bootstrapLocalAdminRole is the built-in role granted to the operator-named
// bootstrapAdmin user. Without a binding, a freshly created user has zero
// roles and falls back to auth.defaultRole (empty by default = 403 on
// everything, middleware_auth.go's resolveRoles) -- bootstrapping a user
// nobody can use would defeat the entire point of "so the console isn't
// locked out on first boot".
const bootstrapLocalAdminRole = "admin"

// bootstrapLocalAdmin implements task-18-brief.md's local-mode bootstrap:
// when auth.local.bootstrapAdmin is set AND the users table is empty, it
// creates that user (from bootstrapAdminPasswordFile) and binds it to the
// built-in admin role, then logs a WARN telling the operator to change the
// password. If the table is already non-empty, it does nothing and logs at
// DEBUG -- re-running bootstrap on every restart would let a stale
// bootstrapAdminPasswordFile Secret silently reset a password the operator
// (or the bootstrap admin) has since rotated.
//
// Callers must only invoke this when db != nil (checked by the caller, not
// here, for the same "conditional deps only inside if db != nil" discipline
// every other optional dependency in main follows).
func bootstrapLocalAdmin(ctx context.Context, db *store.DB, cfg config.LocalConfig) {
	if cfg.BootstrapAdmin == "" {
		return
	}

	count, err := db.CountUsers(ctx)
	if err != nil {
		slog.Error("failed to count users for local-mode admin bootstrap", "error", err)
		os.Exit(1)
	}
	if count > 0 {
		// The table is populated, so no user is created — but reconcile the
		// bootstrap admin's role binding if the user exists without one: a
		// crash between CreateUser and CreateBinding on a previous boot
		// would otherwise strand a permission-less admin FOREVER (this
		// count>0 path would skip, and fixing it needs the very RBAC API
		// the stranded admin cannot reach).
		reconcileBootstrapAdminBinding(ctx, db, cfg.BootstrapAdmin)
		slog.Debug("users table already populated — skipping local-mode admin bootstrap", //nolint:gosec // G706: bootstrapAdmin is an operator-configured username, structured slog field
			"bootstrapAdmin", cfg.BootstrapAdmin)
		return
	}

	password, err := readSecretFile(cfg.BootstrapAdminPasswordFile)
	if err != nil {
		slog.Error("failed to read auth.local.bootstrapAdminPasswordFile", "error", err)
		os.Exit(1)
	}
	hash, err := authn.HashPassword(password)
	if err != nil {
		slog.Error("failed to hash bootstrap admin password", "error", err)
		os.Exit(1)
	}

	user, err := db.CreateUser(ctx, cfg.BootstrapAdmin, hash, cfg.BootstrapAdmin)
	if err != nil {
		if errors.Is(err, store.ErrAlreadyExists) {
			// Another replica's boot won the CountUsers==0 race against the
			// same empty table; nothing left for this one to do.
			slog.Debug("bootstrap admin already created by another replica", //nolint:gosec // G706: see above
				"bootstrapAdmin", cfg.BootstrapAdmin)
			return
		}
		slog.Error("failed to create bootstrap admin user", "error", err)
		os.Exit(1)
	}
	if _, err := db.CreateBinding(ctx, bootstrapLocalAdminRole, "user", user.ID); err != nil {
		slog.Error("failed to bind bootstrap admin to the admin role", "error", err)
		os.Exit(1)
	}

	slog.Warn("bootstrap admin user created on first start — change its password immediately", //nolint:gosec // G706: bootstrapAdmin is an operator-configured username, structured slog field, never the password
		"username", cfg.BootstrapAdmin)
}

// reconcileBootstrapAdminBinding re-creates the bootstrap admin's admin-role
// binding when the user row exists but the binding does not (a crash between
// CreateUser and CreateBinding on a previous boot). Best-effort: every
// failure is logged and boot continues — this is a repair path, not a gate.
func reconcileBootstrapAdminBinding(ctx context.Context, db *store.DB, username string) {
	user, err := db.GetUserByUsername(ctx, username)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			slog.Warn("bootstrap admin binding reconcile: lookup failed", "error", err)
		}
		return
	}
	bindings, err := db.ListBindingsForSubject(ctx, user.ID, nil)
	if err != nil {
		slog.Warn("bootstrap admin binding reconcile: list bindings failed", "error", err)
		return
	}
	for i := range bindings {
		if bindings[i].RoleName == bootstrapLocalAdminRole {
			return // binding present — nothing to repair
		}
	}
	if _, err := db.CreateBinding(ctx, bootstrapLocalAdminRole, "user", user.ID); err != nil && !errors.Is(err, store.ErrAlreadyExists) {
		slog.Warn("bootstrap admin binding reconcile: create binding failed", "error", err)
		return
	}
	slog.Warn("bootstrap admin role binding was missing and has been re-created", //nolint:gosec // G706: operator-configured username, structured slog field
		"username", username)
}

// loadCustomRoles reads every custom (database-defined) role and reshapes it
// into the map[string][]authz.Permission authz.NewPolicy/(*authz.Policy).Reload
// take -- store.Role.Permissions is a plain []string (the roles table's own
// storage shape), so each entry is cast to authz.Permission individually,
// not validated against authz.AllPermissions: an unknown permission string
// simply never matches anything Policy.Can checks for, same as it always
// has for a role loaded once at boot.
func loadCustomRoles(ctx context.Context, db *store.DB) (map[string][]authz.Permission, error) {
	roles, err := db.ListRoles(ctx)
	if err != nil {
		return nil, err
	}
	custom := make(map[string][]authz.Permission, len(roles))
	for _, role := range roles {
		perms := make([]authz.Permission, len(role.Permissions))
		for i, p := range role.Permissions {
			perms[i] = authz.Permission(p)
		}
		custom[role.Name] = perms
	}
	return custom, nil
}

// refreshCustomRoles runs loadCustomRoles every rbacPolicyRefreshInterval and
// applies the result to policy via Reload, so an admin's change to a custom
// role's PERMISSION SET (through POST /api/v1/rbac/roles) takes effect for
// new requests within one interval instead of requiring a console restart --
// task-18-brief.md's "pick the ticker, it is ten lines" over only
// documenting the restart requirement. A role BINDING needs no such ticker
// (roleResolver re-reads role_bindings on every request already); this is
// only for the cached permission SET behind each role name.
//
// A read failure logs and leaves policy exactly as it was: a transient store
// hiccup must never blank out every custom role's permissions for the rest
// of the process.
func refreshCustomRoles(ctx context.Context, db *store.DB, policy *authz.Policy) {
	ticker := time.NewTicker(rbacPolicyRefreshInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			custom, err := loadCustomRoles(ctx, db)
			if err != nil {
				slog.Warn("failed to refresh custom RBAC roles — policy left unchanged", "error", err)
				continue
			}
			policy.Reload(custom)
		}
	}
}

// newBus selects the pub/sub backend the realtime pipeline runs on and returns a
// close func for it.
//
// A non-empty valkey.address means real cross-replica fan-out (cache.ValkeyBus):
// every replica's ingester publishes there, and every replica's Hub sees every
// event. An empty address — or a Valkey that cannot be dialled at startup —
// falls back to cache.NewInProcessBus(). That fallback is deliberate, not a
// papered-over error: a single-replica console on an in-process bus is a
// documented working state (ADR-002), so a Valkey outage must degrade realtime
// fan-out, not stop the console from booting and serving the M1 REST API.
//
// The fallback lasts until the process restarts: NewValkeyBus only fails on the
// initial dial. Disconnects after that are rueidis's problem — the client
// retries and resubscribes internally, with the bus's own receive loop as a
// backstop for the cases rueidis gives up on. The warning below is therefore
// the operator's signal to restart the console once Valkey is back.
//
// The returned closeBus must be called exactly once, after every publisher has
// stopped: cancelling ctx stops the ValkeyBus receive loop but does NOT release
// the rueidis client — only Close does.
func newBus(ctx context.Context, cfg *config.Config) (bus cache.Bus, closeBus func()) {
	if cfg.Valkey.Address == "" {
		slog.Info("valkey.address not set — using the in-process bus (realtime fan-out is single-replica only)")
		return cache.NewInProcessBus(), func() {}
	}

	vb, err := cache.NewValkeyBus(ctx, cfg.Valkey.Address, cfg.Valkey.DialTimeout)
	if err != nil {
		slog.Warn("valkey unreachable at startup — falling back to the in-process bus; "+ //nolint:gosec // G706: address comes from the local config file, structured slog field
			"realtime fan-out is single-replica only until the console is restarted",
			"address", cfg.Valkey.Address, "error", err)
		return cache.NewInProcessBus(), func() {}
	}

	slog.Info("valkey pub/sub enabled", "address", cfg.Valkey.Address) //nolint:gosec // G706: address comes from the local config file, structured slog field
	return vb, vb.Close
}
