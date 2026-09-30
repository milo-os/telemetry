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
	"flag"
	"fmt"
	"os"
	"path/filepath"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.

	"golang.org/x/sync/errgroup"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	_ "k8s.io/client-go/plugin/pkg/client/auth"
	"k8s.io/client-go/tools/clientcmd"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/certwatcher"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/cluster"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"
	"sigs.k8s.io/multicluster-runtime/pkg/multicluster"
	mcsingle "sigs.k8s.io/multicluster-runtime/providers/single"

	telemetryv1alpha1 "go.datum.net/o11y/operator/api/v1alpha1"
	"go.datum.net/o11y/operator/internal/config"
	"go.datum.net/o11y/operator/internal/controller"
	milomulticluster "go.miloapis.com/milo/pkg/multicluster-runtime"
	miloprovider "go.miloapis.com/milo/pkg/multicluster-runtime/milo"
	// +kubebuilder:scaffold:imports
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
	codecs   = serializer.NewCodecFactory(scheme, serializer.EnableStrict)

	// Build information. Populated at build-time via -ldflags.
	version      = "dev"
	gitCommit    = "unknown"
	gitTreeState = "unknown"
	buildDate    = "unknown"
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))

	utilruntime.Must(telemetryv1alpha1.AddToScheme(scheme))
	// +kubebuilder:scaffold:scheme
}

