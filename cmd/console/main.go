package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	appconfig "github.com/EsDmitrii/kconmon-ng/internal/config"
	"github.com/EsDmitrii/kconmon-ng/internal/console/alerting"
	"github.com/EsDmitrii/kconmon-ng/internal/console/authn"
	"github.com/EsDmitrii/kconmon-ng/internal/console/authz"
	"github.com/EsDmitrii/kconmon-ng/internal/console/cache"
	"github.com/EsDmitrii/kconmon-ng/internal/console/checks"
	"github.com/EsDmitrii/kconmon-ng/internal/console/config"
	"github.com/EsDmitrii/kconmon-ng/internal/console/controllerclient"
	"github.com/EsDmitrii/kconmon-ng/internal/console/enrich"
	"github.com/EsDmitrii/kconmon-ng/internal/console/events"
	"github.com/EsDmitrii/kconmon-ng/internal/console/httpapi"
	"github.com/EsDmitrii/kconmon-ng/internal/console/kubectx"
	"github.com/EsDmitrii/kconmon-ng/internal/console/metrics"
	"github.com/EsDmitrii/kconmon-ng/internal/console/promql"
	"github.com/EsDmitrii/kconmon-ng/internal/console/promrules"
	"github.com/EsDmitrii/kconmon-ng/internal/console/push"
	"github.com/EsDmitrii/kconmon-ng/internal/console/scheduler"
	"github.com/EsDmitrii/kconmon-ng/internal/console/store"
	"github.com/EsDmitrii/kconmon-ng/internal/console/ui"
	"github.com/EsDmitrii/kconmon-ng/internal/console/webhooks"
	"github.com/EsDmitrii/kconmon-ng/internal/console/ws"
	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// buildInClusterClientset builds a Kubernetes clientset from the in-cluster service account;
// returns an error when running outside a cluster (e.g. local development).
func buildInClusterClientset() (*kubernetes.Clientset, error) {
	restCfg, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("in-cluster config: %w", err)
	}
	clientset, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return nil, fmt.Errorf("kubernetes client: %w", err)
	}
	return clientset, nil
}

// buildInClusterDynamic builds an unstructured (dynamic) Kubernetes client from the same in-cluster
// service account; same posture as buildInClusterClientset: an error means "not running in a
// cluster".
func buildInClusterDynamic() (dynamic.Interface, error) {
	restCfg, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("in-cluster config: %w", err)
	}
	client, err := dynamic.NewForConfig(restCfg)
	if err != nil {
		return nil, fmt.Errorf("dynamic kubernetes client: %w", err)
	}
	return client, nil
}

// rbacPolicyRefreshInterval is how often cmd/console re-reads the roles table and applies it to the
// live authz.Policy via Reload; a custom role's PERMISSION SET is different: authz.Policy caches.
const rbacPolicyRefreshInterval = 60 * time.Second

// eventSink adapts a store.EventStore to events.EventSink; neither package may import.
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

// roleResolver adapts store.RoleStore's subject-scoped binding lookup (ListBindingsForSubject) to
// httpapi.RoleResolver's narrower RolesFor(ctx, authz.Subject) seam; re-queried on EVERY request
// (resolveRoles, httpapi/middleware_auth.go).
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

