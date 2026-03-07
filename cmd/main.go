package main

import (
	"flag"
	"os"

	// Import all Kubernetes client auth plugins.
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	orchestratorv1alpha1 "github.com/jacekmyjkowski/k8s-agent-orchestrator/api/v1alpha1"
	"github.com/jacekmyjkowski/k8s-agent-orchestrator/internal/cache"
	"github.com/jacekmyjkowski/k8s-agent-orchestrator/internal/controller"
	"github.com/jacekmyjkowski/k8s-agent-orchestrator/internal/rest"
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(orchestratorv1alpha1.AddToScheme(scheme))
}

func main() {
	var (
		metricsAddr          string
		probeAddr            string
		restAddr             string
		defaultNamespace     string
		leaderElect          bool
		debug                bool
	)

	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "Address to bind the metrics endpoint.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "Address to bind health probes.")
	flag.StringVar(&restAddr, "rest-bind-address", ":8082", "Address to bind the REST API server.")
	flag.StringVar(&defaultNamespace, "default-namespace", "default", "Default namespace for REST API requests that omit the namespace.")
	flag.BoolVar(&leaderElect, "leader-elect", false, "Enable leader election for the controller manager.")
	flag.BoolVar(&debug, "debug", false, "Enable debug logging and Gin debug mode.")
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

	// ── In-memory Cache ───────────────────────────────────────────────────────
	cacheManager := cache.NewAgentCacheManager()

	// ── Agent Controller ──────────────────────────────────────────────────────
	agentReconciler := &controller.AgentReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
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
	restServer := rest.NewServer(mgr.GetClient(), cacheManager, agentReconciler, defaultNamespace, debug)
	go func() {
		setupLog.Info("Starting REST API server", "addr", restAddr)
		if err := restServer.Run(restAddr); err != nil {
			setupLog.Error(err, "REST API server error")
			os.Exit(1)
		}
	}()

	// ── Start Manager (blocking) ──────────────────────────────────────────────
	setupLog.Info("Starting controller manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}
