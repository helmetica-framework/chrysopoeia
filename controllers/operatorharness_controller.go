package controllers

import (
	"context"
	"errors"
	"fmt"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	admissionregistrationv1ac "k8s.io/client-go/applyconfigurations/admissionregistration/v1"
	corev1ac "k8s.io/client-go/applyconfigurations/core/v1"
	metav1ac "k8s.io/client-go/applyconfigurations/meta/v1"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	chrysopoeiav1 "github.com/helmetica-framework/chrysopoeia/api/v1"
)

const (
	// OperatorHarnessLabel names the OperatorHarness an object was created for.
	OperatorHarnessLabel = "chrysopoeia.io/operator-harness"

	// ProxyCAConfigMapName is the name of the ConfigMap the harness proxy's CA certificate is copied
	// to in the harnessed operator's namespace.
	ProxyCAConfigMapName = "chrysopoeia-proxy-root-ca.crt"
	// ProxyCACertKey is the key the CA certificate is stored under, both in the proxy's serving
	// certificate secret and in the copy in the operator's namespace.
	ProxyCACertKey = "ca.crt"

	// proxyInjectionWebhookName is the name of the single webhook in every generated
	// MutatingWebhookConfiguration.
	proxyInjectionWebhookName = "proxy-injection.chrysopoeia.helmetica.io"

	operatorHarnessManagerName = "chrysopoeia-operator-harness-manager"
)

// ProxyInjection describes how a harnessed operator's pods reach the harness proxy.
type ProxyInjection struct {
	// ServiceHost is injected into the operator's containers as KUBERNETES_SERVICE_HOST.
	ServiceHost string
	// ServicePort is injected into the operator's containers as KUBERNETES_SERVICE_PORT.
	ServicePort string
	// CASecret is the secret holding the harness proxy's serving CA certificate under
	// [ProxyCACertKey]. It is copied to the harnessed operator's namespace.
	CASecret types.NamespacedName
}

// WebhookService locates this controller's own webhook server, so that the generated
// MutatingWebhookConfigurations can reach it.
type WebhookService struct {
	Name      string
	Namespace string
	Port      int32

	// CABundle returns the CA bundle clients must use to verify the webhook server. It is called on
	// every reconciliation so that certificate rotation is picked up.
	CABundle func() ([]byte, error)
}

// OperatorHarnessManager harnesses operators that would otherwise need cluster-wide permissions.
// For now it only sets up the proxy injection: it copies the harness proxy's CA certificate to the
// harnessed operator's namespace and registers a mutating webhook that points the operator's pods at
// the harness proxy instead of the Kubernetes API server.
type OperatorHarnessManager struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder events.EventRecorder

	Proxy          ProxyInjection
	WebhookService WebhookService
}

//+kubebuilder:rbac:groups=helmetica.io,resources=operatorharnesses,verbs=get;list;watch
//+kubebuilder:rbac:groups=helmetica.io,resources=operatorharnesses/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=admissionregistration.k8s.io,resources=mutatingwebhookconfigurations,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch

