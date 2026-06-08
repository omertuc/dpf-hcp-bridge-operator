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
	"crypto/tls"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/certwatcher"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	dpuservicev1alpha1 "github.com/nvidia/doca-platform/api/dpuservice/v1alpha1"
	operatorv1alpha1 "github.com/nvidia/doca-platform/api/operator/v1alpha1"
	dpuprovisioningv1alpha1 "github.com/nvidia/doca-platform/api/provisioning/v1alpha1"
	configv1 "github.com/openshift/api/config/v1"
	hyperv1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
	provisioningv1alpha1 "github.com/rh-ecosystem-edge/dpf-hcp-provisioner-operator/api/v1alpha1"
	"github.com/rh-ecosystem-edge/dpf-hcp-provisioner-operator/cmd/dpuworkerconfigure"
	"github.com/rh-ecosystem-edge/dpf-hcp-provisioner-operator/internal/common"
	"github.com/rh-ecosystem-edge/dpf-hcp-provisioner-operator/internal/controller"
	"github.com/rh-ecosystem-edge/dpf-hcp-provisioner-operator/internal/controller/bfocplookup"
	"github.com/rh-ecosystem-edge/dpf-hcp-provisioner-operator/internal/controller/csrapproval"
	"github.com/rh-ecosystem-edge/dpf-hcp-provisioner-operator/internal/controller/dpucluster"
	"github.com/rh-ecosystem-edge/dpf-hcp-provisioner-operator/internal/controller/finalizer"
	"github.com/rh-ecosystem-edge/dpf-hcp-provisioner-operator/internal/controller/hostedcluster"
	"github.com/rh-ecosystem-edge/dpf-hcp-provisioner-operator/internal/controller/ignitiongenerator"
	"github.com/rh-ecosystem-edge/dpf-hcp-provisioner-operator/internal/controller/kubeconfiginjection"
	"github.com/rh-ecosystem-edge/dpf-hcp-provisioner-operator/internal/controller/metallb"
	"github.com/rh-ecosystem-edge/dpf-hcp-provisioner-operator/internal/controller/secrets"
	metallbv1beta1 "go.universe.tf/metallb/api/v1beta1"
	// +kubebuilder:scaffold:imports
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))

	utilruntime.Must(provisioningv1alpha1.AddToScheme(scheme))
	utilruntime.Must(dpuprovisioningv1alpha1.AddToScheme(scheme))
	utilruntime.Must(dpuservicev1alpha1.AddToScheme(scheme))
	utilruntime.Must(operatorv1alpha1.AddToScheme(scheme))
	utilruntime.Must(hyperv1.AddToScheme(scheme))
	utilruntime.Must(metallbv1beta1.AddToScheme(scheme))
	utilruntime.Must(configv1.Install(scheme))
	// +kubebuilder:scaffold:scheme
}

