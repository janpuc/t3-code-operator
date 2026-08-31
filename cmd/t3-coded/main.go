package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/janpuc/t3-code-operator/internal/apply"
	"github.com/janpuc/t3-code-operator/internal/render"
	"github.com/janpuc/t3-code-operator/internal/sidecar"
	"github.com/janpuc/t3-code-operator/internal/t3client"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

type options struct {
	namespace              string
	workstationName        string
	workstationUID         string
	podRevision            string
	manifestConfigMapName  string
	reportConfigMapName    string
	dataRoot               string
	workspaceRoot          string
	t3BaseDirectory        string
	t3BaseURL              string
	t3Binary               string
	codexBinary            string
	claudeBinary           string
	miseBinary             string
	retryInterval          time.Duration
	refreshInterval        time.Duration
	repositoryScanInterval time.Duration
	probeBindAddress       string
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, parseOptions()); err != nil {
		log.Fatal(err)
	}
}

func parseOptions() options {
	var value options
	flag.StringVar(&value.namespace, "namespace", os.Getenv("POD_NAMESPACE"), "Workstation namespace")
	flag.StringVar(&value.workstationName, "workstation-name", os.Getenv("WORKSTATION_NAME"), "Workstation name")
	flag.StringVar(&value.workstationUID, "workstation-uid", os.Getenv("WORKSTATION_UID"), "Workstation UID")
	flag.StringVar(&value.podRevision, "pod-revision", os.Getenv("T3_CODE_POD_REVISION"), "Workstation Pod revision")
	flag.StringVar(&value.manifestConfigMapName, "manifest-configmap", os.Getenv("MANIFEST_CONFIGMAP"), "rendered manifest ConfigMap")
	flag.StringVar(&value.reportConfigMapName, "report-configmap", os.Getenv("REPORT_CONFIGMAP"), "sidecar report ConfigMap")
	flag.StringVar(&value.dataRoot, "data-root", "/data", "persistent data root")
	flag.StringVar(&value.workspaceRoot, "workspace-root", "/workspace", "workspace root")
	flag.StringVar(&value.t3BaseDirectory, "t3-base-dir", "/data/t3", "upstream t3 base directory")
	flag.StringVar(&value.t3BaseURL, "t3-url", "http://127.0.0.1:3000", "loopback upstream t3 URL")
	flag.StringVar(&value.t3Binary, "t3-binary", "t3", "upstream t3 CLI path")
	flag.StringVar(&value.codexBinary, "codex-binary", "codex", "Codex CLI path")
	flag.StringVar(&value.claudeBinary, "claude-binary", "claude", "Claude CLI path")
	flag.StringVar(&value.miseBinary, "mise-binary", "mise", "mise CLI path")
	flag.DurationVar(&value.retryInterval, "retry-interval", sidecar.DefaultRetryInterval, "failed apply retry interval")
	flag.DurationVar(&value.refreshInterval, "refresh-interval", sidecar.DefaultRefreshInterval, "upstream drift refresh interval")
	flag.DurationVar(&value.repositoryScanInterval, "repository-scan-interval", apply.DefaultRepositoryScanInterval, "Git repository scan interval")
	flag.StringVar(&value.probeBindAddress, "probe-bind-address", ":8082", "health and readiness probe bind address")
	flag.Parse()
	return value
}

