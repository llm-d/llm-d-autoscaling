/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"context"
	"crypto/tls"
	"errors"
	goflag "flag"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"time"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	flag "github.com/spf13/pflag"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/certwatcher"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	ctrlzap "sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	kedav1alpha1 "github.com/kedacore/keda/v2/apis/keda/v1alpha1"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/collector/locator"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/collector/registration"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/collector/source"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/collector/source/prometheus"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/controller"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/controller/indexers"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/coordinator"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/coordinator/plugins/gpurebalance"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/datastore"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/analyzers/throughput"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/pipeline"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/saturation"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/scalefromzero"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/logging"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/metrics"
	prometheusutil "github.com/llm-d/llm-d-workload-variant-autoscaler/internal/prometheus"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils/crd"
	poolutil "github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils/pool"
	promoperator "github.com/prometheus-operator/prometheus-operator/pkg/apis/monitoring/v1"
	"github.com/prometheus/client_golang/api"
	promv1 "github.com/prometheus/client_golang/api/prometheus/v1"
	corev1 "k8s.io/api/core/v1"
	crmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
	inferencePoolV1 "sigs.k8s.io/gateway-api-inference-extension/api/v1"
	inferencePoolV1alpha2 "sigs.k8s.io/gateway-api-inference-extension/apix/v1alpha2"
	lwsv1 "sigs.k8s.io/lws/api/leaderworkerset/v1"
	// +kubebuilder:scaffold:imports
)

var (
	scheme = runtime.NewScheme()
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(promoperator.AddToScheme(scheme))
	utilruntime.Must(inferencePoolV1.Install(scheme))
	utilruntime.Must(inferencePoolV1alpha2.Install(scheme))
	// KEDA scheme is registered unconditionally so the client can list ScaledObjects
	// when the CRD is present. Listing fails gracefully (NoMatchError) when not installed.
	utilruntime.Must(kedav1alpha1.AddToScheme(scheme))
	// Note: LeaderWorkerSet scheme is added conditionally in main() after checking if CRD exists
	// +kubebuilder:scaffold:scheme
}