func (r *OperatorHarnessManager) Reconcile(ctx context.Context, req reconcile.Request) (res ctrl.Result, err error) {
	l := log.FromContext(ctx).WithName("OperatorHarnessManager.Reconcile")
	l.Info("Reconciling OperatorHarness")

	var harness chrysopoeiav1.OperatorHarness
	if err := r.Get(ctx, req.NamespacedName, &harness); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !harness.DeletionTimestamp.IsZero() {
		// Everything we create is owned by the harness, the garbage collector cleans it up.
		l.Info("OperatorHarness is being deleted, nothing to clean up")
		return ctrl.Result{}, nil
	}

	statusCondition := metav1.Condition{
		Type:               "Ready",
		Status:             metav1.ConditionFalse,
		Reason:             "UnknownError",
		Message:            "OperatorHarness is not ready due to an unknown error",
		ObservedGeneration: harness.Generation,
	}
	defer func() {
		if apimeta.SetStatusCondition(&harness.Status.Conditions, statusCondition) {
			if err := r.Status().Update(ctx, &harness); err != nil {
				l.Error(err, "Failed to update OperatorHarness status")
				res = ctrl.Result{}
			}
		}
	}()

	if !harness.Spec.Operator.InjectProxyConfiguration {
		if err := r.pruneProxyConfiguration(ctx, harness, ""); err != nil {
			l.Error(err, "Failed to remove proxy configuration")
			statusCondition.Reason = "ProxyConfigurationRemovalFailed"
			statusCondition.Message = err.Error()
			return ctrl.Result{}, err
		}

		statusCondition.Status = metav1.ConditionTrue
		statusCondition.Reason = "ProxyInjectionDisabled"
		statusCondition.Message = "Proxy configuration injection is disabled"
		return ctrl.Result{}, nil
	}

	if err := r.ensureProxyCACertificate(ctx, harness); err != nil {
		l.Error(err, "Failed to copy harness proxy CA certificate")
		statusCondition.Reason = "ProxyCACertificateFailed"
		statusCondition.Message = err.Error()
		return ctrl.Result{}, err
	}

	if err := r.ensureProxyInjectionWebhook(ctx, harness); err != nil {
		l.Error(err, "Failed to configure proxy injection webhook")
		statusCondition.Reason = "ProxyInjectionWebhookFailed"
		statusCondition.Message = err.Error()
		return ctrl.Result{}, err
	}

	// The operator namespace may have changed, leaving objects behind in the previous one.
	if err := r.pruneProxyConfiguration(ctx, harness, harness.Spec.Operator.Namespace); err != nil {
		l.Error(err, "Failed to prune stale proxy configuration")
		statusCondition.Reason = "ProxyConfigurationRemovalFailed"
		statusCondition.Message = err.Error()
		return ctrl.Result{}, err
	}

	statusCondition.Status = metav1.ConditionTrue
	statusCondition.Reason = "ProxyConfigurationInjected"
	statusCondition.Message = fmt.Sprintf("Pods of %v in namespace %s are pointed at the harness proxy",
		harness.Spec.Operator.ServiceAccounts, harness.Spec.Operator.Namespace)
	return ctrl.Result{}, nil
}

func (r *OperatorHarnessManager) SetupWithManager(name string, mgr ctrl.Manager) error {
	return builder.ControllerManagedBy(mgr).
		For(&chrysopoeiav1.OperatorHarness{}).
		Owns(&corev1.ConfigMap{}).
		Owns(&admissionregistrationv1.MutatingWebhookConfiguration{}).
		Named(name).
		Complete(r)
}

// ensureProxyCACertificate copies the harness proxy's CA certificate to the harnessed operator's
// namespace, so that the operator's pods can verify the proxy's serving certificate.
func (r *OperatorHarnessManager) ensureProxyCACertificate(ctx context.Context, harness chrysopoeiav1.OperatorHarness) error {
	var caSecret corev1.Secret
	if err := r.Get(ctx, r.Proxy.CASecret, &caSecret); err != nil {
		return fmt.Errorf("unable to get harness proxy CA secret %s: %w", r.Proxy.CASecret, err)
	}

	ca := caSecret.Data[ProxyCACertKey]
	if len(ca) == 0 {
		return fmt.Errorf("harness proxy CA secret %s has no %q", r.Proxy.CASecret, ProxyCACertKey)
	}

	cm := corev1ac.
		ConfigMap(ProxyCAConfigMapName, harness.Spec.Operator.Namespace).
		WithLabels(harnessLabels(harness)).
		WithOwnerReferences(harnessOwnerReference(harness)).
		WithData(map[string]string{ProxyCACertKey: string(ca)})

	return r.Apply(ctx, cm, client.ForceOwnership, client.FieldOwner(operatorHarnessManagerName))
}

