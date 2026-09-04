package controllers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"strings"
	"time"

	helmv2 "github.com/fluxcd/helm-controller/api/v2"
	sourcev1 "github.com/fluxcd/source-controller/api/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	corev1ac "k8s.io/client-go/applyconfigurations/core/v1"
	rbacv1ac "k8s.io/client-go/applyconfigurations/rbac/v1"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	chrysopoeiav1 "github.com/helmetica-framework/chrysopoeia/api/v1"
	chrysopoeiav1ac "github.com/helmetica-framework/chrysopoeia/api/v1/applyconfiguration/api/v1"
)

type ReleaseController struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder events.EventRecorder

	// GVK is the GroupVersionKind of the resource that this controller manages.
	GVK schema.GroupVersionKind
}

//+kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups="",resources=serviceaccounts,verbs=get;list;watch;create;update;patch
//+kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=clusterroles;roles;rolebindings;clusterrolebindings,verbs=get;list;watch;create;update;patch

//+kubebuilder:rbac:groups=helmetica.io,resources=instancerevisions,verbs=get;list;watch
//+kubebuilder:rbac:groups=helmetica.io,resources=dependencygroups,verbs=get;list;watch
//+kubebuilder:rbac:groups=helmetica.io,resources=operatorharnesses,verbs=get;list;watch;create;update;patch;delete;deletecollection

//+kubebuilder:rbac:groups=helm.toolkit.fluxcd.io,resources=helmreleases,verbs=get;list;watch;update;patch;create

func NewReleaseController() DynamicReconciler {
	return &ReleaseController{}
}

func (r *ReleaseController) Reconcile(ctx context.Context, req reconcile.Request) (res ctrl.Result, err error) {
	l := log.FromContext(ctx).WithName("ReleaseController.Reconcile").WithValues("request", req)
	l.Info("Reconciling Claim")

	instanceNSName := r.instanceNamespaceName(req.NamespacedName)

	var claim unstructured.Unstructured
	claim.SetAPIVersion(r.GVK.GroupVersion().String())
	claim.SetKind(r.GVK.Kind)
	if err := r.Get(ctx, req.NamespacedName, &claim); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, r.cleanupRelease(ctx, instanceNSName)
		}
		return ctrl.Result{}, err
	}
	if !claim.GetDeletionTimestamp().IsZero() {
		return ctrl.Result{}, nil
	}
	{
		var ns corev1.Namespace
		if err := r.Get(ctx, client.ObjectKey{Name: instanceNSName}, &ns); err != nil && !apierrors.IsNotFound(err) {
			return ctrl.Result{}, fmt.Errorf("unknown error retrieving namespace for deletion check %s: %w", instanceNSName, err)
		}
		if !ns.DeletionTimestamp.IsZero() {
			l.Info("Previous release is being deleted, skipping release until deletion is complete", "namespace", instanceNSName)
			return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
		}
	}

	var revisions chrysopoeiav1.InstanceRevisionList
	if err := r.List(ctx, &revisions, client.InNamespace(req.Namespace), client.MatchingFields{ownerUIDField: string(claim.GetUID())}); err != nil {
		return ctrl.Result{}, err
	}
	sortByApprovalNewestFirst(revisions.Items)

	if len(revisions.Items) == 0 || revisions.Items[0].Spec.ApprovedAt == nil {
		l.Info("No approved InstanceRevision found, skipping release")
		return ctrl.Result{}, nil
	}
	revision := revisions.Items[0]

	_, digest, found := strings.Cut(revision.Spec.Version, "@")
	if !found {
		return ctrl.Result{}, fmt.Errorf("invalid version format: %s", revision.Spec.Version)
	}

	if err := r.ensureRelease(ctx, claim, instanceNSName, digest, revision); err != nil {
		return ctrl.Result{}, err
	}

	var release helmv2.HelmRelease
	if err := r.Get(ctx, client.ObjectKey{Namespace: instanceNSName, Name: claim.GetName()}, &release); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
	if release.GetAnnotations()["chrysopoeia.io/revision-name"] != revision.GetName() {
		// Cache has not yet caught up
		return ctrl.Result{}, nil
	}

	status := "Unknown"
	if release.Generation > release.Status.ObservedGeneration {
		status = "Pending"
	} else {
		cond := apimeta.FindStatusCondition(release.Status.Conditions, "Ready")
		if cond != nil {
			if cond.Status == metav1.ConditionTrue {
				status = "Ready"
			} else {
				status = cond.Reason
			}
		}
	}

	drifted := apimeta.IsStatusConditionTrue(release.Status.Conditions, helmv2.DriftedCondition)

	statusPatch := &unstructured.Unstructured{}
	statusPatch.SetGroupVersionKind(claim.GroupVersionKind())
	statusPatch.SetName(claim.GetName())
	statusPatch.SetNamespace(claim.GetNamespace())
	if err := errors.Join(
		unstructured.SetNestedField(statusPatch.Object, status, "status", "releaseStatus"),
		unstructured.SetNestedField(statusPatch.Object, revision.GetName(), "status", "appliedRevision"),
		unstructured.SetNestedField(statusPatch.Object, drifted, "status", "driftDetected"),
		unstructured.SetNestedField(statusPatch.Object, instanceNSName, "status", "instanceNamespace"),
	); err != nil {
		return ctrl.Result{}, err
	}

	if err := r.Status().Apply(ctx, client.ApplyConfigurationFromUnstructured(statusPatch), client.FieldOwner("chrysopoeia:release-controller:status")); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

