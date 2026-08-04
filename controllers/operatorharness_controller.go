package controllers

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

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
	rbacv1ac "k8s.io/client-go/applyconfigurations/rbac/v1"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	chrysopoeiav1 "github.com/helmetica-framework/chrysopoeia/api/v1"
	"github.com/helmetica-framework/chrysopoeia/proxy"
)

const (
	// OperatorHarnessLabel names the OperatorHarness an object was created for.
	OperatorHarnessLabel = "chrysopoeia.io/operator-harness"

	// operatorHarnessInstanceNamespaceLabel names the instance namespace an OperatorHarness was
	// created for. A cluster-scoped harness cannot be owned by the namespaced instance, so this is
	// what ties the two together.
	operatorHarnessInstanceNamespaceLabel = "chrysopoeia.io/instance-namespace"

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
//+kubebuilder:rbac:groups="",resources=serviceaccounts,verbs=get;list;watch;patch;update
//+kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=clusterroles;clusterrolebindings,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=clusterroles,verbs=bind;escalate

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

	// The scope of the operator is authorized by the proxy against the service account's annotation
	// and the scopedlist permission, whether or not the proxy configuration is injected here.
	if err := r.ensureServiceAccountScope(ctx, harness); err != nil {
		l.Error(err, "Failed to annotate the harnessed service accounts")
		statusCondition.Reason = "ServiceAccountScopeFailed"
		statusCondition.Message = err.Error()
		return ctrl.Result{}, err
	}

	if err := r.ensureScopedListRBAC(ctx, harness); err != nil {
		l.Error(err, "Failed to grant the scopedlist permission")
		statusCondition.Reason = "ScopedListRBACFailed"
		statusCondition.Message = err.Error()
		return ctrl.Result{}, err
	}

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
		// The operator's chart creates its service accounts after the harness exists, and they need
		// the scope annotation and the scopedlist permission as they appear.
		Watches(&corev1.ServiceAccount{}, handler.EnqueueRequestsFromMapFunc(r.harnessesForServiceAccount)).
		Named(name).
		Complete(r)
}

func (r *OperatorHarnessManager) harnessesForServiceAccount(ctx context.Context, obj client.Object) []reconcile.Request {
	var harnesses chrysopoeiav1.OperatorHarnessList
	if err := r.List(ctx, &harnesses); err != nil {
		log.FromContext(ctx).Error(err, "unable to list OperatorHarnesses for a ServiceAccount")
		return nil
	}

	var requests []reconcile.Request
	for _, harness := range harnesses.Items {
		if harnessesServiceAccount(harness, obj.GetNamespace(), obj.GetName()) {
			requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&harness)})
		}
	}
	return requests
}

// harnessedServiceAccounts returns the existing service accounts the harness applies to. If the
// harness names none, every service account in the operator's namespace is harnessed.
//
// Only existing service accounts are returned: the operator's chart creates them, and a service
// account created here first would collide with the chart's ownership of it.
func (r *OperatorHarnessManager) harnessedServiceAccounts(ctx context.Context, harness chrysopoeiav1.OperatorHarness) ([]corev1.ServiceAccount, error) {
	var serviceAccounts corev1.ServiceAccountList
	if err := r.List(ctx, &serviceAccounts, client.InNamespace(harness.Spec.Operator.Namespace)); err != nil {
		return nil, fmt.Errorf("unable to list ServiceAccounts in %s: %w", harness.Spec.Operator.Namespace, err)
	}

	harnessed := make([]corev1.ServiceAccount, 0, len(serviceAccounts.Items))
	for _, sa := range serviceAccounts.Items {
		if harnessesServiceAccount(harness, sa.Namespace, sa.Name) {
			harnessed = append(harnessed, sa)
		}
	}
	return harnessed, nil
}

