// @title           K8s Agent Orchestrator API
// @version         1.0
// @description     REST API for managing Kubernetes Agents (pods) — lifecycle control, env patching, log streaming and per-agent cache.
// @contact.name    Jacek Myjkowski
// @license.name    MIT
// @host            localhost:8082
// @BasePath        /
// @schemes         http
// @tag.name         health
// @tag.description  Liveness and readiness probes
// @tag.name         agents
// @tag.description  Agent (Pod) CRUD operations
// @tag.name         lifecycle
// @tag.description  Start / stop / restart agents
// @tag.name         env
// @tag.description  Environment variable management
// @tag.name         logs
// @tag.description  Pod log retrieval and streaming
// @tag.name         cache
// @tag.description  Per-agent in-memory key-value cache
package main

import (
	"context"
	"flag"
	"os"
	"time"

	// Import all Kubernetes client auth plugins.
	"net/http"

	_ "k8s.io/client-go/plugin/pkg/client/auth"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	orchestratov1alpha1 "github.com/jacekmyjkowski/k8s-agent-orchestrator/api/v1alpha1"
	_ "github.com/jacekmyjkowski/k8s-agent-orchestrator/docs"
	"github.com/jacekmyjkowski/k8s-agent-orchestrator/internal/cache"
	"github.com/jacekmyjkowski/k8s-agent-orchestrator/internal/controller"
	"github.com/jacekmyjkowski/k8s-agent-orchestrator/internal/history"
	"github.com/jacekmyjkowski/k8s-agent-orchestrator/internal/idle"
	"github.com/jacekmyjkowski/k8s-agent-orchestrator/internal/rest"
	uiserver "github.com/jacekmyjkowski/k8s-agent-orchestrator/internal/ui"
	redis "github.com/redis/go-redis/v9"
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(orchestratov1alpha1.AddToScheme(scheme))
}

func main() {
	var (
		metricsAddr        string
		probeAddr          string
		restAddr           string
		defaultNamespace   string
		leaderElect        bool
		debug              bool
		idleTimeoutDefault int
		idleCheckInterval  int
		uiAddr             string
		// Redis / history flags
		redisAddr         string
		redisPassword     string
		redisDB           int
		historyMaxEntries int
	)

	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "Address to bind the metrics endpoint.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "Address to bind health probes.")
	flag.StringVar(&restAddr, "rest-bind-address", ":8082", "Address to bind the REST API server.")
	flag.StringVar(&defaultNamespace, "default-namespace", "default", "Default namespace for REST API requests that omit the namespace.")
	flag.BoolVar(&leaderElect, "leader-elect", false, "Enable leader election for the controller manager.")
	flag.BoolVar(&debug, "debug", false, "Enable debug logging and Gin debug mode.")
	flag.IntVar(&idleTimeoutDefault, "idle-timeout-default", 0, "Global idle timeout in seconds. 0 disables idle tracking unless overridden per-agent via spec.idleTimeout.")
	flag.IntVar(&idleCheckInterval, "idle-check-interval", 30, "How often (in seconds) the idle watcher checks all agents.")
	flag.StringVar(&uiAddr, "ui-bind-address", ":8083", "Address to bind the web UI dashboard.")
	flag.StringVar(&redisAddr, "redis-addr", "", "Redis address (host:port). Empty = in-memory fallback.")
	flag.StringVar(&redisPassword, "redis-password", "", "Redis password.")
	flag.IntVar(&redisDB, "redis-db", 0, "Redis database index.")
	flag.IntVar(&historyMaxEntries, "history-max-entries", 1000, "Maximum lifecycle events per agent kept in history.")
	flag.Parse()

	// Read override from env (useful inside pod via Helm values).
	if ns := os.Getenv("WATCH_NAMESPACE"); ns != "" {
		defaultNamespace = ns
	}

	opts := zap.Options{Development: debug}
	opts.BindFlags(flag.CommandLine)
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	// ── Controller Manager ────────────────────────────────────────────────────
	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme: scheme,
		Metrics: metricsserver.Options{
			BindAddress: metricsAddr,
		},
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         leaderElect,
		LeaderElectionID:       "k8s-agent-orchestrator-leader",
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	// ── Cache and History Stores ──────────────────────────────────────────────
	var cacheStore cache.CacheStore
	var historyStore history.HistoryStore
	if redisAddr != "" {
		rdb := redis.NewClient(&redis.Options{
			Addr:     redisAddr,
			Password: redisPassword,
			DB:       redisDB,
		})
		if _, err := rdb.Ping(context.Background()).Result(); err != nil {
			setupLog.Error(err, "unable to connect to Redis", "addr", redisAddr)
			os.Exit(1)
		}
		setupLog.Info("Using Redis-backed cache and history", "addr", redisAddr)
		cacheStore = cache.NewRedisCache(rdb)
		historyStore = history.NewRedisHistory(rdb, historyMaxEntries)
	} else {
		setupLog.Info("Using in-memory cache and history (no Redis configured)")
		cacheStore = cache.NewInMemoryCache()
		historyStore = history.NewInMemoryHistory(historyMaxEntries)
	}

	// ── Idle Watcher ──────────────────────────────────────────────────────────
	idleWatcher := &idle.Watcher{
		Client:        mgr.GetClient(),
		Cache:         cacheStore,
		GlobalTimeout: time.Duration(idleTimeoutDefault) * time.Second,
		CheckInterval: time.Duration(idleCheckInterval) * time.Second,
	}

	// ── Agent Controller ──────────────────────────────────────────────────────
	agentReconciler := &controller.AgentReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Recorder: mgr.GetEventRecorderFor("agent-controller"),
		History:  historyStore,
	}
	if err = agentReconciler.SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "Agent")
		os.Exit(1)
	}

	// ── Health Checks ─────────────────────────────────────────────────────────
	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	// ── REST API Server ───────────────────────────────────────────────────────
	// Run in a separate goroutine so it doesn't block the controller manager.
	restServer := rest.NewServer(mgr.GetClient(), cacheStore, agentReconciler, mgr.GetEventRecorderFor("agent-rest-api"), defaultNamespace, debug, idleWatcher, historyStore)
	go func() {
		setupLog.Info("Starting REST API server", "addr", restAddr)
		if err := restServer.Run(restAddr); err != nil {
			setupLog.Error(err, "REST API server error")
			os.Exit(1)
		}
	}()

	// ── Web UI Server ────────────────────────────────────────────────────────
	go func() {
		setupLog.Info("Starting web UI", "addr", uiAddr)
		if err := http.ListenAndServe(uiAddr, uiserver.NewHandler()); err != nil {
			setupLog.Error(err, "Web UI server error")
		}
	}()

	// ── Start Manager (blocking) ──────────────────────────────────────────────
	setupLog.Info("Starting controller manager")
	ctx := ctrl.SetupSignalHandler()

	// Start idle watcher only when at least one timeout source is configured.
	if idleTimeoutDefault > 0 {
		setupLog.Info("Starting idle watcher",
			"globalTimeoutSec", idleTimeoutDefault,
			"checkIntervalSec", idleCheckInterval,
		)
		go idleWatcher.Start(ctx)
	} else {
		setupLog.Info("Idle watcher disabled (--idle-timeout-default=0); per-agent spec.idleTimeout still active")
		// Start watcher anyway so per-agent timeouts work; watcher skips agents with effective timeout==0.
		go idleWatcher.Start(ctx)
	}

	if err := mgr.Start(ctx); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}