func (r *ReleaseController) instanceNamespaceName(nsn types.NamespacedName) string {
	gvkh := sha256.New()
	idh := sha256.New()

	// Fprint doesn't add any sperators between the operators if they are strings.
	// Which could lead to subtle collisons. Adding a space in between avoids
	// that since, a space is not valid in these fields.
	_, _ = fmt.Fprintf(gvkh, "%s %s %s", r.GVK.Group, r.GVK.Version, r.GVK.Kind)
	_, _ = fmt.Fprintf(idh, "%s %s %s %s %s", r.GVK.Group, r.GVK.Version, r.GVK.Kind, nsn.Namespace, nsn.Name)

	kind := strings.ToLower(r.GVK.Kind)
	kind = kind[:min(len(kind), 32)]
	idHex := hex.EncodeToString(idh.Sum(nil))[:min(32, 48-len(kind))]

	return fmt.Sprintf("helx-%s-%x-%s", kind, gvkh.Sum(nil)[:4], idHex)
}

func (r *ReleaseController) cleanupRelease(ctx context.Context, helmNSName string) error {
	log.FromContext(ctx).WithName("cleanupRelease").Info("Cleaning up release", "namespace", helmNSName)

	if err := r.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: helmNSName}}); err != nil && !apierrors.IsNotFound(err) {
		return err
	}

	// The harness of an operator instance is cluster-scoped, so the garbage collector does not remove
	// it with the instance namespace.
	if err := r.DeleteAllOf(ctx, &chrysopoeiav1.OperatorHarness{},
		client.MatchingLabels{operatorHarnessInstanceNamespaceLabel: helmNSName}); err != nil {
		return fmt.Errorf("unable to delete the OperatorHarness of %s: %w", helmNSName, err)
	}
	return nil
}