// harnessesServiceAccount reports whether the harness applies to a service account. A harness that
// names no service account applies to every one in the operator's namespace.
func harnessesServiceAccount(harness chrysopoeiav1.OperatorHarness, namespace, name string) bool {
	if harness.Spec.Operator.Namespace != namespace {
		return false
	}
	return len(harness.Spec.Operator.ServiceAccounts) == 0 ||
		slices.Contains(harness.Spec.Operator.ServiceAccounts, name)
}

// ensureServiceAccountScope annotates the harnessed service accounts with the scope label. The proxy
// reads the annotation to decide which label it scopes a cluster-scoped list or watch to.
func (r *OperatorHarnessManager) ensureServiceAccountScope(ctx context.Context, harness chrysopoeiav1.OperatorHarness) error {
	serviceAccounts, err := r.harnessedServiceAccounts(ctx, harness)
	if err != nil {
		return err
	}

	var errs []error
	for _, sa := range serviceAccounts {
		if sa.Annotations[proxy.ScopeAnnotation] == harness.Spec.ScopeToLabel {
			continue
		}

		// Patched instead of applied so that a service account deleted in the meantime is not
		// recreated as an empty stub.
		patch := client.MergeFrom(sa.DeepCopy())
		if sa.Annotations == nil {
			sa.Annotations = map[string]string{}
		}
		sa.Annotations[proxy.ScopeAnnotation] = harness.Spec.ScopeToLabel

		if err := r.Patch(ctx, &sa, patch, client.FieldOwner(operatorHarnessManagerName)); err != nil && !apierrors.IsNotFound(err) {
			errs = append(errs, fmt.Errorf("unable to annotate ServiceAccount %s/%s: %w", sa.Namespace, sa.Name, err))
		}
	}
	return errors.Join(errs...)
}

// ensureScopedListRBAC grants the harnessed service accounts the custom scopedlist verb for the scope
// label. The proxy authorizes every cluster-scoped list or watch it rewrites against it, with the
// label as the resource name, so this is what bounds the operator to its own scope.
func (r *OperatorHarnessManager) ensureScopedListRBAC(ctx context.Context, harness chrysopoeiav1.OperatorHarness) error {
	serviceAccounts, err := r.harnessedServiceAccounts(ctx, harness)
	if err != nil {
		return err
	}

	roleName := scopedListRoleName(harness)

	clusterRole := rbacv1ac.
		ClusterRole(roleName).
		WithLabels(harnessLabels(harness)).
		WithOwnerReferences(harnessOwnerReference(harness)).
		WithRules(
			rbacv1ac.PolicyRule().
				WithAPIGroups("*").
				WithResources("*").
				WithVerbs(proxy.ScopedListVerb).
				WithResourceNames(harness.Spec.ScopeToLabel),
		)

	subjects := make([]*rbacv1ac.SubjectApplyConfiguration, 0, len(serviceAccounts))
	for _, sa := range serviceAccounts {
		subjects = append(subjects,
			rbacv1ac.Subject().
				WithKind("ServiceAccount").
				WithName(sa.Name).
				WithNamespace(sa.Namespace),
		)
	}

	clusterRoleBinding := rbacv1ac.
		ClusterRoleBinding(roleName).
		WithLabels(harnessLabels(harness)).
		WithOwnerReferences(harnessOwnerReference(harness)).
		WithRoleRef(
			rbacv1ac.RoleRef().
				WithAPIGroup("rbac.authorization.k8s.io").
				WithKind("ClusterRole").
				WithName(roleName),
		).
		WithSubjects(subjects...)

	return errors.Join(
		r.Apply(ctx, clusterRole, client.ForceOwnership, client.FieldOwner(operatorHarnessManagerName)),
		r.Apply(ctx, clusterRoleBinding, client.ForceOwnership, client.FieldOwner(operatorHarnessManagerName)),
	)
}

func scopedListRoleName(harness chrysopoeiav1.OperatorHarness) string {
	return strings.Join([]string{"chrysopoeia", "harness", "scopedlist", harness.Name}, ":")
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