func main() {
	rootCmd := newRootCommand()
	rootCmd.AddCommand(dpuworkerconfigure.NewCommand())

	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

// nolint:gocyclo
func newRootCommand() *cobra.Command {
	var metricsAddr string
	var metricsCertPath, metricsCertName, metricsCertKey string
	var webhookCertPath, webhookCertName, webhookCertKey string
	var enableLeaderElection bool
	var probeAddr string
	var secureMetrics bool
	var enableHTTP2 bool

	zapOpts := zap.Options{Development: true}

	cmd := &cobra.Command{
		Use:   "manager",
		Short: "DPF HCP Provisioner Operator",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runManager(
				&zapOpts,
				metricsAddr, probeAddr,
				metricsCertPath, metricsCertName, metricsCertKey,
				webhookCertPath, webhookCertName, webhookCertKey,
				enableLeaderElection, secureMetrics, enableHTTP2,
			)
		},
	}

	flags := cmd.Flags()
	flags.StringVar(&metricsAddr, "metrics-bind-address", "0", "The address the metrics endpoint binds to. "+
		"Use :8443 for HTTPS or :8080 for HTTP, or leave as 0 to disable the metrics service.")
	flags.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flags.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	flags.BoolVar(&secureMetrics, "metrics-secure", true,
		"If set, the metrics endpoint is served securely via HTTPS. Use --metrics-secure=false to use HTTP instead.")
	flags.StringVar(&webhookCertPath, "webhook-cert-path", "", "The directory that contains the webhook certificate.")
	flags.StringVar(&webhookCertName, "webhook-cert-name", "tls.crt", "The name of the webhook certificate file.")
	flags.StringVar(&webhookCertKey, "webhook-cert-key", "tls.key", "The name of the webhook key file.")
	flags.StringVar(&metricsCertPath, "metrics-cert-path", "",
		"The directory that contains the metrics server certificate.")
	flags.StringVar(&metricsCertName, "metrics-cert-name", "tls.crt", "The name of the metrics server certificate file.")
	flags.StringVar(&metricsCertKey, "metrics-cert-key", "tls.key", "The name of the metrics server key file.")
	flags.BoolVar(&enableHTTP2, "enable-http2", false,
		"If set, HTTP/2 will be enabled for the metrics and webhook servers")

	zapOpts.BindFlags(flag.CommandLine)
	flags.AddGoFlagSet(flag.CommandLine)

	return cmd
}