func (r *ReleaseController) ensureRelease(ctx context.Context, instance unstructured.Unstructured, helmNSName string, digest string, revision chrysopoeiav1.InstanceRevision) error {
	const saName = "instance-admin"
	ownerOpt := client.FieldOwner(fmt.Sprintf("release-controller:%s:%s:%s:%s", r.GVK.Group, r.GVK.Version, r.GVK.Kind, instance.GetName()))

	commonAnnotations := map[string]string{
		"chrysopoeia.io/claim-apiVersion": instance.GetAPIVersion(),
		"chrysopoeia.io/claim-kind":       instance.GetKind(),
		"chrysopoeia.io/claim-namespace":  instance.GetNamespace(),
		"chrysopoeia.io/claim-name":       instance.GetName(),
		"chrysopoeia.io/claim-uid":        string(instance.GetUID()),
		"chrysopoeia.io/revision-name":    revision.GetName(),
	}
	commonLabels := map[string]string{
		"chrysopoeia.io/instance": "",
	}

	requires := extractRequires(instance)
	for _, ref := range requires {
		commonLabels[RequiresLabelPrefix+ref.ScopeName()] = ""
	}

	// `provides` only says that the chart ships the CRDs of a group, which is a chart of its own if the
	// operator does not bundle them. The provider label is what consumers are granted access to and
	// what the harness scopes, so it names the operator instance: it follows `manages`.
	provides := extractProvides(instance)
	isProvider := len(provides) > 0

	manages := extractManages(instance)
	for _, ref := range manages {
		commonLabels[ProvidesLabelPrefix+ref.ScopeName()] = ""
	}

	if err := r.ensureOperatorHarness(ctx, instance, helmNSName, commonAnnotations, commonLabels, ownerOpt); err != nil {
		return fmt.Errorf("unable to ensure the OperatorHarness: %w", err)
	}

	// The scope labels name dependency groups, the RBAC below needs the CRDs they bundle.
	providedCRDs, err := r.resolveGroupCRDs(ctx, provides)
	if err != nil {
		return fmt.Errorf("unable to resolve provided dependency groups: %w", err)
	}
	requiredCRDs, err := r.resolveGroupCRDs(ctx, requires)
	if err != nil {
		return fmt.Errorf("unable to resolve required dependency groups: %w", err)
	}

	providerRoleName := strings.Join([]string{"chrysopoeia", "provider", helmNSName}, ":")
	cr := rbacv1ac.
		ClusterRole(providerRoleName).
		WithAnnotations(commonAnnotations).
		WithLabels(commonLabels)
	// An empty resourceNames list matches every CRD in RBAC, so an instance that ships no CRDs gets no
	// rule at all instead of write access to all of them.
	if len(providedCRDs) > 0 {
		cr = cr.WithRules(
			rbacv1ac.PolicyRule().
				WithAPIGroups("apiextensions.k8s.io").
				WithResources("customresourcedefinitions").
				WithResourceNames(providedCRDs...).
				WithVerbs("*"),
		)
	}
	if err := r.Apply(ctx, cr, ownerOpt); err != nil {
		return err
	}
	crb := rbacv1ac.
		ClusterRoleBinding(providerRoleName).
		WithAnnotations(commonAnnotations).
		WithLabels(commonLabels).
		WithRoleRef(
			rbacv1ac.RoleRef().
				WithAPIGroup("rbac.authorization.k8s.io").
				WithKind("ClusterRole").
				WithName(providerRoleName),
		).WithSubjects(
		rbacv1ac.Subject().
			WithKind("ServiceAccount").
			WithName(saName).
			WithNamespace(helmNSName),
	)
	if err := r.Apply(ctx, crb, ownerOpt); err != nil {
		return err
	}

	namespaceLabels, err := r.appuioNamespaceLabels(ctx, instance)
	if err != nil {
		return fmt.Errorf("failed to get APPUiO namespace labels: %w", err)
	}
	maps.Copy(namespaceLabels, commonLabels)
	namespaceLabels["chrysopoeia.io/managed"] = ""
	namespaceLabels["chrysopoeia.io/instance"] = ""
	if err := r.Apply(ctx,
		corev1ac.Namespace(helmNSName).
			WithAnnotations(commonAnnotations).
			WithLabels(namespaceLabels),
		ownerOpt); err != nil {
		return err
	}

	if err := r.Apply(ctx,
		corev1ac.ServiceAccount(saName, helmNSName).
			WithAnnotations(commonAnnotations).
			WithLabels(commonLabels),
		ownerOpt); err != nil {
		return err
	}

	adminRoleBinding := rbacv1ac.RoleBinding(fmt.Sprintf("%s-admin", saName), helmNSName).
		WithAnnotations(commonAnnotations).
		WithLabels(commonLabels).
		WithRoleRef(
			rbacv1ac.RoleRef().
				WithAPIGroup("rbac.authorization.k8s.io").
				WithKind("ClusterRole").
				WithName("admin"),
		).WithSubjects(
		rbacv1ac.Subject().
			WithKind("ServiceAccount").
			WithName(saName).
			WithNamespace(helmNSName),
	)
	if err := r.Apply(ctx, adminRoleBinding, ownerOpt); err != nil {
		return err
	}

	rbacRequires := make([]*rbacv1ac.PolicyRuleApplyConfiguration, 0, len(requiredCRDs))
	for _, crd := range requiredCRDs {
		resource, group, found := strings.Cut(crd, ".")
		if !found {
			return fmt.Errorf("invalid CRD name %q in a required dependency group", crd)
		}
		rbacRequires = append(
			rbacRequires, rbacv1ac.PolicyRule().
				WithAPIGroups(group).
				WithResources(resource).
				WithVerbs("*"),
		)
	}
	requiresRoleName := strings.Join([]string{"chrysopoeia", "requires"}, ":")
	requiresRole := rbacv1ac.
		Role("chrysopoeia:requires", helmNSName).
		WithAnnotations(commonAnnotations).
		WithLabels(commonLabels).
		WithRules(rbacRequires...)
	if err := r.Apply(ctx, requiresRole, ownerOpt); err != nil {
		return err
	}
	requiresRoleBinding := rbacv1ac.RoleBinding("chrysopoeia:requires-instance-admin", helmNSName).
		WithAnnotations(commonAnnotations).
		WithLabels(commonLabels).
		WithRoleRef(
			rbacv1ac.RoleRef().
				WithAPIGroup("rbac.authorization.k8s.io").
				WithKind("Role").
				WithName(requiresRoleName),
		).WithSubjects(
		rbacv1ac.Subject().
			WithKind("ServiceAccount").
			WithName(saName).
			WithNamespace(helmNSName),
	)
	if err := r.Apply(ctx, requiresRoleBinding, ownerOpt); err != nil {
		return err
	}

	artifact := &sourcev1.OCIRepository{}
	artifact.SetGroupVersionKind(sourcev1.GroupVersion.WithKind("OCIRepository"))
	artifact.SetNamespace(helmNSName)
	artifact.SetName(fmt.Sprintf("artifact-%s", strings.TrimPrefix(digest, "sha256:")))
	artifact.SetAnnotations(commonAnnotations)
	artifact.SetLabels(commonLabels)
	artifact.Spec.URL = revision.Spec.OCIUrl
	// We pin the artifact to the digest of the approved revision, and set a long interval to avoid unnecessary re-reconciliation.
	artifact.Spec.Interval = metav1.Duration{Duration: 9 * 24 * time.Hour}
	artifact.Spec.Reference = &sourcev1.OCIRepositoryRef{
		Digest: digest,
	}
	aac, err := runtime.DefaultUnstructuredConverter.ToUnstructured(artifact)
	if err != nil {
		return fmt.Errorf("failed to convert OCIRepository to unstructured: %w", err)
	}
	if err := r.Apply(ctx, client.ApplyConfigurationFromUnstructured(&unstructured.Unstructured{Object: aac}), ownerOpt); err != nil {
		return err
	}

	crdStrategy := helmv2.Skip
	if isProvider {
		crdStrategy = helmv2.CreateReplace
	}
	// https://fluxcd.io/flux/components/helm/helmreleases/#recommended-settings
	release := &helmv2.HelmRelease{
		Spec: helmv2.HelmReleaseSpec{
			ChartRef: &helmv2.CrossNamespaceSourceReference{
				APIVersion: artifact.APIVersion,
				Kind:       artifact.Kind,
				Name:       artifact.GetName(),
			},
			CommonMetadata: &helmv2.CommonMetadata{
				Labels:      commonLabels,
				Annotations: commonAnnotations,
			},

			ServiceAccountName: saName,
			Interval:           metav1.Duration{Duration: 30 * time.Minute},
			DriftDetection: &helmv2.DriftDetection{
				Mode: helmv2.DriftDetectionWarn,
			},
			Install: &helmv2.Install{
				Strategy: &helmv2.InstallStrategy{
					Name:          "RetryOnFailure",
					RetryInterval: &metav1.Duration{Duration: 5 * time.Minute},
				},
				CRDs: crdStrategy,
			},
			Upgrade: &helmv2.Upgrade{
				Strategy: &helmv2.UpgradeStrategy{
					Name:          "RetryOnFailure",
					RetryInterval: &metav1.Duration{Duration: 5 * time.Minute},
				},
				CRDs: crdStrategy,
			},
		},
	}
	release.SetGroupVersionKind(helmv2.GroupVersion.WithKind("HelmRelease"))
	release.SetNamespace(helmNSName)
	release.SetName(instance.GetName())
	release.SetAnnotations(commonAnnotations)
	helmLabels := map[string]string{"chrysopoeia.io/managed": ""}
	maps.Copy(helmLabels, commonLabels)
	release.SetLabels(helmLabels)
	if len(revision.Spec.Values.Raw) > 0 {
		release.Spec.Values = revision.Spec.Values.DeepCopy()
	}

	hrac, err := runtime.DefaultUnstructuredConverter.ToUnstructured(release)
	if err != nil {
		return fmt.Errorf("failed to convert HelmRelease to unstructured: %w", err)
	}
	if err := r.Apply(ctx, client.ApplyConfigurationFromUnstructured(&unstructured.Unstructured{Object: hrac}), ownerOpt); err != nil {
		return err
	}
	return nil
}