func run(ctx context.Context, value options) error {
	if value.namespace == "" || value.workstationName == "" || value.workstationUID == "" || value.podRevision == "" ||
		value.manifestConfigMapName == "" || value.reportConfigMapName == "" {
		return errors.New("namespace, Workstation identity, and ConfigMap names are required")
	}
	if err := os.MkdirAll(value.t3BaseDirectory, 0o700); err != nil {
		return errors.New("create t3 base directory")
	}

	clusterConfig, err := rest.InClusterConfig()
	if err != nil {
		return errors.New("load in-cluster Kubernetes configuration")
	}
	clusterConfig.Timeout = 30 * time.Second
	clientset, err := kubernetes.NewForConfig(clusterConfig)
	if err != nil {
		return errors.New("create Kubernetes client")
	}
	store, err := sidecar.NewKubernetesStore(clientset.CoreV1(), sidecar.StoreConfig{
		Namespace:             value.namespace,
		ManifestConfigMapName: value.manifestConfigMapName,
		ReportConfigMapName:   value.reportConfigMapName,
	})
	if err != nil {
		return err
	}
	t3Version, err := t3client.DetectVersion(ctx, value.t3Binary)
	if err != nil {
		t3Version = "unknown"
	}

	auth, err := t3client.NewAuthTokenManager(t3client.AuthConfig{
		BaseURL:       value.t3BaseURL,
		BaseDirectory: value.t3BaseDirectory,
		ClientID:      value.workstationUID,
		T3Binary:      value.t3Binary,
	})
	if err != nil {
		return err
	}
	defer func() {
		closeContext, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = auth.Close(closeContext)
	}()

	upstream, err := t3client.New(t3client.Config{
		BaseURL: value.t3BaseURL,
		Tokens:  auth,
	})
	if err != nil {
		return err
	}
	activity, err := t3client.NewStableActivityReader(upstream, t3client.DefaultStableIdleWindow)
	if err != nil {
		return err
	}
	installers, err := apply.NewNativeExtensionInstallers(apply.NativeExtensionInstallerConfig{
		DataRoot:     value.dataRoot,
		CodexBinary:  value.codexBinary,
		ClaudeBinary: value.claudeBinary,
	})
	if err != nil {
		return err
	}
	extensions, err := apply.NewCachedExtensionManager(apply.CachedExtensionManagerConfig{
		DataRoot:   value.dataRoot,
		Fetchers:   apply.DefaultExtensionFetchers(value.dataRoot),
		Installers: installers,
	})
	if err != nil {
		return err
	}
	miseRuntime, err := apply.NewMiseRuntime(apply.MiseRuntimeConfig{
		Binary:   value.miseBinary,
		DataRoot: value.dataRoot,
	})
	if err != nil {
		return err
	}
	tools, err := apply.NewMiseToolManager(apply.MiseToolManagerConfig{
		DataRoot: value.dataRoot,
		Runtime:  miseRuntime,
	})
	if err != nil {
		return err
	}
	applier, err := apply.New(apply.Config{
		DataRoot:               value.dataRoot,
		WorkspaceRoot:          value.workspaceRoot,
		RepositoryScanInterval: value.repositoryScanInterval,
		Secrets:                store,
		Upstream:               upstream,
		Activity:               activity,
		Extensions:             extensions,
		Tools:                  tools,
	})
	if err != nil {
		return err
	}
	probes := &sidecar.ProbeState{}
	runner, err := sidecar.NewRunner(sidecar.RunnerConfig{
		Store:    store,
		Applier:  applier,
		Activity: activity,
		Workstation: render.WorkstationIdentity{
			Namespace: value.namespace,
			Name:      value.workstationName,
			UID:       value.workstationUID,
		},
		PodRevision:     value.podRevision,
		T3Version:       t3Version,
		RetryInterval:   value.retryInterval,
		RefreshInterval: value.refreshInterval,
		Probes:          probes,
	})
	if err != nil {
		return err
	}
	server := &http.Server{
		Addr:              value.probeBindAddress,
		Handler:           probes.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	serverErrors := make(chan error, 1)
	go func() {
		serverErrors <- server.ListenAndServe()
	}()
	runnerErrors := make(chan error, 1)
	go func() {
		runnerErrors <- runner.Run(ctx)
	}()
	select {
	case err := <-serverErrors:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	case err := <-runnerErrors:
		shutdownContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		shutdownErr := server.Shutdown(shutdownContext)
		return errors.Join(err, shutdownErr)
	case <-ctx.Done():
		shutdownContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return server.Shutdown(shutdownContext)
	}
}