// nolint:gocyclo
func main() {
	var metricsAddr string
	var metricsCertPath, metricsCertName, metricsCertKey string
	var webhookCertPath, webhookCertName, webhookCertKey string
	var vectorConfigLabelKey, vectorConfigLabelValue string
	var enableLeaderElection bool
	var probeAddr string
	var secureMetrics bool
	var enableHTTP2 bool
	var tlsOpts []func(*tls.Config)
	var vectorConfigurationNamespace string
	var upstreamClusterKubeconfig string
	var serverConfigFile string
	flag.StringVar(&metricsAddr, "metrics-bind-address", "0", "The address the metrics endpoint binds to. "+
		"Use :8443 for HTTPS or :8080 for HTTP, or leave as 0 to disable the metrics service.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	flag.BoolVar(&secureMetrics, "metrics-secure", true,
		"If set, the metrics endpoint is served securely via HTTPS. Use --metrics-secure=false to use HTTP instead.")
	flag.StringVar(&webhookCertPath, "webhook-cert-path", "", "The directory that contains the webhook certificate.")
	flag.StringVar(&webhookCertName, "webhook-cert-name", "tls.crt", "The name of the webhook certificate file.")
	flag.StringVar(&webhookCertKey, "webhook-cert-key", "tls.key", "The name of the webhook key file.")
	flag.StringVar(&metricsCertPath, "metrics-cert-path", "",
		"The directory that contains the metrics server certificate.")
	flag.StringVar(&metricsCertName, "metrics-cert-name", "tls.crt", "The name of the metrics server certificate file.")
	flag.StringVar(&metricsCertKey, "metrics-cert-key", "tls.key", "The name of the metrics server key file.")
	flag.BoolVar(&enableHTTP2, "enable-http2", false,
		"If set, HTTP/2 will be enabled for the metrics and webhook servers")
	flag.StringVar(
		&vectorConfigLabelKey,
		"vector-config-label-key",
		"telemetry.miloapis.com/vector-export-policy-config",
		"The key of the label that will be added to the vector config secret.",
	)
	flag.StringVar(&vectorConfigLabelValue,
		"vector-config-label-value",
		"true",
		"The value of the label that will be added to the vector config secret.",
	)
	flag.StringVar(&vectorConfigurationNamespace, "vector-config-namespace", "default",
		"The namespace in the downstream cluster to create the vector config secret in.")
	opts := zap.Options{
		Development: true,
	}
	flag.StringVar(&serverConfigFile, "server-config", "", "path to the server config file")

	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	setupLog.Info("starting telemetry-services-operator",
		"version", version,
		"gitCommit", gitCommit,
		"gitTreeState", gitTreeState,
		"buildDate", buildDate,
	)

	if len(serverConfigFile) == 0 {
		setupLog.Error(fmt.Errorf("must provide --server-config"), "")
		os.Exit(1)
	}

	var serverConfig config.TelemetryServicesOperator
	data, err := os.ReadFile(serverConfigFile)
	if err != nil {
		setupLog.Error(fmt.Errorf("unable to read server config from %q", serverConfigFile), "")
		os.Exit(1)
	}

	if err := runtime.DecodeInto(codecs.UniversalDecoder(), data, &serverConfig); err != nil {
		setupLog.Error(err, "unable to decode server config")
		os.Exit(1)
	}

	setupLog.Info("server config", "config", serverConfig)

	// if the enable-http2 flag is false (the default), http/2 should be disabled
	// due to its vulnerabilities. More specifically, disabling http/2 will
	// prevent from being vulnerable to the HTTP/2 Stream Cancellation and
	// Rapid Reset CVEs. For more information see:
	// - https://github.com/advisories/GHSA-qppj-fm5r-hxr3
	// - https://github.com/advisories/GHSA-4374-p667-p6c8
	disableHTTP2 := func(c *tls.Config) {
		setupLog.Info("disabling http/2")
		c.NextProtos = []string{"http/1.1"}
	}

	if !enableHTTP2 {
		tlsOpts = append(tlsOpts, disableHTTP2)
	}

	// Create watchers for metrics and webhooks certificates
	var metricsCertWatcher, webhookCertWatcher *certwatcher.CertWatcher

	// Initial webhook TLS options
	webhookTLSOpts := tlsOpts

	if len(webhookCertPath) > 0 {
		setupLog.Info("Initializing webhook certificate watcher using provided certificates",
			"webhook-cert-path", webhookCertPath, "webhook-cert-name", webhookCertName, "webhook-cert-key", webhookCertKey)

		var err error
		webhookCertWatcher, err = certwatcher.New(
			filepath.Join(webhookCertPath, webhookCertName),
			filepath.Join(webhookCertPath, webhookCertKey),
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

	// Metrics endpoint is enabled in 'config/default/kustomization.yaml'. The Metrics options configure the server.
	// More info:
	// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.20.2/pkg/metrics/server
	// - https://book.kubebuilder.io/reference/metrics.html
	metricsServerOptions := metricsserver.Options{
		BindAddress:   metricsAddr,
		SecureServing: secureMetrics,
		TLSOpts:       tlsOpts,
	}

	if secureMetrics {
		// FilterProvider is used to protect the metrics endpoint with authn/authz.
		// These configurations ensure that only authorized users and service accounts
		// can access the metrics endpoint. The RBAC are configured in 'config/rbac/kustomization.yaml'. More info:
		// https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.20.2/pkg/metrics/filters#WithAuthenticationAndAuthorization
		metricsServerOptions.FilterProvider = filters.WithAuthenticationAndAuthorization
	}

	// If the certificate is not specified, controller-runtime will automatically
	// generate self-signed certificates for the metrics server. While convenient for development and testing,
	// this setup is not recommended for production.
	//
	// TODO(user): If you enable certManager, uncomment the following lines:
	// - [METRICS-WITH-CERTS] at config/default/kustomization.yaml to generate and use certificates
	// managed by cert-manager for the metrics server.
	// - [PROMETHEUS-WITH-CERTS] at config/prometheus/kustomization.yaml for TLS certification.
	if len(metricsCertPath) > 0 {
		setupLog.Info("Initializing metrics certificate watcher using provided certificates",
			"metrics-cert-path", metricsCertPath, "metrics-cert-name", metricsCertName, "metrics-cert-key", metricsCertKey)

		var err error
		metricsCertWatcher, err = certwatcher.New(
			filepath.Join(metricsCertPath, metricsCertName),
			filepath.Join(metricsCertPath, metricsCertKey),
		)
		if err != nil {
			setupLog.Error(err, "to initialize metrics certificate watcher", "error", err)
			os.Exit(1)
		}

		metricsServerOptions.TLSOpts = append(metricsServerOptions.TLSOpts, func(config *tls.Config) {
			config.GetCertificate = metricsCertWatcher.GetCertificate
		})
	}

	upstreamClusterConfig, err := clientcmd.BuildConfigFromFlags("", upstreamClusterKubeconfig)
	if err != nil {
		setupLog.Error(err, "unable to load control plane kubeconfig")
		os.Exit(1)
	}

	// Retrieve the configuration for the cluster that the operator is deployed
	// in. This is the cluster that will have the vector config secret created
	// for telemetry services.
	downstreamClusterConfig := ctrl.GetConfigOrDie()

	downstreamCluster, err := cluster.New(downstreamClusterConfig, func(o *cluster.Options) {
		o.Scheme = scheme
	})
	if err != nil {
		setupLog.Error(err, "failed to construct downstream luster")
		os.Exit(1)
	}

	runnables, provider, err := initializeClusterDiscovery(serverConfig, downstreamCluster, scheme)
	if err != nil {
		setupLog.Error(err, "unable to initialize cluster discovery")
		os.Exit(1)
	}

	mgr, err := mcmanager.New(upstreamClusterConfig, provider, ctrl.Options{
		Scheme:                        scheme,
		Metrics:                       metricsServerOptions,
		WebhookServer:                 webhookServer,
		HealthProbeBindAddress:        probeAddr,
		LeaderElection:                enableLeaderElection,
		LeaderElectionID:              "telemetry.miloapis.com",
		LeaderElectionConfig:          leaderElectionRestConfig(upstreamClusterConfig),
		LeaderElectionReleaseOnCancel: true,
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	if err = (&controller.ExportPolicyReconciler{
		DownstreamClient:                downstreamCluster.GetClient(),
		DownstreamVectorConfigNamespace: vectorConfigurationNamespace,
		MetricsService: controller.MetricsService{
			Endpoint: os.Getenv("TELEMETRY_SERVICE_METRICS_ENDPOINT"),
			Username: os.Getenv("TELEMETRY_SERVICE_METRICS_USERNAME"),
			Password: os.Getenv("TELEMETRY_SERVICE_METRICS_PASSWORD"),
		},
		VectorConfigLabelKey:   vectorConfigLabelKey,
		VectorConfigLabelValue: vectorConfigLabelValue,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "ExportPolicy")
		os.Exit(1)
	}
	// nolint:goconst
	// if os.Getenv("ENABLE_WEBHOOKS") != "false" {
	// 	TODO: Re-enable webhooks once the webhook server is properly configured
	// 	      to support multicluster.

	// 	if err = webhooktelemetryv1alpha1.SetupExportPolicyWebhookWithManager(mgr); err != nil {
	// 		setupLog.Error(err, "unable to create webhook", "webhook", "ExportPolicy")
	// 		os.Exit(1)
	// 	}
	// }
	// +kubebuilder:scaffold:builder

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	ctx := ctrl.SetupSignalHandler()

	workers := make([]func(context.Context) error, 0, len(runnables)+1)
	for _, runnable := range runnables {
		workers = append(workers, runnable.Start)
	}
	workers = append(workers, func(ctx context.Context) error {
		return provider.Run(ctx, mgr)
	})

	setupLog.Info("starting cluster discovery provider and multicluster manager")
	if err := runUntilManagerStops(ctx, mgr.Start, workers...); err != nil {
		setupLog.Error(err, "unable to start")
		os.Exit(1)
	}
}

func runUntilManagerStops(
	ctx context.Context,
	startManager func(context.Context) error,
	workers ...func(context.Context) error,
) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	g, ctx := errgroup.WithContext(ctx)

	for _, work := range workers {
		g.Go(func() error {
			return ignoreCanceled(work(ctx))
		})
	}

	g.Go(func() error {
		defer cancel()
		return ignoreCanceled(startManager(ctx))
	})

	return g.Wait()
}

type runnableProvider interface {
	multicluster.Provider
	Run(context.Context, mcmanager.Manager) error
}

// Needed until we contribute the patch in the following PR again (need to sign CLA):
//
//	See: https://github.com/kubernetes-sigs/multicluster-runtime/pull/18
type wrappedSingleClusterProvider struct {
	multicluster.Provider
	cluster cluster.Cluster
}

func (p *wrappedSingleClusterProvider) Run(ctx context.Context, mgr mcmanager.Manager) error {
	if err := mgr.Engage(ctx, "single", p.cluster); err != nil {
		return err
	}
	return p.Provider.(runnableProvider).Run(ctx, mgr)
}

func initializeClusterDiscovery(
	serverConfig config.TelemetryServicesOperator,
	deploymentCluster cluster.Cluster,
	scheme *runtime.Scheme,
) (runnables []manager.Runnable, provider runnableProvider, err error) {
	runnables = append(runnables, deploymentCluster)
	switch serverConfig.Discovery.Mode {
	case milomulticluster.ProviderSingle:
		provider = &wrappedSingleClusterProvider{
			Provider: mcsingle.New("single", deploymentCluster),
			cluster:  deploymentCluster,
		}

	case milomulticluster.ProviderMilo:
		discoveryRestConfig, err := serverConfig.Discovery.DiscoveryRestConfig()
		if err != nil {
			return nil, nil, fmt.Errorf("unable to get discovery rest config: %w", err)
		}

		projectRestConfig, err := serverConfig.Discovery.ProjectRestConfig()
		if err != nil {
			return nil, nil, fmt.Errorf("unable to get project rest config: %w", err)
		}

		discoveryManager, err := manager.New(discoveryRestConfig, manager.Options{
			Client: client.Options{
				Cache: &client.CacheOptions{
					Unstructured: true,
				},
			},
		})
		if err != nil {
			return nil, nil, fmt.Errorf("unable to set up overall controller manager: %w", err)
		}

		provider, err = miloprovider.New(discoveryManager, miloprovider.Options{
			ClusterOptions: []cluster.Option{
				func(o *cluster.Options) {
					o.Scheme = scheme
				},
			},
			InternalServiceDiscovery: serverConfig.Discovery.InternalServiceDiscovery,
			ProjectRestConfig:        projectRestConfig,
		})
		if err != nil {
			return nil, nil, fmt.Errorf("unable to create datum project provider: %w", err)
		}

		runnables = append(runnables, discoveryManager)

	// case providers.ProviderKind:
	// 	provider = mckind.New(mckind.Options{
	// 		ClusterOptions: []cluster.Option{
	// 			func(o *cluster.Options) {
	// 				o.Scheme = scheme
	// 			},
	// 		},
	// 	})

	default:
		return nil, nil, fmt.Errorf(
			"unsupported cluster discovery mode %s",
			serverConfig.Discovery.Mode,
		)
	}

	return runnables, provider, nil
}

func ignoreCanceled(err error) error {
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}