// appuioNamespaceLabels returns a map of labels that are required on the APPUiO platform for its multi-tenant feautures.
// TODO: This might be done more generically eg. with CEL but whatever, sometimes the simplest solution is the best.
func (r *ReleaseController) appuioNamespaceLabels(ctx context.Context, claim unstructured.Unstructured) (map[string]string, error) {
	labels := map[string]string{
		"appuio.io/unmanaged-namespace": "",
	}

	var claimNS corev1.Namespace
	if err := r.Get(ctx, types.NamespacedName{Name: claim.GetNamespace()}, &claimNS); err != nil {
		return nil, fmt.Errorf("failed to get namespace for claim %s: %w", claim.GetNamespace(), err)
	}

	const orgLabel = "appuio.io/organization"
	if org := claimNS.Labels[orgLabel]; org != "" {
		labels[orgLabel] = org
	}

	return labels, nil
}

func (r *ReleaseController) ControllerName() string {
	return "release-controller"
}

func (r *ReleaseController) SetupDynamicControllerWithWatches(dynCtrl controller.TypedController[reconcile.Request], mgr ctrl.Manager, gvk schema.GroupVersionKind) error {
	r.Client = mgr.GetClient()
	r.Scheme = mgr.GetScheme()
	r.Recorder = mgr.GetEventRecorder(fmt.Sprintf("%s-%s-%s-%s", r.ControllerName(), gvk.Group, gvk.Version, gvk.Kind))
	r.GVK = gvk

	target := &unstructured.Unstructured{}
	target.SetGroupVersionKind(gvk)

	if err := dynCtrl.Watch(source.TypedKind(mgr.GetCache(), client.Object(target), &handler.TypedEnqueueRequestForObject[client.Object]{})); err != nil {
		return fmt.Errorf("failed to watch target resource: %w", err)
	}
	if err := dynCtrl.Watch(source.TypedKind(mgr.GetCache(), &chrysopoeiav1.InstanceRevision{}, handler.TypedEnqueueRequestForOwner[*chrysopoeiav1.InstanceRevision](mgr.GetScheme(), mgr.GetRESTMapper(), target, handler.OnlyControllerOwner()))); err != nil {
		return fmt.Errorf("failed to watch InstanceRevision resource: %w", err)
	}
	if err := dynCtrl.Watch(source.TypedKind(mgr.GetCache(), &helmv2.HelmRelease{}, handler.TypedEnqueueRequestsFromMapFunc(func(ctx context.Context, hr *helmv2.HelmRelease) []reconcile.Request {
		a := hr.GetAnnotations()
		instanceName := a["chrysopoeia.io/claim-name"]
		instanceNamespace := a["chrysopoeia.io/claim-namespace"]
		if instanceName != "" && instanceNamespace != "" {
			return []reconcile.Request{
				{NamespacedName: client.ObjectKey{Namespace: instanceNamespace, Name: instanceName}},
			}
		}
		return nil
	}))); err != nil {
		return fmt.Errorf("failed to watch HelmRelease resource: %w", err)
	}

	return nil
}

