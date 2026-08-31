package main

import (
	"flag"
	"os"
	"time"

	t3v1alpha1 "github.com/janpuc/t3-code-operator/api/v1alpha1"
	"github.com/janpuc/t3-code-operator/internal/controller"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

func main() {
	var leaderElection bool
	var healthProbeAddress string
	var miseBinary string
	var miseCacheDirectory string
	var toolResolutionTimeout time.Duration
	var activityFreshness time.Duration
	var logOptions zap.Options

	flag.BoolVar(&leaderElection, "leader-elect", true, "Use leader election for the operator manager.")
	flag.StringVar(&healthProbeAddress, "health-probe-bind-address", ":8081", "Bind address for health probes. Use 0 to disable them.")
	flag.StringVar(&miseBinary, "mise-binary", "/usr/local/bin/mise", "Absolute path to the mise executable.")
	flag.StringVar(&miseCacheDirectory, "mise-cache-directory", "/var/cache/t3-code-operator/mise", "Absolute path to the shared mise resolution cache.")
	flag.DurationVar(&toolResolutionTimeout, "tool-resolution-timeout", 2*time.Minute, "Maximum duration for one mise platform resolution.")
	flag.DurationVar(&activityFreshness, "activity-freshness", 15*time.Second, "Maximum age of a drain activity report.")
	logOptions.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&logOptions)))
	setupLog := ctrl.Log.WithName("setup")

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		setupLog.Error(err, "cannot register the Kubernetes scheme")
		os.Exit(1)
	}
	if err := t3v1alpha1.AddToScheme(scheme); err != nil {
		setupLog.Error(err, "cannot register the t3-code scheme")
		os.Exit(1)
	}

	manager, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                        scheme,
		LeaderElection:                leaderElection,
		LeaderElectionID:              "t3-code-operator.t3code.janpuc.com",
		LeaderElectionReleaseOnCancel: true,
		Metrics:                       metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress:        healthProbeAddress,
		PprofBindAddress:              "0",
	})
	if err != nil {
		setupLog.Error(err, "cannot create the operator manager")
		os.Exit(1)
	}

	toolResolver, err := controller.NewMiseToolResolver(controller.MiseToolResolverConfig{
		Binary:         miseBinary,
		CacheDirectory: miseCacheDirectory,
		Timeout:        toolResolutionTimeout,
	})
	if err != nil {
		setupLog.Error(err, "cannot create the tool resolver")
		os.Exit(1)
	}
	assembler := &controller.Assembler{Reader: manager.GetClient(), Tools: toolResolver}

	controllers := []interface{ SetupWithManager(ctrl.Manager) error }{
		&controller.WorkstationReconciler{
			Client:            manager.GetClient(),
			Scheme:            manager.GetScheme(),
			Assembler:         assembler,
			ActivityFreshness: activityFreshness,
		},
		&controller.HarnessReconciler{Client: manager.GetClient(), Assembler: assembler},
		&controller.ExtensionReconciler{Client: manager.GetClient(), Assembler: assembler},
		&controller.MCPServerReconciler{Client: manager.GetClient(), Assembler: assembler},
	}
	for _, reconciler := range controllers {
		if err := reconciler.SetupWithManager(manager); err != nil {
			setupLog.Error(err, "cannot register a controller")
			os.Exit(1)
		}
	}

	if healthProbeAddress != "0" {
		if err := manager.AddHealthzCheck("healthz", healthz.Ping); err != nil {
			setupLog.Error(err, "cannot register the health check")
			os.Exit(1)
		}
		if err := manager.AddReadyzCheck("readyz", healthz.Ping); err != nil {
			setupLog.Error(err, "cannot register the readiness check")
			os.Exit(1)
		}
	}

	setupLog.Info("starting the operator manager")
	if err := manager.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "operator manager stopped with an error")
		os.Exit(1)
	}
}