// pushInterval is how often each snapshot pusher recomputes and broadcasts when nothing nudges it;
// it matches the frontend's MATRIX_POLL_MS/TOPOLOGY_POLL_MS (both 15s).
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

	// Two contexts on purpose. rootCtx is cancelled by SIGINT/SIGTERM and governs the HTTP server
	// only.
	rootCtx, stopSignals := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	bgCtx, stopBackground := context.WithCancel(context.Background())

	// An unreadable valkey.passwordFile is fatal for the same reason dsnFile is: a broken Secret mount,
	// not an operator asking to run without a password.
	bus, closeBus, busErr := newBus(bgCtx, cfg)
	if busErr != nil {
		slog.Error("failed to resolve the Valkey password", "error", busErr)
		os.Exit(1)
	}

	// dsn resolution errors are fatal (unlike the Valkey fallback above): a
	// dsnFile that cannot be read means the Secret mount is wrong, not that the
	// operator meant to run without a database.
	dsn, err := cfg.Database.ResolveDSN()
	if err != nil {
		slog.Error("failed to resolve database DSN", "error", err)
		os.Exit(1)
	}

	// db stays nil for the entire process when database.dsn/dsnFile are unset: that is the documented
	// degraded path.
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

	// Built from the SAME bus newBus already dialled -- never a second Valkey connection.
	var kv cache.KV
	if vb, ok := bus.(*cache.ValkeyBus); ok {
		kv = cache.NewValkeyKVFromBus(vb)
	} else {
		kv = cache.NewInProcessKV()
	}
	sessions := authn.NewSessionStore(kv, cfg.Auth.Session.TTL)

	// authenticator is built per cfg.Auth.Mode; the default case below is unreachable through
	// config.Load (Config.Validate already restricts Mode to anonymous|local|header|oidc) but is
	// required by the switch's own well-formedness.
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
		// Unreachable through config.Load (the validator rejects unknown modes), so reaching it means
		// validation was bypassed.
		slog.Error("unknown auth.mode — config validation was bypassed, refusing to start", //nolint:gosec // G706: mode is operator config, structured slog field
			"mode", cfg.Auth.Mode)
		os.Exit(1)
	}
	if db != nil {
		// PATs work in every mode (SECURITY.md §10.1): wrapping is ONE composition applied regardless of
		// auth.mode.
		authenticator = authn.NewTokenFallback(db, authenticator, authn.WithOwnerDisabledCheck(db))
	}

	// Deps that only make sense with a database: role bindings.
	var rolesDep httpapi.RoleResolver
	var usersDep authn.UserStore
	var auditDep httpapi.Auditor
	var rbacDep httpapi.RoleAdmin
	var tokensDep httpapi.TokenAdmin
	// targetsDep has NO non-database fallback, unlike runnerDep below: targets are persisted
	// configuration.
	var targetsDep httpapi.TargetService
	// definitionsDep/schedulesDep share targetsDep's exact posture: persisted configuration, no
	// in-memory fallback.
	var definitionsDep httpapi.DefinitionService
	var schedulesDep httpapi.ScheduleService
	// mtrDep/annotationsDep share the exact same posture, and are wired in the SAME task as their
	// handlers precisely.
	var mtrDep httpapi.MTRService
	var annotationsDep httpapi.AnnotationService
	// incidentsDep/maintenanceDep/webhooksDep/k8sEventsDep share the identical posture.
	var incidentsDep httpapi.IncidentService
	var maintenanceDep httpapi.MaintenanceService
	var webhooksDep httpapi.WebhookService
	var k8sEventsDep httpapi.K8sEventService
	// alertRulesDep shares the identical posture: nil means /api/v1/export and /api/v1/import answer
	// 503.
	var alertRulesDep httpapi.AlertRuleService
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
		mtrDep = db
		annotationsDep = db
		incidentsDep = db
		maintenanceDep = db
		webhooksDep = db
		k8sEventsDep = db
		alertRulesDep = db

		if cfg.Auth.Mode == "local" {
			bootstrapLocalAdmin(bgCtx, db, cfg.Auth.Local)
		}

		// Custom roles are loaded once here, at boot, then kept fresh by the rbac-policy-refresh ticker
		// spawned below.
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
		// WithEventSink(nil) must never be passed: a non-nil events.EventSink interface holding a nil
		// eventSink would panic on first InsertEvent call.
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

	// eventsDep starts as Deps{}'s ordinary zero value — a genuine nil EventLister interface.
	var eventsDep httpapi.EventLister
	var topologyHistoryDep httpapi.TopologyHistory
	if db != nil {
		eventsDep = eventStore
		topologyHistoryDep = eventStore
	}

	// runner executes on-demand diagnostics runs: constructed only when ctrl != nil.
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

	// enricherDep swaps the cache-only hop enrichment read for a resolving one.
	var enricherDep httpapi.EnrichmentReader
	var enricher *enrich.Resolver
	switch {
	case !cfg.MTR.Enrichment.Enabled:
		// The default. Nothing to log: an off feature is not news.
	case db == nil:
		slog.Warn("mtr.enrichment.enabled is set but no database is configured — hop enrichment is off " +
			"(the TTL cache lives in PostgreSQL; set console.database.mode)")
	default:
		// enrichErr rather than err: assigning the function-scoped err this
		// far down would make every earlier `x, err :=` a govet shadow report.
		resolver, enrichErr := enrich.New(cfg.MTR.Enrichment, db, nil, m)
		if enrichErr != nil {
			slog.Error("failed to build the hop enrichment resolver", "error", enrichErr)
			os.Exit(1)
		}
		enricher, enricherDep = resolver, resolver
		slog.Info("hop enrichment enabled",
			"rdns", cfg.MTR.Enrichment.RDNS.Enabled,
			"asnPath", cfg.MTR.Enrichment.GeoIP.ASNPath, //nolint:gosec // G706: structured slog fields, operator config
			"cityPath", cfg.MTR.Enrichment.GeoIP.CityPath,
			"ttl", cfg.MTR.Enrichment.TTL)
	}

	// A key that is configured but unreadable or malformed is a wrong Secret mount, not a deliberate
	// omission.
	webhookKey, keyErr := cfg.Webhooks.ResolveEncryptionKey()
	if keyErr != nil {
		slog.Error("failed to resolve the webhook encryption key", "error", keyErr)
		os.Exit(1)
	}
	var dispatcher *webhooks.Dispatcher
	var webhookSealerDep httpapi.SecretSealer
	var webhookTesterDep httpapi.TestDispatcher
	var incidentNotifierDep httpapi.IncidentNotifier
	switch {
	case len(webhookKey) == 0:
	case db == nil:
		slog.Warn("webhooks.encryptionKey is set but no database is configured — webhook delivery is off " +
			"(endpoints, their encrypted secrets and their delivery outcomes all live in PostgreSQL; " +
			"set console.database.mode)")
	default:
		// dispatcherErr rather than err: assigning the function-scoped err
		// this far down would make every earlier `x, err :=` a govet shadow
		// report (the same reason enrichErr exists above).
		d, dispatcherErr := webhooks.New(webhookKey, db, m)
		if dispatcherErr != nil {
			slog.Error("failed to build the webhook dispatcher", "error", dispatcherErr)
			os.Exit(1)
		}
		dispatcher = d
		webhookSealerDep, webhookTesterDep, incidentNotifierDep = d, d, d
		slog.Info("webhook delivery enabled")
	}

	// The PrometheusRule reconciler is opt-in (console.alerting.enabled, default false) and needs a
	// database.
	var ruleReconciler *promrules.Reconciler
	switch {
	case !cfg.Alerting.Enabled:
	case db == nil:
		slog.Warn("alerting.enabled is set but no database is configured — prometheus rule sync is off " +
			"(alert rules live in PostgreSQL; set console.database.mode)")
	default:
		dyn, dynErr := buildInClusterDynamic()
		if dynErr != nil {
			slog.Warn("alerting.enabled is set but the in-cluster Kubernetes config is unavailable — "+
				"prometheus rule sync is off (the console is not running in a cluster, or its "+
				"ServiceAccount token is not mounted); the alert-rule builder and API are unaffected",
				"error", dynErr)
			break
		}
		namespace := cfg.Alerting.ResolveNamespace()
		ruleClient, clientErr := promrules.NewClient(dyn, namespace)
		if clientErr != nil {
			slog.Warn("prometheus rule sync is off", "error", clientErr)
			break
		}
		// The renderer carries cfg.MetricsPrefix, not the package default.
		reconciler, recErr := promrules.New(promrules.Deps{
			Client:     ruleClient,
			Store:      db,
			Renderer:   alerting.NewRenderer(cfg.MetricsPrefix),
			BundleName: cfg.Alerting.BundleName,
			Interval:   cfg.Alerting.SyncInterval,
		})
		if recErr != nil {
			slog.Warn("prometheus rule sync is off", "error", recErr)
			break
		}
		ruleReconciler = reconciler
		slog.Info("prometheus rule sync enabled", //nolint:gosec // G706: namespace/bundle come from operator config or the downward API, structured slog fields
			"namespace", namespace, "bundle", cfg.Alerting.BundleName,
			"syncInterval", cfg.Alerting.SyncInterval, "metricsPrefix", cfg.MetricsPrefix)
	}
	// It needs three things at once, and every one of them is a hard prerequisite rather than a
	// degradable nicety: a dispatcher.
	var alertWatcher *webhooks.AlertWatcher
	switch {
	case dispatcher == nil:
		// Silent when webhooks are simply not configured (the keyless state is
		// not news), and already warned about just above when they are
		// configured but unbuildable.
	case !cfg.Alerting.Enabled:
		slog.Info("alert webhooks are configured but alerting.enabled is false — no alert.fired/" +
			"alert.resolved deliveries (the watcher only reports alerts this console manages, and " +
			"with the rule reconciler off there are none)")
	case prom == nil:
		slog.Warn("alert webhooks are configured but prometheus.url is not set — no alert.fired/" +
			"alert.resolved deliveries (Prometheus evaluates the alerts; the console only reads " +
			"their state)")
	default:
		// watcherErr rather than err, enrichErr's reason: assigning the
		// function-scoped err this far down turns every earlier `x, err :=`
		// into a govet shadow report.
		aw, watcherErr := webhooks.NewAlertWatcher(webhooks.AlertWatcherDeps{
			Alerts:   prom,
			Notifier: dispatcher,
			// It is non-nil here by construction -- a dispatcher exists only when a database does.
			Rules:    db,
			Interval: cfg.Webhooks.AlertPollInterval,
		})
		if watcherErr != nil {
			slog.Warn("alert transition webhooks are off", "error", watcherErr)
			break
		}
		alertWatcher = aw
		slog.Info("alert transition webhooks enabled",
			"alertPollInterval", cfg.Webhooks.AlertPollInterval)
	}

	// Assigned through an interface-typed var behind an explicit nil check.
	var ruleSyncDep httpapi.RuleSyncer
	if ruleReconciler != nil {
		ruleSyncDep = ruleReconciler
	}

	srv := httpapi.NewServer(httpapi.Deps{
		Config: cfg, Metrics: m, PromRegistry: promReg, UI: uiHandler,
		Controller: ctrl, Prometheus: prom, Hub: hub, Realtime: ingester,
		Events:          eventsDep,
		TopologyHistory: topologyHistoryDep,
		Authenticator:   authenticator, Policy: policy, Roles: rolesDep, Sessions: sessions,
		Users: usersDep, OIDC: oidcDep,
		Audit: auditDep, RBAC: rbacDep, Tokens: tokensDep,
		Runner: runnerDep, Targets: targetsDep,
		Definitions: definitionsDep, Schedules: schedulesDep,
		MTR: mtrDep, Annotations: annotationsDep, Enricher: enricherDep,
		Incidents: incidentsDep, Maintenance: maintenanceDep,
		Webhooks: webhooksDep, K8sEvents: k8sEventsDep,
		AlertRules: alertRulesDep, RuleSync: ruleSyncDep,
		// All three are the same *webhooks.Dispatcher, or all three are nil; assigned through the
		// interface-typed vars above so a nil dispatcher stays a genuine nil interface rather than the
		// typed-nil trap.
		WebhookSealer:         webhookSealerDep,
		WebhookTestDispatcher: webhookTesterDep,
		IncidentNotifier:      incidentNotifierDep,
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
	if dispatcher != nil {
		// Dispatcher.Run only waits for cancellation and then drains.
		spawn("webhook-dispatcher", dispatcher.Run)
	}
	if alertWatcher != nil {
		// Spawned AHEAD of nothing in particular but stopped by the same bgCtx, which matters in one
		// specific way.
		spawn("alert-webhook-watcher", alertWatcher.Run)
	}
	if ruleReconciler != nil {
		// Every replica runs it: the apply is idempotent SSA of identical bytes, so there is no advisory
		// lock here and deliberately so.
		spawn("prometheus-rule-sync", ruleReconciler.Run)
	}

	// The schedule loop is opt-in (console.scheduler.enabled, default false) and needs BOTH a
	// database.
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
		// The continuous external-check reconciler rides the SAME gate.
		spawn("external-check-reconciler", checks.NewReconciler(checks.ReconcilerDeps{
			Lock: db, Store: db, Topology: ctrl, Controller: ctrl,
			Metrics: m, Interval: cfg.Scheduler.TickInterval, LockKey: scheduler.LockKey,
		}).Run)
		slog.Info("schedule loop enabled", "tickInterval", cfg.Scheduler.TickInterval)
	}

	// The Kubernetes event reader is opt-in (console.kubernetesContext.enabled, default false) and
	// needs a database.
	switch {
	case !cfg.KubernetesContext.Enabled:
	case db == nil:
		slog.Warn("kubernetesContext.enabled is set but no database is configured — kubernetes event " +
			"capture is off (captured events live in PostgreSQL; set console.database.mode)")
	default:
		clientset, kubeErr := buildInClusterClientset()
		if kubeErr != nil {
			slog.Warn("kubernetesContext.enabled is set but the in-cluster Kubernetes config is "+
				"unavailable — kubernetes event capture is off (the console is not running in a "+
				"cluster, or its ServiceAccount token is not mounted)", "error", kubeErr)
			break
		}
		// ctrl is a TYPED nil when controller.url is unset, and a typed nil in an interface is not a nil
		// interface.
		var topo kubectx.TopologySource
		if ctrl != nil {
			topo = ctrl
		} else {
			slog.Warn("kubernetesContext.enabled is set but controller.url is empty — node events " +
				"will be dropped (M6 Decision 3 fails closed: without a topology nothing vouches " +
				"for a node); pod events are unaffected")
		}
		reader := kubectx.New(cfg.KubernetesContext, clientset, topo, db, m)
		spawn("k8s-events-reader", reader.Run)
		slog.Info("kubernetes event capture enabled", //nolint:gosec // G706: namespace comes from operator config or the downward API, structured slog field
			"namespace", reader.Namespace(), "resyncInterval", cfg.KubernetesContext.ResyncInterval)
	}

	srv.SetReady(true)

	runErr := srv.Run(rootCtx)

	// It covers ONLY the components spawned above (hub.Run, ingester, pushers, relay).
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
	// Before db.Close(), for no functional reason -- the mmdb readers are
	// mmapped files and owe the database nothing -- but keeping the teardown
	// in the reverse order of construction is what makes it reviewable.
	if enricher != nil {
		if err := enricher.Close(); err != nil {
			slog.Warn("closing the hop enrichment mmdb readers failed", "error", err)
		}
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

// readSecretFile reads a file-mounted secret (an OIDC client secret, a bootstrap admin password)
// and trims surrounding whitespace; every auth secret is a file path, never an inline config value.
func readSecretFile(path string) (string, error) {
	data, err := os.ReadFile(path) //nolint:gosec // path comes from operator config (a mounted Secret), not user input -- same trust boundary as config.Load's own os.ReadFile
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

// bootstrapLocalAdminRole is the built-in role granted to the operator-named bootstrapAdmin user;
// without a binding, a freshly created user has zero roles and falls back to auth.defaultRole
// (empty by default = 403 on everything, middleware_auth.go's resolveRoles).
const bootstrapLocalAdminRole = "admin"

// bootstrapLocalAdmin implements; if the table is already non-empty, it does nothing and logs at
// DEBUG.
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
		// The table is populated, so no user is created.
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

// reconcileBootstrapAdminBinding re-creates the bootstrap admin's admin-role binding when the user
// row exists but the binding does not (a crash between CreateUser and CreateBinding on a previous
// boot).
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

// loadCustomRoles reads every custom (database-defined) role and reshapes it into the
// map[string][]authz.Permission authz.NewPolicy/(*authz.Policy).Reload take.
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

// refreshCustomRoles runs loadCustomRoles every rbacPolicyRefreshInterval and applies the result to
// policy via Reload.
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

// newBus selects the pub/sub backend the realtime pipeline runs on and returns a close func for it;
// an empty address — or a Valkey that cannot be dialled at startup.
func newBus(ctx context.Context, cfg *config.Config) (bus cache.Bus, closeBus func(), err error) {
	if cfg.Valkey.Address == "" {
		slog.Info("valkey.address not set — using the in-process bus (realtime fan-out is single-replica only)")
		return cache.NewInProcessBus(), func() {}, nil
	}

	// A file error is NOT the dial fallback below: an unreadable Secret is a broken mount.
	password, err := cfg.Valkey.ResolvePassword()
	if err != nil {
		return nil, nil, err
	}
	vb, err := cache.NewValkeyBus(ctx, cfg.Valkey.Address, cfg.Valkey.DialTimeout, password)
	if err != nil {
		slog.Warn("valkey unreachable at startup — falling back to the in-process bus; "+ //nolint:gosec // G706: address comes from the local config file, structured slog field
			"realtime fan-out is single-replica only until the console is restarted",
			"address", cfg.Valkey.Address, "error", err)
		return cache.NewInProcessBus(), func() {}, nil
	}

	slog.Info("valkey pub/sub enabled", "address", cfg.Valkey.Address) //nolint:gosec // G706: address comes from the local config file, structured slog field
	return vb, vb.Close, nil
}