func extractRequires(revision unstructured.Unstructured) []chrysopoeiav1.DependencyGroupReference {
	return extractDependencies(revision, "requires")
}

func extractManages(revision unstructured.Unstructured) []chrysopoeiav1.DependencyGroupReference {
	return extractDependencies(revision, "manages")
}

func extractProvides(revision unstructured.Unstructured) []chrysopoeiav1.DependencyGroupReference {
	return extractDependencies(revision, "provides")
}

// extractDependencies reads the DependencyGroup references from `.spec.<key>` of an instance.
// References to anything but a DependencyGroup are ignored, there are none yet.
func extractDependencies(revision unstructured.Unstructured, key string) []chrysopoeiav1.DependencyGroupReference {
	dependencies, found, err := unstructured.NestedSlice(revision.Object, "spec", key)
	if err != nil || !found {
		return nil
	}

	refs := make([]chrysopoeiav1.DependencyGroupReference, 0, len(dependencies))
	for _, dependency := range dependencies {
		m, ok := dependency.(map[string]any)
		if !ok {
			continue
		}
		group, ok := m["dependencyGroup"].(map[string]any)
		if !ok {
			continue
		}
		name, ok := group["name"].(string)
		if !ok || name == "" {
			continue
		}
		alias, _ := group["as"].(string)
		refs = append(refs, chrysopoeiav1.DependencyGroupReference{Name: name, As: alias})
	}

	return refs
}

