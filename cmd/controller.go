package cmd

import (
	"crypto/tls"
	"fmt"
	"os"
	"path/filepath"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	helmv2 "github.com/fluxcd/helm-controller/api/v2"
	imagereflectorv1 "github.com/fluxcd/image-reflector-controller/api/v1"
	sourcev1 "github.com/fluxcd/source-controller/api/v1"
	"github.com/spf13/cobra"
	"go.uber.org/multierr"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/certwatcher"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	chrysopoeiav1 "github.com/helmetica-framework/chrysopoeia/api/v1"
	"github.com/helmetica-framework/chrysopoeia/controllers"
	//+kubebuilder:scaffold:imports
)

var metricsAddr string
var enableLeaderElection bool
var probeAddr string
var sourceControllerHostnameOverride, imageReflectorControllerHostname string

func init() {
	RootCmd.AddCommand(controllerCmd)

	controllerCmd.Flags().StringVar(&metricsAddr, "metrics-bind-address", ":8080", "The address the metric endpoint binds to.")
	controllerCmd.Flags().StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	controllerCmd.Flags().BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")

	defaultNamespace := "default"
	if ns := os.Getenv("POD_NAMESPACE"); ns != "" {
		defaultNamespace = ns
	}
	controllerCmd.Flags().String("controller-namespace", defaultNamespace, "The namespace the controller runs in.")

	controllerCmd.Flags().String("webhook-cert-path", "", "The directory that contains the webhook certificate.")
	controllerCmd.Flags().String("webhook-cert-name", "tls.crt", "The name of the webhook certificate file.")
	controllerCmd.Flags().String("webhook-cert-key", "tls.key", "The name of the webhook key file.")

	controllerCmd.Flags().Bool("metrics-secure", true, "If set, the metrics endpoint is served securely via HTTPS. Use --metrics-secure=false to use HTTP instead.")
	controllerCmd.Flags().String("metrics-cert-path", "", "The directory that contains the metrics server certificate.")
	controllerCmd.Flags().String("metrics-cert-name", "tls.crt", "The name of the metrics server certificate file.")
	controllerCmd.Flags().String("metrics-cert-key", "tls.key", "The name of the metrics server key file.")

	controllerCmd.Flags().String("webhook-service-name", "chrysopoeia-webhook-service", "The name of the service fronting this controller's webhook server. Used in the webhook configurations generated for OperatorHarnesses.")
	controllerCmd.Flags().String("webhook-service-namespace", "", "The namespace of the service fronting this controller's webhook server. Defaults to the controller namespace.")
	controllerCmd.Flags().Int32("webhook-service-port", 443, "The port of the service fronting this controller's webhook server.")
	controllerCmd.Flags().String("webhook-ca-name", "ca.crt", "The name of the CA certificate file in the webhook certificate directory. Used as the caBundle in the webhook configurations generated for OperatorHarnesses.")

	controllerCmd.Flags().String("harness-proxy-host", "chrysopoeia-proxy.chrysopoeia-proxy-system.svc", "The host harnessed operators are pointed at instead of the Kubernetes API server.")
	controllerCmd.Flags().String("harness-proxy-port", "8443", "The port harnessed operators are pointed at instead of the Kubernetes API server.")
	controllerCmd.Flags().String("harness-proxy-ca-secret-namespace", "chrysopoeia-proxy-system", "The namespace of the secret holding the harness proxy's serving certificate.")
	controllerCmd.Flags().String("harness-proxy-ca-secret-name", "proxy-serving-cert", "The name of the secret holding the harness proxy's serving certificate, as in the .spec.secretName of its Certificate. Its ca.crt is copied to the namespaces of harnessed operators.")

	controllerCmd.Flags().StringVar(&sourceControllerHostnameOverride, "source-controller-hostname-override", "", "If set, overrides the hostname used to access the source controller. Useful for testing against a local source controller.")
	controllerCmd.Flags().StringVar(&imageReflectorControllerHostname, "image-reflector-controller-hostname", "image-reflector-controller-tags.image-reflector-system.svc", "Sets the hostname used to access the image reflector controller to load tags for a OCI image.")
}