func runManager(
	zapOpts *zap.Options,
	metricsAddr, probeAddr string,
	metricsCertPath, metricsCertName, metricsCertKey string,
	webhookCertPath, webhookCertName, webhookCertKey string,
	enableLeaderElection, secureMetrics, enableHTTP2 bool,
) error {
	var tlsOpts []func(*tls.Config)

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(zapOpts)))

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
			return fmt.Errorf("initializing webhook certificate watcher: %w", err)
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
	// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.21.0/pkg/metrics/server
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
		// https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.21.0/pkg/metrics/filters#WithAuthenticationAndAuthorization
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
			return fmt.Errorf("initializing metrics certificate watcher: %w", err)
		}

		metricsServerOptions.TLSOpts = append(metricsServerOptions.TLSOpts, func(config *tls.Config) {
			config.GetCertificate = metricsCertWatcher.GetCertificate
		})
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsServerOptions,
		WebhookServer:          webhookServer,
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "4ebdb3db.dpu.hcp.io",
		Cache: cache.Options{
			ByObject: map[crclient.Object]cache.ByObject{
				&corev1.ConfigMap{}: {
					Label: labels.SelectorFromSet(labels.Set{
						ignitiongenerator.BfcfgTemplateLabel: "true",
					}),
				},
			},
		},
		// LeaderElectionReleaseOnCancel defines if the leader should step down voluntarily
		// when the Manager ends. This requires the binary to immediately end when the
		// Manager is stopped, otherwise, this setting is unsafe. Setting this significantly
		// speeds up voluntary leader transitions as the new leader don't have to wait
		// LeaseDuration time first.
		//
		// In the default scaffold provided, the program ends immediately after
		// the manager stops, so would be fine to enable this option. However,
		// if you are doing or is intended to do any operation such as perform cleanups
		// after the manager stops then its usage might be unsafe.
		// LeaderElectionReleaseOnCancel: true,
	})
	if err != nil {
		return fmt.Errorf("starting manager: %w", err)
	}

	client := mgr.GetClient()
	scheme := mgr.GetScheme()

	// Create event recorders for each controller/component
	provisionerRecorder := mgr.GetEventRecorderFor(common.ProvisionerControllerName)
	csrApprovalRecorder := mgr.GetEventRecorderFor(common.CSRApprovalControllerName)

	// Initialize BlueField OCP Layer Image Lookup
	imageLookup := bfocplookup.NewImageLookup(client, provisionerRecorder)

	// Initialize DPUCluster Validator
	dpuClusterValidator := dpucluster.NewValidator(client, provisionerRecorder)

	// Initialize Secrets Validator
	secretsValidator := secrets.NewValidator(client, provisionerRecorder)

	// Initialize Secret Manager for HostedCluster lifecycle
	secretManager := hostedcluster.NewSecretManager(client, scheme)

	// Initialize HostedCluster Manager
	hostedClusterManager := hostedcluster.NewHostedClusterManager(client, scheme)

	// Initialize NodePool Manager
	nodePoolManager := hostedcluster.NewNodePoolManager(client, scheme)

	// Initialize Kubeconfig Injector
	kubeconfigInjector := kubeconfiginjection.NewKubeconfigInjector(client, provisionerRecorder)

	// Initialize MetalLB Manager
	metalLBManager := metallb.NewMetalLBManager(client, provisionerRecorder)

	// Initialize CSR Approver
	csrApprover := csrapproval.NewCSRApprover(client, csrApprovalRecorder)

	// Initialize Finalizer Manager with pluggable cleanup handlers
	// Handlers are executed in registration order
	finalizerManager := finalizer.NewManager(client, provisionerRecorder)

	// Register cleanup handlers in order (dependent resources first, dependencies last)
	// 1. Kubeconfig injection cleanup (removes kubeconfig from DPUCluster namespace)
	finalizerManager.RegisterHandler(kubeconfiginjection.NewCleanupHandler(client, provisionerRecorder))
	// 2. HostedCluster cleanup (removes HostedCluster, NodePool, services, and secrets)
	//    Must run before MetalLB cleanup because LoadBalancer services depend on IPAddressPool
	finalizerManager.RegisterHandler(hostedcluster.NewCleanupHandler(client, provisionerRecorder))
	// 3. MetalLB cleanup (removes IPAddressPool and L2Advertisement)
	//    Must run after HostedCluster cleanup to avoid deleting IPs while services still exist
	finalizerManager.RegisterHandler(metallb.NewCleanupHandler(client, provisionerRecorder))

	// Initialize Status Syncer for HostedCluster status mirroring
	statusSyncer := hostedcluster.NewStatusSyncer(client)

	// Initialize Ignition Generator for DPF provisioning
	ignitionGenerator := ignitiongenerator.NewIgnitionGenerator(client, scheme, provisionerRecorder)

	// Setup main DPFHCPProvisioner controller
	if err := (&controller.DPFHCPProvisionerReconciler{
		Client:               client,
		Scheme:               scheme,
		Recorder:             provisionerRecorder,
		ImageLookup:          imageLookup,
		DPUClusterValidator:  dpuClusterValidator,
		SecretsValidator:     secretsValidator,
		SecretManager:        secretManager,
		MetalLBManager:       metalLBManager,
		HostedClusterManager: hostedClusterManager,
		NodePoolManager:      nodePoolManager,
		FinalizerManager:     finalizerManager,
		StatusSyncer:         statusSyncer,
		KubeconfigInjector:   kubeconfigInjector,
		IgnitionGenerator:    ignitionGenerator,
	}).SetupWithManager(mgr); err != nil {
		return fmt.Errorf("setting up DPFHCPProvisioner controller: %w", err)
	}

	// Setup CSR Approval controller (separate from main controller)
	if err := (&csrapproval.CSRApprovalReconciler{
		Client:   client,
		Scheme:   scheme,
		Approver: csrApprover,
	}).SetupWithManager(mgr); err != nil {
		return fmt.Errorf("setting up CSRApproval controller: %w", err)
	}
	// +kubebuilder:scaffold:builder

	if metricsCertWatcher != nil {
		setupLog.Info("Adding metrics certificate watcher to manager")
		if err := mgr.Add(metricsCertWatcher); err != nil {
			return fmt.Errorf("adding metrics certificate watcher: %w", err)
		}
	}

	if webhookCertWatcher != nil {
		setupLog.Info("Adding webhook certificate watcher to manager")
		if err := mgr.Add(webhookCertWatcher); err != nil {
			return fmt.Errorf("adding webhook certificate watcher: %w", err)
		}
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		return fmt.Errorf("setting up health check: %w", err)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		return fmt.Errorf("setting up ready check: %w", err)
	}

	setupLog.Info("starting manager")
	return mgr.Start(ctrl.SetupSignalHandler())
}
