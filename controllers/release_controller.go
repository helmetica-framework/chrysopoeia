package controllers

import (
	"context"
	"crypto/sha256"
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
)

type ReleaseController struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder events.EventRecorder

	// GVK is the GroupVersionKind of the resource that this controller manages.
	GVK schema.GroupVersionKind
}

//+kubebuilder:rbac:groups=helmetica.io,resources=instancerevisions,verbs=get;list;watch

//+kubebuilder:rbac:groups=helm.toolkit.fluxcd.io,resources=helmreleases,verbs=get;list;watch;update;patch

func NewReleaseController() DynamicReconciler {
	return &ReleaseController{}
}

func (r *ReleaseController) Reconcile(ctx context.Context, req reconcile.Request) (res ctrl.Result, err error) {
	l := log.FromContext(ctx).WithName("ReleaseController.Reconcile").WithValues("request", req)
	l.Info("Reconciling Instance")

	instanceNSName := r.instanceNamespaceName(req.NamespacedName)

	var instance unstructured.Unstructured
	instance.SetAPIVersion(r.GVK.GroupVersion().String())
	instance.SetKind(r.GVK.Kind)
	if err := r.Get(ctx, req.NamespacedName, &instance); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, r.cleanupRelease(ctx, instanceNSName)
		}
		return ctrl.Result{}, err
	}
	if !instance.GetDeletionTimestamp().IsZero() {
		return ctrl.Result{}, nil
	}

	var revisions chrysopoeiav1.InstanceRevisionList
	if err := r.List(ctx, &revisions, client.InNamespace(req.Namespace), client.MatchingFields{ownerUIDField: string(instance.GetUID())}); err != nil {
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

	if err := r.ensureRelease(ctx, instance, instanceNSName, digest, revision); err != nil {
		return ctrl.Result{}, err
	}

	var release helmv2.HelmRelease
	if err := r.Get(ctx, client.ObjectKey{Namespace: instanceNSName, Name: instance.GetName()}, &release); err != nil {
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
	statusPatch.SetGroupVersionKind(instance.GroupVersionKind())
	statusPatch.SetName(instance.GetName())
	statusPatch.SetNamespace(instance.GetNamespace())
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
	_, _ = fmt.Fprint(gvkh, r.GVK.Group, r.GVK.Version, r.GVK.Kind)
	name := fmt.Sprintf("x-%x-%s-%s", gvkh.Sum(nil)[:4], nsn.Namespace, nsn.Name)
	if len(name) <= 63 {
		return name
	}
	prefix := name[:63-9]
	hash := sha256.Sum256([]byte(name))
	return fmt.Sprintf("%s-%x", prefix, hash[:4])
}

func (r *ReleaseController) cleanupRelease(ctx context.Context, helmNSName string) error {
	log.FromContext(ctx).WithName("cleanupRelease").Info("Cleaning up release", "namespace", helmNSName)

	if err := r.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: helmNSName}}); err != nil && !apierrors.IsNotFound(err) {
		return err
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
	for _, r := range requires {
		commonLabels[fmt.Sprintf("requires.helmetica.io/%s", r)] = ""
	}

	provides := extractProvides(instance)
	isProvider := len(provides) > 0
	for _, p := range provides {
		commonLabels[fmt.Sprintf("provides.helmetica.io/%s", p)] = ""
	}

	providerRoleName := strings.Join([]string{"chrysopoeia", "provider", helmNSName}, ":")
	cr := rbacv1ac.
		ClusterRole(providerRoleName).
		WithAnnotations(commonAnnotations).
		WithLabels(commonLabels).
		WithRules(
			rbacv1ac.PolicyRule().
				WithAPIGroups("apiextensions.k8s.io").
				WithResources("customresourcedefinitions").
				WithResourceNames(provides...).
				WithVerbs("*"),
		)
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

	if err := r.Apply(ctx,
		corev1ac.Namespace(helmNSName).
			WithAnnotations(commonAnnotations).
			WithLabels(commonLabels),
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

	rbacRequires := make([]*rbacv1ac.PolicyRuleApplyConfiguration, 0, len(requires))
	for _, r := range requires {
		resource, group, found := strings.Cut(r, ".")
		if !found {
			return fmt.Errorf("invalid requires format: %s", r)
		}
		rbacRequires = append(rbacRequires, rbacv1ac.PolicyRule().
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

func extractRequires(revision unstructured.Unstructured) []string {
	return extractDependencies(revision, "requires")
}

func extractProvides(revision unstructured.Unstructured) []string {
	return extractDependencies(revision, "provides")
}

func extractDependencies(revision unstructured.Unstructured, key string) []string {
	requires, found, err := unstructured.NestedSlice(revision.Object, "spec", key)
	if err != nil || !found {
		return nil
	}
	strs := make([]string, 0, len(requires))
	for _, r := range requires {
		if m, ok := r.(map[string]any); ok {
			if name, ok := m["name"].(string); ok {
				strs = append(strs, name)
			}
		}
	}

	return strs
}