var controllerCmd = &cobra.Command{
	Use:   "controller",
	Short: "Starts the controller manager",
	Long:  "Starts the controller manager",
	RunE:  runController,
}

func newScheme() *runtime.Scheme {
	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(chrysopoeiav1.AddToScheme(scheme))
	utilruntime.Must(sourcev1.AddToScheme(scheme))
	utilruntime.Must(imagereflectorv1.AddToScheme(scheme))
	utilruntime.Must(apiextv1.AddToScheme(scheme))
	utilruntime.Must(helmv2.AddToScheme(scheme))
	//+kubebuilder:scaffold:scheme
	return scheme
}

func runController(cmd *cobra.Command, _ []string) error {
	controllerNamespace, cnerr := cmd.Flags().GetString("controller-namespace")

	webhookCertPath, wcperr := cmd.Flags().GetString("webhook-cert-path")
	webhookCertName, wcnerr := cmd.Flags().GetString("webhook-cert-name")
	webhookCertKey, wckerr := cmd.Flags().GetString("webhook-cert-key")

	secureMetrics, smerr := cmd.Flags().GetBool("metrics-secure")
	metricsCertPath, mcperr := cmd.Flags().GetString("metrics-cert-path")
	metricsCertName, mcnerr := cmd.Flags().GetString("metrics-cert-name")
	metricsCertKey, mckerr := cmd.Flags().GetString("metrics-cert-key")

	webhookServiceName, wsnerr := cmd.Flags().GetString("webhook-service-name")
	webhookServiceNamespace, wssnerr := cmd.Flags().GetString("webhook-service-namespace")
	webhookServicePort, wsperr := cmd.Flags().GetInt32("webhook-service-port")
	webhookCAName, wcaerr := cmd.Flags().GetString("webhook-ca-name")

	harnessProxyHost, hpherr := cmd.Flags().GetString("harness-proxy-host")
	harnessProxyPort, hpperr := cmd.Flags().GetString("harness-proxy-port")
	harnessProxyCANamespace, hpcnserr := cmd.Flags().GetString("harness-proxy-ca-secret-namespace")
	harnessProxyCAName, hpcnerr := cmd.Flags().GetString("harness-proxy-ca-secret-name")

	sourceControllerHostnameOverride, sherr := cmd.Flags().GetString("source-controller-hostname-override")
	imageReflectorControllerHostname, irherr := cmd.Flags().GetString("image-reflector-controller-hostname")

	if err := multierr.Combine(cnerr, wcperr, wcnerr, wckerr, mcperr, mcnerr, mckerr, smerr, sherr, irherr,
		wsnerr, wssnerr, wsperr, wcaerr, hpherr, hpperr, hpcnserr, hpcnerr); err != nil {
		return fmt.Errorf("failed to get flags: %w", err)
	}

	if webhookServiceNamespace == "" {
		webhookServiceNamespace = controllerNamespace
	}

	cmd.Println("Starting the controller manager",
		"controller-namespace", controllerNamespace,
	)

	scheme := newScheme()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&zapOpts)))

	var webhookCertWatcher *certwatcher.CertWatcher

	var webhookTLSOpts []func(*tls.Config)
	if len(webhookCertPath) > 0 {
		cmd.Println("Initializing webhook certificate watcher using provided certificates",
			"webhook-cert-path", webhookCertPath, "webhook-cert-name", webhookCertName, "webhook-cert-key", webhookCertKey)

		var err error
		webhookCertWatcher, err = certwatcher.New(
			filepath.Join(webhookCertPath, webhookCertName),
			filepath.Join(webhookCertPath, webhookCertKey),
		)
		if err != nil {
			return fmt.Errorf("failed to initialize webhook certificate watcher: %w", err)
		}

		webhookTLSOpts = append(webhookTLSOpts, func(config *tls.Config) {
			config.GetCertificate = webhookCertWatcher.GetCertificate
		})
	}

	webhookServer := webhook.NewServer(webhook.Options{
		TLSOpts: webhookTLSOpts,
	})

	var metricsCertWatcher *certwatcher.CertWatcher

	metricsServerOptions := metricsserver.Options{
		BindAddress:   metricsAddr,
		SecureServing: secureMetrics,
		TLSOpts:       []func(*tls.Config){},
	}

	if secureMetrics {
		// FilterProvider is used to protect the metrics endpoint with authn/authz.
		// These configurations ensure that only authorized users and service accounts
		// can access the metrics endpoint. The RBAC are configured in 'config/rbac/kustomization.yaml'. More info:
		// https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.20.4/pkg/metrics/filters#WithAuthenticationAndAuthorization
		metricsServerOptions.FilterProvider = filters.WithAuthenticationAndAuthorization
	}

	if len(metricsCertPath) > 0 {
		cmd.Println("Initializing metrics certificate watcher using provided certificates",
			"metrics-cert-path", metricsCertPath, "metrics-cert-name", metricsCertName, "metrics-cert-key", metricsCertKey)

		var err error
		metricsCertWatcher, err = certwatcher.New(
			filepath.Join(metricsCertPath, metricsCertName),
			filepath.Join(metricsCertPath, metricsCertKey),
		)
		if err != nil {
			cmd.Println("failed to initialize metrics certificate watcher", "error", err)
			os.Exit(1)
		}

		metricsServerOptions.TLSOpts = append(metricsServerOptions.TLSOpts, func(config *tls.Config) {
			config.GetCertificate = metricsCertWatcher.GetCertificate
		})
	}

	restConf := ctrl.GetConfigOrDie()
	mgr, err := ctrl.NewManager(restConf, ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsServerOptions,
		WebhookServer:          webhookServer,
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "4ede2161a2.chrysopoeia.helmetica.io",

		Cache: cache.Options{
			ByObject: map[client.Object]cache.ByObject{
				&chrysopoeiav1.CustomResourceDefinitionSource{}: {
					Namespaces: map[string]cache.Config{
						controllerNamespace: {},
					},
				},

				&apiextv1.CustomResourceDefinition{}: {
					Label: labels.SelectorFromSet(labels.Set{"chrysopoeia.io/managed": ""}),
				},
				&helmv2.HelmRelease{}: {
					Label: labels.SelectorFromSet(labels.Set{"chrysopoeia.io/managed": ""}),
				},
				&rbacv1.RoleBinding{}:    {},
				&corev1.ServiceAccount{}: {},

				// The harness only ever reads the proxy's serving certificate.
				&corev1.Secret{}: {
					Namespaces: map[string]cache.Config{
						harnessProxyCANamespace: {},
					},
				},
				&corev1.ConfigMap{}: {
					Label: labels.SelectorFromSet(labels.Set{"chrysopoeia.io/managed": ""}),
				},
				&admissionregistrationv1.MutatingWebhookConfiguration{}: {
					Label: labels.SelectorFromSet(labels.Set{"chrysopoeia.io/managed": ""}),
				},
			},
		},

		LeaderElectionReleaseOnCancel: true,
	})
	if err != nil {
		return fmt.Errorf("unable to start manager: %w", err)
	}

	lifetimeCtx := cmd.Context()

	if err := controllers.SetupInstanceRevisionOwnerFieldIndex(mgr); err != nil {
		return fmt.Errorf("unable to set up InstanceRevision owner field index: %w", err)
	}

	bsm := &controllers.CustomResourceDefinitionSourceManager{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Recorder: mgr.GetEventRecorder("customresourcedefinitionsource-controller"),

		SourceControllerHostnameOverride: sourceControllerHostnameOverride,
		ImageReflectorControllerHostname: imageReflectorControllerHostname,
	}
	if err := bsm.SetupWithManager("customresourcedefinitionsource", mgr, controllerNamespace); err != nil {
		return fmt.Errorf("unable to create CustomResourceDefinitionSource controller: %w", err)
	}

	dram := &controllers.DynamicCRDsRBACAggregatorManager{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Recorder: mgr.GetEventRecorder("rbac-aggregator-manager"),
	}
	if err := dram.SetupWithManager("rbac-aggregator-manager", mgr); err != nil {
		return fmt.Errorf("unable to create RBACAggregatorManager controller: %w", err)
	}

	drm := &controllers.DependencyRBACManager{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Recorder: mgr.GetEventRecorder("dependency-rbac-manager"),
	}
	if err := drm.SetupWithManager("dependency-rbac-manager", mgr); err != nil {
		return fmt.Errorf("unable to create DependencyRBACManager controller: %w", err)
	}

	imm := &controllers.DynamicReconcilerManager{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Recorder: mgr.GetEventRecorder("instance-manager-manager"),

		ControllerLifetimeCtx: lifetimeCtx,
		ManagedReconcilers: []func() controllers.DynamicReconciler{
			controllers.NewRevisionManager,
			controllers.NewAutomaticApprovalManager,
			controllers.NewReleaseController,
		},
	}
	if err := imm.SetupWithManager("instance-manager-manager", mgr); err != nil {
		return fmt.Errorf("unable to create RevisionManagerManager controller: %w", err)
	}

	// The OperatorHarness proxy injection needs a webhook server: it patches the harnessed
	// operator's pods and points the generated webhook configurations at this controller.
	if webhookCertPath == "" {
		cmd.Println("No webhook certificate configured, not starting the OperatorHarness controller")
	} else {
		proxyInjection := controllers.ProxyInjection{
			ServiceHost: harnessProxyHost,
			ServicePort: harnessProxyPort,
			CASecret: types.NamespacedName{
				Namespace: harnessProxyCANamespace,
				Name:      harnessProxyCAName,
			},
		}

		ohm := &controllers.OperatorHarnessManager{
			Client:   mgr.GetClient(),
			Scheme:   mgr.GetScheme(),
			Recorder: mgr.GetEventRecorder("operator-harness-manager"),

			Proxy: proxyInjection,
			WebhookService: controllers.WebhookService{
				Name:      webhookServiceName,
				Namespace: webhookServiceNamespace,
				Port:      webhookServicePort,
				CABundle: func() ([]byte, error) {
					return os.ReadFile(filepath.Join(webhookCertPath, webhookCAName))
				},
			},
		}
		if err := ohm.SetupWithManager("operator-harness-manager", mgr); err != nil {
			return fmt.Errorf("unable to create OperatorHarnessManager controller: %w", err)
		}

		mgr.GetWebhookServer().Register(controllers.ProxyInjectionWebhookPath, &webhook.Admission{
			Handler: &controllers.PodProxyInjector{
				Client:  mgr.GetClient(),
				Decoder: admission.NewDecoder(mgr.GetScheme()),

				Proxy: proxyInjection,
			},
		})
	}

	//+kubebuilder:scaffold:builder

	if metricsCertWatcher != nil {
		cmd.Println("Adding metrics certificate watcher to manager")
		if err := mgr.Add(metricsCertWatcher); err != nil {
			cmd.Println("unable to add metrics certificate watcher to manager", err)
			os.Exit(1)
		}
	}

	if webhookCertWatcher != nil {
		cmd.Println("Adding webhook certificate watcher to manager")
		if err := mgr.Add(webhookCertWatcher); err != nil {
			return fmt.Errorf("unable to add webhook certificate watcher to manager: %w", err)
		}
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		return fmt.Errorf("unable to set up health check: %w", err)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		return fmt.Errorf("unable to set up ready check: %w", err)
	}

	cmd.Println("Starting the controller manager")
	if err := mgr.Start(lifetimeCtx); err != nil {
		return fmt.Errorf("problem running manager: %w", err)
	}
	return nil
}