// nolint:gocyclo
func main() {
	// Command-line flags

	loggerVerbosity := flag.Int("v", logging.DEFAULT, "number for the log level verbosity")

	configFilePath := flag.String("config-file", "", "Path to the YAML configuration file. "+
		"When set, the main configuration is read from this file instead of a Kubernetes ConfigMap.")

	disableCRDAutoRestart := flag.Bool("disable-crd-auto-restart", false,
		"Disable automatically restarting the controller when an optional CRD (KEDA ScaledObject or "+
			"LeaderWorkerSet) is installed after startup. When set, enabling support for a CRD installed "+
			"later requires a manual controller restart.")

	flag.String("metrics-bind-address", "0", "The address the metrics endpoint binds to. "+
		"Use :8443 for HTTPS or :8080 for HTTP, or leave as 0 to disable the metrics service.")
	flag.String("health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.Bool("leader-elect", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	flag.Bool("metrics-secure", true,
		"If set, the metrics endpoint is served securely via HTTPS. Use --metrics-secure=false to use HTTP instead.")
	flag.String("webhook-cert-path", "", "The directory that contains the webhook certificate.")
	flag.String("webhook-cert-name", "tls.crt", "The name of the webhook certificate file.")
	flag.String("webhook-cert-key", "tls.key", "The name of the webhook key file.")
	flag.String("metrics-cert-path", "",
		"The directory that contains the metrics server certificate.")
	flag.String("metrics-cert-name", "tls.crt", "The name of the metrics server certificate file.")
	flag.String("metrics-cert-key", "tls.key", "The name of the metrics key file.")
	flag.String("watch-namespace", "",
		"Namespace to watch for updates. If unspecified, all namespaces are watched.")

	// Leader election timeout configuration flags
	// These can be overridden in manager.yaml to tune for different environments
	// (e.g., higher values for environments with network latency or API server slowness)
	flag.Duration("leader-election-lease-duration", 60*time.Second,
		"The duration that non-leader candidates will wait to force acquire leadership. "+
			"Increased from default 15s to 60s to prevent lease renewal failures in environments with network latency.")
	flag.Duration("leader-election-renew-deadline", 50*time.Second,
		"The duration that the acting master will retry refreshing leadership before giving up. "+
			"Increased from default 10s to 50s to provide more tolerance for network latency and API server delays.")
	flag.Duration("leader-election-retry-period", 10*time.Second,
		"The duration the clients should wait between tries of actions. "+
			"Increased from default 2s to 10s to reduce API server load and provide more time between renewal attempts.")
	flag.Duration("rest-client-timeout", 60*time.Second,
		"The timeout for REST API calls to the Kubernetes API server. "+
			"Increased from default ~30s to 60s for better resilience against network latency.")

	opts := ctrlzap.Options{
		Development: true,
	}
	gfs := goflag.NewFlagSet("zap", goflag.ExitOnError)
	opts.BindFlags(gfs) // zap expects a standard Go FlagSet and pflag.FlagSet is not compatible.
	flag.CommandLine.AddGoFlagSet(gfs)

	flag.Parse()

	logging.InitLogging(&opts, loggerVerbosity)
	defer logging.Sync() // nolint:errcheck

	setupLog := ctrl.Log.WithName("setup")
	setupLog.Info("Logger initialized")

	// Get REST config early (needed for config loading)
	restConfig := ctrl.GetConfigOrDie()

	// Load unified configuration (fail-fast if invalid)
	// Viper resolves precedence: flags > env > config file > defaults
	// For more information see:
	// https://github.com/llm-d/llm-d-workload-variant-autoscaler/blob/main/docs/user-guide/configuration.md
	cfg, err := config.Load(flag.CommandLine, *configFilePath)
	if err != nil {
		setupLog.Error(err, "failed to load configuration - this is a fatal error")
		logging.Sync() //nolint:errcheck
		os.Exit(1)     //nolint:gocritic // exitAfterDefer: Sync() called explicitly above
	}
	setupLog.Info("Configuration loaded successfully")

	restartNote := "support requires a controller restart, which happens automatically if the CRD is installed later"
	if *disableCRDAutoRestart {
		restartNote = "support requires a manual controller restart after the CRD is installed " +
			"(automatic restart disabled via --disable-crd-auto-restart)"
	}

	// Conditionally add LeaderWorkerSet scheme if CRD exists
	lwsEnabled := crd.CheckLeaderWorkerSetCRD(restConfig, setupLog)
	if lwsEnabled {
		if err := lwsv1.AddToScheme(scheme); err != nil {
			setupLog.Error(err, "failed to add LeaderWorkerSet scheme")
			os.Exit(1)
		}
		setupLog.Info("LeaderWorkerSet CRD detected - support enabled")
	} else {
		setupLog.Info("LeaderWorkerSet CRD not found - support disabled (Deployment-only mode); " + restartNote)
	}

	// Detect KEDA for annotation-based ScaledObject discovery
	kedaEnabled := crd.CheckKEDACRD(restConfig, setupLog)
	if kedaEnabled {
		setupLog.Info("KEDA ScaledObject CRD detected - annotation-based ScaledObject discovery enabled")
	} else {
		setupLog.Info("KEDA ScaledObject CRD not found - annotation-based discovery limited to HPAs; " + restartNote)
	}
	// Gate the pod locator's ScaledObject lookups on KEDA availability. Set before
	// the saturation engine goroutine constructs its locator.
	locator.SetKEDAEnabled(kedaEnabled)

	crdTargets := []crd.Target{
		{
			CRDName:       "leaderworkersets.leaderworkerset.x-k8s.io",
			GroupVersion:  "leaderworkerset.x-k8s.io/v1",
			Kind:          "LeaderWorkerSet",
			PresentAtBoot: lwsEnabled,
		},
		{
			CRDName:       "scaledobjects.keda.sh",
			GroupVersion:  "keda.sh/v1alpha1",
			Kind:          "ScaledObject",
			PresentAtBoot: kedaEnabled,
		},
	}

	tlsOpts := []func(*tls.Config){
		func(c *tls.Config) {
			c.NextProtos = []string{"h2", "http/1.1"}
		},
	}

	// Create watchers for metrics and webhooks certificates
	var metricsCertWatcher, webhookCertWatcher *certwatcher.CertWatcher

	// Initial webhook TLS options
	webhookTLSOpts := tlsOpts

	if len(cfg.WebhookCertPath()) > 0 {
		setupLog.Info("Initializing webhook certificate watcher using provided certificates",
			"webhookCertPath", cfg.WebhookCertPath(),
			"webhookCertName", cfg.WebhookCertName(),
			"webhookCertKey", cfg.WebhookCertKey())

		var err error
		webhookCertWatcher, err = certwatcher.New(
			filepath.Join(cfg.WebhookCertPath(), cfg.WebhookCertName()),
			filepath.Join(cfg.WebhookCertPath(), cfg.WebhookCertKey()),
		)
		if err != nil {
			setupLog.Error(err, "Failed to initialize webhook certificate watcher")
			os.Exit(1)
		}

		webhookTLSOpts = append(webhookTLSOpts, func(config *tls.Config) {
			config.GetCertificate = webhookCertWatcher.GetCertificate
		})
	}

	webhookServer := webhook.NewServer(webhook.Options{
		TLSOpts: webhookTLSOpts,
	})

	// Metrics endpoint is enabled in 'config/base/kustomization.yaml'. The Metrics options configure the server.
	// More info:
	// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.20.4/pkg/metrics/server
	// - https://book.kubebuilder.io/reference/metrics.html
	metricsServerOptions := metricsserver.Options{
		BindAddress:   cfg.MetricsAddr(),
		SecureServing: cfg.SecureMetrics(),
		TLSOpts:       tlsOpts,
	}

	if cfg.SecureMetrics() {
		// FilterProvider is used to protect the metrics endpoint with authn/authz.
		// These configurations ensure that only authorized users and service accounts
		// can access the metrics endpoint. The RBAC are configured in 'config/base/rbac/kustomization.yaml'. More info:
		// https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.20.4/pkg/metrics/filters#WithAuthenticationAndAuthorization
		metricsServerOptions.FilterProvider = filters.WithAuthenticationAndAuthorization
	}

	// If the certificate is not specified, controller-runtime will automatically
	// generate self-signed certificates for the metrics server. While convenient for development and testing,
	// this setup is not recommended for production.
	//
	// TODO(user): If you enable certManager, uncomment the following lines:
	// - [METRICS-WITH-CERTS] at config/base/kustomization.yaml to generate and use certificates
	// managed by cert-manager for the metrics server.
	// - [PROMETHEUS-WITH-CERTS] at config/base/monitoring/kustomization.yaml for TLS certification.
	if len(cfg.MetricsCertPath()) > 0 {
		setupLog.Info("Initializing metrics certificate watcher using provided certificates",
			"metricsCertPath", cfg.MetricsCertPath(),
			"metricsCertName", cfg.MetricsCertName(),
			"metricsCertKey", cfg.MetricsCertKey(),
		)

		var err error
		metricsCertWatcher, err = certwatcher.New(
			filepath.Join(cfg.MetricsCertPath(), cfg.MetricsCertName()),
			filepath.Join(cfg.MetricsCertPath(), cfg.MetricsCertKey()),
		)
		if err != nil {
			setupLog.Error(err, "Failed to initialize metrics certificate watcher")
			os.Exit(1)
		}

		metricsServerOptions.TLSOpts = append(metricsServerOptions.TLSOpts, func(config *tls.Config) {
			config.GetCertificate = metricsCertWatcher.GetCertificate
		})
	}

	// --- Setup Datastore ---
	ds := datastore.NewDatastore(cfg)

	// Use configurable REST client timeout from Config (default 60s, can be overridden via --rest-client-timeout flag)
	restConfig.Timeout = cfg.RestTimeout()

	// Configure leader election with configurable timeouts to prevent lease renewal failures
	// Default values are: LeaseDuration=60s, RenewDeadline=50s, RetryPeriod=10s
	// These can be overridden via command-line flags in manager.yaml
	// Increased from controller-runtime defaults (15s, 10s, 2s) to provide more tolerance
	// for network latency and API server delays

	leaseDurationVal := cfg.LeaseDuration()
	renewDeadlineVal := cfg.RenewDeadline()
	retryPeriodVal := cfg.RetryPeriod()
	mgrOptions := ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsServerOptions,
		WebhookServer:          webhookServer,
		HealthProbeBindAddress: cfg.ProbeAddr(),
		LeaderElection:         cfg.EnableLeaderElection(),
		LeaderElectionID:       cfg.LeaderElectionID(),
		// Leader election timeout configuration (from Config, can be overridden via flags/env/ConfigMap)
		LeaseDuration: &leaseDurationVal,
		RenewDeadline: &renewDeadlineVal,
		RetryPeriod:   &retryPeriodVal,
		// LeaderElectionReleaseOnCancel defines if the leader should step down voluntarily
		// when the Manager ends. This requires the binary to immediately end when the
		// Manager is stopped, otherwise, this setting is unsafe. Setting this significantly
		// speeds up voluntary leader transitions as the new leader don't have to wait
		// LeaseDuration time first.
		//
		// This is safe to enable because the program ends immediately after the manager stops
		// (see mgr.Start() call at the end of main()). This enables fast failover during
		// deployments and upgrades, reducing downtime from ~60s to ~1-2s.
		LeaderElectionReleaseOnCancel: true,
	}

	watchNS := cfg.WatchNamespace()
	if watchNS != "" {
		setupLog.Info("Watching single namespace", "namespace", watchNS)
		mgrOptions.Cache = cache.Options{
			DefaultNamespaces: map[string]cache.Config{
				watchNS: {},
			},
		}
	} else {
		// Multi-namespace mode: Use label selector to filter ConfigMaps in the cache
		// This significantly reduces memory usage by only caching WVA-related configmaps
		wvaConfigSelector := labels.SelectorFromSet(labels.Set{
			"app.kubernetes.io/name": "workload-variant-autoscaler",
		})

		setupLog.Info("Configuring cache with label selector for ConfigMaps",
			"labelSelector", wvaConfigSelector.String())

		// Configure cache to only watch configmaps with the WVA labels
		// Other resource types are cached normally without filtering
		mgrOptions.Cache = cache.Options{
			ByObject: map[client.Object]cache.ByObject{
				&corev1.ConfigMap{}: {
					// Empty map means cache all namespaces, but filter by label
					Namespaces: map[string]cache.Config{},
					Label:      wvaConfigSelector,
				},
			},
		}
	}

	mgr, err := ctrl.NewManager(restConfig, mgrOptions)
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	// Setup custom indexes for lookups on HPAs and ScaledObjects
	setupLog.Info("Setting up indexes")
	if err := indexers.SetupIndexes(context.Background(), mgr, kedaEnabled); err != nil {
		setupLog.Error(err, "unable to setup indexes")
		os.Exit(1)
	}
	setupLog.Info("Indexes setup completed")

	// Initialize metrics
	setupLog.Info("Creating metrics emitter instance")
	// Force initialization of metrics by creating a metrics emitter
	_ = metrics.NewMetricsEmitter()
	setupLog.Info("Metrics emitter created successfully")

	// Create ConfigMap reconciler for configuration management.
	// Bootstrap uses the temporary uncached client so ConfigMap-backed settings
	// are loaded before any manager runnables start.
	configMapReconciler := &controller.ConfigMapReconciler{
		Reader:    mgr.GetAPIReader(),
		Scheme:    mgr.GetScheme(),
		Config:    cfg,
		Datastore: ds,
		Recorder:  mgr.GetEventRecorderFor("workload-variant-autoscaler-configmap-reconciler"),
	}

	ctx := context.Background()
	ctx = ctrl.LoggerInto(ctx, setupLog)
	if err = configMapReconciler.BootstrapInitialConfigMaps(ctx); err != nil {
		setupLog.Error(err, "unable to bootstrap initial ConfigMaps")
		os.Exit(1)
	}
	setupLog.Info("Initial ConfigMap bootstrap completed")

	// Frozen registration decision: analyzer registration cannot change without
	// a controller restart, so this value is captured once (after the bootstrap
	// above has loaded ConfigMap-backed settings) and shared with the
	// ConfigMapReconciler so it can detect a later live-config divergence.
	taRegistered := cfg.ThroughputAnalyzerEnabled()
	configMapReconciler.ThroughputRegistered = taRegistered

	// Use Prometheus configuration from unified Config (already validated during Load())
	if cfg.PrometheusBaseURL() == "" {
		setupLog.Error(nil, "no Prometheus configuration found - this should not happen after validation")
		os.Exit(1)
	}

	// Validate Prometheus transport configuration before creating the client.
	if err := prometheusutil.ValidateTLSConfig(cfg); err != nil {
		setupLog.Error(err, "Prometheus transport configuration validation failed")
		os.Exit(1)
	}

	promURL, _ := url.Parse(cfg.PrometheusBaseURL()) // already validated above
	setupLog.Info("Initializing Prometheus client",
		"address", promURL.Redacted(),
		"tlsEnabled", prometheusutil.IsHTTPS(cfg.PrometheusBaseURL()),
		"allowHTTP", cfg.PrometheusAllowHTTP(),
	)

	// Create Prometheus client with TLS support
	promClientConfig, err := prometheusutil.CreatePrometheusClientConfig(cfg)
	if err != nil {
		setupLog.Error(err, "failed to create prometheus client config")
		os.Exit(1)
	}

	promClient, err := api.NewClient(*promClientConfig)
	if err != nil {
		setupLog.Error(err, "failed to create prometheus client")
		os.Exit(1)
	}

	promAPI := promv1.NewAPI(promClient)

	// Validate that the API is working by testing a simple query with retry logic
	if err := prometheusutil.ValidatePrometheusAPI(context.Background(), promAPI); err != nil {
		setupLog.Error(err, "CRITICAL: Failed to connect to Prometheus - WVA requires Prometheus connectivity for autoscaling decisions")
		os.Exit(1)
	}
	setupLog.Info("Prometheus client and API wrapper initialized and validated successfully")

	// Register optimization engine loops with the manager. Only start when leader.
	err = mgr.Add(manager.RunnableFunc(func(ctx context.Context) error {
		sourceRegistry := source.NewSourceRegistry()
		setupLog.Info("Initializing metrics source registry")

		// Prometheus cache configuration is loaded via unified Config during startup.
		// The cache config is available in cfg.Dynamic.PrometheusCache and is updated
		// automatically when the ConfigMap changes. We use the default config here
		// as the unified Config system handles cache configuration loading.

		// Register PrometheusSource with default config
		promSource := prometheus.NewPrometheusSource(ctx, promAPI, prometheus.DefaultPrometheusSourceConfig())

		// Register in global source registry
		if err := sourceRegistry.Register("prometheus", promSource); err != nil {
			setupLog.Error(err, "failed to register prometheus source in source registry")
			os.Exit(1)
		}

		// Build the initial GPU limiter from the effective limiter mode — the
		// limiters: list on the saturation "default" config, or the inventory
		// default when none is declared. The ConfigMaps were bootstrapped above,
		// so the selection is already visible here. The engine rebuilds the limiter
		// live (see SetLimiterBuilder) when the ConfigMap changes.
		gpuLimiter, err := pipeline.NewLimiterFromConfig(cfg, mgr.GetClient())
		if err != nil {
			setupLog.Error(err, "failed to build GPU limiter")
			return err
		}
		setupLog.Info("GPU limiter constructed", "type", cfg.EffectiveLimiterMode(), "name", gpuLimiter.Name())

		// The GPU limiter is only consulted on the limited optimizer path, which
		// the engine selects per-model when enableLimiter is true in the
		// saturation-scaling ConfigMap. Selecting quota mode via limiters: alone
		// does NOT enforce quotas unless enableLimiter is also set, so warn.
		if cfg.EffectiveLimiterMode() == config.LimiterTypeQuota {
			setupLog.Info("Quota limiter selected; quota caps are enforced ONLY when " +
				"enableLimiter: true is set in the saturation-scaling ConfigMap. " +
				"With the default enableLimiter: false the engine runs the unlimited " +
				"optimizer and quota caps are not applied.")
		}

		// Quota mode means "no physical-capacity discovery" — including the
		// inventory-collection call in the saturation engine. We honor that
		// at the call site (see saturation.shouldCollectClusterInventory),
		// but warn loudly here so an operator who explicitly enabled
		// WVA_LIMITED_MODE sees that their inventory log will be suppressed.
		if cfg.EffectiveLimiterMode() == config.LimiterTypeQuota && cfg.LimitedModeEnabled() {
			setupLog.Info("Quota limiter mode is active; cluster inventory collection is disabled "+
				"despite WVA_LIMITED_MODE=true (no Node API access in quota mode). "+
				"To re-enable cluster inventory logging, switch to inventory mode.",
				"limiterType", cfg.EffectiveLimiterMode(),
				"limitedModeEnabled", cfg.LimitedModeEnabled())
		}

		engine := saturation.NewEngine(
			mgr.GetClient(),
			mgr.GetAPIReader(),
			mgr.GetScheme(),
			mgr.GetEventRecorderFor("workload-variant-autoscaler-saturation-engine"),
			sourceRegistry,
			cfg, // Pass unified Config to engine
			gpuLimiter,
		)
		// Rebuild the limiter live when the saturation ConfigMap's limiters: list
		// changes — no restart required. The builder re-reads the effective config.
		engine.SetLimiterBuilder(func() (pipeline.Limiter, error) {
			return pipeline.NewLimiterFromConfig(cfg, mgr.GetClient())
		})
		if taRegistered {
			registration.RegisterThroughputAnalyzerQueries(sourceRegistry)
			if err := engine.RegisterAnalyzer(throughput.AnalyzerName, throughput.NewThroughputAnalyzer()); err != nil {
				return err
			}
			setupLog.Info("ThroughputAnalyzer registered (enabled in saturation config)")
		} else {
			setupLog.Info("ThroughputAnalyzer NOT registered — no saturation config entry " +
				"enables 'throughput'. Add it to the analyzers config and restart the " +
				"controller to enable it.")
		}
		go engine.StartOptimizeLoop(ctx)
		return nil
	}))

	if err != nil {
		setupLog.Error(err, "unable to add optimization engine loop to manager")
		os.Exit(1)
	}

	// Register scale from zero engine loop with the manager. Only start when leader.
	err = mgr.Add(manager.RunnableFunc(func(ctx context.Context) error {
		engine, err := scalefromzero.NewEngine(mgr.GetClient(), mgr.GetEventRecorderFor("workload-variant-autoscaler-scalezero-engine"), mgr.GetRESTMapper(), restConfig, ds, cfg)
		if err != nil {
			return err
		}
		go engine.StartOptimizeLoop(ctx)
		return nil
	}))

	if err != nil {
		setupLog.Error(err, "unable to add optimization engine loop to manager")
		os.Exit(1)
	}

	// HPAReconciler: tracks namespaces for annotation-based discovery (always registered).
	if err = (&controller.HPAReconciler{
		Client:    mgr.GetClient(),
		Datastore: ds,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create HPA controller")
		os.Exit(1)
	}

	// ScaledObjectReconciler: registered only when KEDA CRD is present.
	if kedaEnabled {
		if err = (&controller.ScaledObjectReconciler{
			Client:    mgr.GetClient(),
			Datastore: ds,
		}).SetupWithManager(mgr); err != nil {
			setupLog.Error(err, "unable to create ScaledObject controller")
			os.Exit(1)
		}
	}
	// +kubebuilder:scaffold:builder

	// Create InferencePool reconciler
	poolGroupEnv := os.Getenv("POOL_GROUP")
	poolGKNN, err := poolutil.GetPoolGKNN(poolGroupEnv)
	if err != nil {
		setupLog.Error(err, "unable to create default pool GKNN from POOL_GROUP", "poolGroup", poolGroupEnv)
		os.Exit(1)
	}
	inferencePoolReconciler := &controller.InferencePoolReconciler{
		Datastore: ds,
		Client:    mgr.GetClient(),
		PoolGKNN:  poolGKNN,
	}

	if err = inferencePoolReconciler.SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create inferencePool controller")
		os.Exit(1)
	}

	if err = configMapReconciler.SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create configmap controller")
		os.Exit(1)
	}

	// Coordinator: cluster-wide leader-elected loop that dispatches a
	// selected set of HPAs and ScaledObjects to registered plugins.
	// EXPERIMENTAL: off by default; opt in by setting EXPERIMENTAL_COORDINATOR_ENABLED: "true"
	// in the manager ConfigMap.
	if cfg.CoordinatorEnabled() {
		setupLog.Info("WARNING: Coordinator is an experimental feature. " +
			"It may change or be removed in future releases. " +
			"Do not use in production without understanding the risks.")
		plugins := []coordinator.Plugin{
			gpurebalance.New(mgr.GetClient(), promAPI),
		}
		coord, err := coordinator.New(mgr.GetClient(), plugins, coordinator.Options{
			Interval:    cfg.CoordinatorInterval(),
			KEDAEnabled: kedaEnabled,
		})
		if err != nil {
			setupLog.Error(err, "unable to construct Coordinator")
			os.Exit(1)
		}
		if err := mgr.Add(coord); err != nil {
			setupLog.Error(err, "unable to add Coordinator to manager")
			os.Exit(1)
		}
		setupLog.Info("Coordinator enabled",
			"interval", cfg.CoordinatorInterval(),
			"kedaEnabled", kedaEnabled,
		)
	} else {
		setupLog.Info("Coordinator disabled (experimental feature; set EXPERIMENTAL_COORDINATOR_ENABLED=true to enable)")
	}

	if *disableCRDAutoRestart {
		setupLog.Info("Automatic restart on post-startup CRD installation is disabled " +
			"(--disable-crd-auto-restart); install-then-restart must be performed manually")
	} else if err := mgr.Add(crd.NewWatcher(restConfig, crdTargets)); err != nil {
		setupLog.Error(err, "unable to add CRD watcher to manager")
		os.Exit(1)
	}

	if metricsCertWatcher != nil {
		setupLog.Info("Adding metrics certificate watcher to manager")
		if err := mgr.Add(metricsCertWatcher); err != nil {
			setupLog.Error(err, "unable to add metrics certificate watcher to manager")
			os.Exit(1)
		}
	}

	if webhookCertWatcher != nil {
		setupLog.Info("Adding webhook certificate watcher to manager")
		if err := mgr.Add(webhookCertWatcher); err != nil {
			setupLog.Error(err, "unable to add webhook certificate watcher to manager")
			os.Exit(1)
		}
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", func(_ *http.Request) error {
		if cfg.ConfigMapsBootstrapComplete() {
			return nil
		}
		_, _, syncErr := cfg.ConfigMapsBootstrapSyncStatus()
		if syncErr != "" {
			return fmt.Errorf("initial ConfigMap bootstrap not complete: %s", syncErr)
		}
		return errors.New("initial ConfigMap bootstrap not complete")
	}); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("Starting manager")

	// Sync the custom logger before starting the manager
	if err := logging.Sync(); err != nil {
		setupLog.Error(err, "Failed to sync logger before starting manager")
		os.Exit(1)
	}

	// Register custom metrics with the controller-runtime Prometheus registry
	// This makes the metrics available for scraping by Prometheus and direct endpoint access
	setupLog.Info("Registering custom metrics with Prometheus registry")
	if err := metrics.InitMetrics(crmetrics.Registry); err != nil {
		setupLog.Error(err, "failed to initialize metrics")
		os.Exit(1)
	}

	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		if errors.Is(err, crd.ErrRestartRequired) {
			setupLog.Info("restarting to enable support for a CRD installed after startup", "reason", err.Error())
			os.Exit(1)
		}
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}