// ensureOperatorHarness harnesses the operator an instance deploys, if it manages a DependencyGroup.
// The harness scopes the operator to the label of the group's scope, so that it only ever sees the
// resources of the instances that requested that scope.
//
// The harness is named after the scope, not after the instance: an operator deployed a second time
// under an alias serves a different scope and so gets its own harness, while the same scope is
// always served by exactly one operator deployment.
func (r *ReleaseController) ensureOperatorHarness(
	ctx context.Context,
	instance unstructured.Unstructured,
	helmNSName string,
	annotations, labels map[string]string,
	ownerOpt client.FieldOwner,
) error {
	manages := extractManages(instance)
	if len(manages) == 0 {
		return nil
	}
	// `manages` holds at most one group, the CRD enforces it.
	scope := manages[0].ScopeName()

	harnessLabels := maps.Clone(labels)
	// The harness is cluster-scoped and cannot be owned by the namespaced instance, so it is tied to
	// the instance namespace by label and removed with it in cleanupRelease.
	harnessLabels[operatorHarnessInstanceNamespaceLabel] = helmNSName

	harness := chrysopoeiav1ac.
		OperatorHarness(scope).
		WithAnnotations(annotations).
		WithLabels(harnessLabels).
		WithSpec(
			chrysopoeiav1ac.OperatorHarnessSpec().
				WithScopeToLabel(RequiresLabelPrefix + scope).
				WithOperator(
					chrysopoeiav1ac.OperatorHarnessOperator().
						WithNamespace(helmNSName).
						// Every pod in the operator's own instance namespace belongs to the operator,
						// and the service accounts its chart creates are not known here.
						WithInjectProxyConfiguration(true),
				),
		)

	return r.Apply(ctx, harness, client.ForceOwnership, ownerOpt)
}

// resolveGroupCRDs returns the names of the CRDs bundled by the referenced DependencyGroups.
// A group that is not accepted is an error: its claim on the CRDs is disputed, so granting access to
// them could hand out resources another group owns.
func (r *ReleaseController) resolveGroupCRDs(ctx context.Context, refs []chrysopoeiav1.DependencyGroupReference) ([]string, error) {
	var crds []string
	for _, ref := range refs {
		var group chrysopoeiav1.DependencyGroup
		if err := r.Get(ctx, client.ObjectKey{Name: ref.Name}, &group); err != nil {
			return nil, fmt.Errorf("unable to get DependencyGroup %q: %w", ref.Name, err)
		}
		if group.Status.State != chrysopoeiav1.DependencyGroupAccepted {
			return nil, fmt.Errorf("DependencyGroup %q is not accepted, its state is %q", ref.Name, group.Status.State)
		}
		for _, crd := range group.Spec.CRDs {
			crds = append(crds, crd.Name)
		}
	}
	return crds, nil
}