// ensureProxyInjectionWebhook registers the mutating webhook that patches the harnessed operator's
// pods. The webhook is scoped to the operator's namespace, the handler additionally matches the
// pod's service account against the harness.
func (r *OperatorHarnessManager) ensureProxyInjectionWebhook(ctx context.Context, harness chrysopoeiav1.OperatorHarness) error {
	caBundle, err := r.WebhookService.CABundle()
	if err != nil {
		return fmt.Errorf("unable to load the webhook server's CA bundle: %w", err)
	}

	mwc := admissionregistrationv1ac.
		MutatingWebhookConfiguration(proxyInjectionWebhookConfigurationName(harness)).
		WithLabels(harnessLabels(harness)).
		WithOwnerReferences(harnessOwnerReference(harness)).
		WithWebhooks(
			admissionregistrationv1ac.MutatingWebhook().
				WithName(proxyInjectionWebhookName).
				WithAdmissionReviewVersions("v1").
				WithSideEffects(admissionregistrationv1.SideEffectClassNone).
				WithFailurePolicy(admissionregistrationv1.Fail).
				WithReinvocationPolicy(admissionregistrationv1.NeverReinvocationPolicy).
				WithMatchPolicy(admissionregistrationv1.Equivalent).
				WithNamespaceSelector(
					metav1ac.LabelSelector().
						WithMatchLabels(map[string]string{corev1.LabelMetadataName: harness.Spec.Operator.Namespace}),
				).
				WithRules(
					admissionregistrationv1ac.RuleWithOperations().
						WithOperations(admissionregistrationv1.Create).
						WithAPIGroups("").
						WithAPIVersions("v1").
						WithResources("pods").
						WithScope(admissionregistrationv1.NamespacedScope),
				).
				WithClientConfig(
					admissionregistrationv1ac.WebhookClientConfig().
						WithCABundle(caBundle...).
						WithService(
							admissionregistrationv1ac.ServiceReference().
								WithName(r.WebhookService.Name).
								WithNamespace(r.WebhookService.Namespace).
								WithPort(r.WebhookService.Port).
								WithPath(ProxyInjectionWebhookPath),
						),
				),
		)

	return r.Apply(ctx, mwc, client.ForceOwnership, client.FieldOwner(operatorHarnessManagerName))
}

// pruneProxyConfiguration deletes the proxy configuration created for the harness. Objects that are
// still wanted in keepNamespace are kept, pass an empty namespace to delete everything.
func (r *OperatorHarnessManager) pruneProxyConfiguration(ctx context.Context, harness chrysopoeiav1.OperatorHarness, keepNamespace string) error {
	sel := client.MatchingLabels{OperatorHarnessLabel: harness.Name}

	var configMaps corev1.ConfigMapList
	if err := r.List(ctx, &configMaps, sel); err != nil {
		return fmt.Errorf("unable to list ConfigMaps of the harness: %w", err)
	}

	var errs []error
	for _, cm := range configMaps.Items {
		if keepNamespace != "" && cm.Namespace == keepNamespace && cm.Name == ProxyCAConfigMapName {
			continue
		}
		if err := r.Delete(ctx, &cm); err != nil && !apierrors.IsNotFound(err) {
			errs = append(errs, fmt.Errorf("unable to delete ConfigMap %s/%s: %w", cm.Namespace, cm.Name, err))
		}
	}

	var webhookConfigs admissionregistrationv1.MutatingWebhookConfigurationList
	if err := r.List(ctx, &webhookConfigs, sel); err != nil {
		errs = append(errs, fmt.Errorf("unable to list MutatingWebhookConfigurations of the harness: %w", err))
		return errors.Join(errs...)
	}
	for _, mwc := range webhookConfigs.Items {
		if keepNamespace != "" && mwc.Name == proxyInjectionWebhookConfigurationName(harness) {
			continue
		}
		if err := r.Delete(ctx, &mwc); err != nil && !apierrors.IsNotFound(err) {
			errs = append(errs, fmt.Errorf("unable to delete MutatingWebhookConfiguration %s: %w", mwc.Name, err))
		}
	}

	return errors.Join(errs...)
}

func proxyInjectionWebhookConfigurationName(harness chrysopoeiav1.OperatorHarness) string {
	return "chrysopoeia-operator-harness-" + harness.Name
}

func harnessLabels(harness chrysopoeiav1.OperatorHarness) map[string]string {
	return map[string]string{
		"chrysopoeia.io/managed": "",
		OperatorHarnessLabel:     harness.Name,
	}
}

func harnessOwnerReference(harness chrysopoeiav1.OperatorHarness) *metav1ac.OwnerReferenceApplyConfiguration {
	return metav1ac.OwnerReference().
		WithAPIVersion(chrysopoeiav1.GroupVersion.String()).
		WithKind("OperatorHarness").
		WithName(harness.Name).
		WithUID(harness.UID).
		WithController(true)
}
