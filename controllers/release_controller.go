package controllers

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	helmv2 "github.com/fluxcd/helm-controller/api/v2"
	sourcev1 "github.com/fluxcd/source-controller/api/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
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

	var instance unstructured.Unstructured
	instance.SetAPIVersion(r.GVK.GroupVersion().String())
	instance.SetKind(r.GVK.Kind)
	if err := r.Get(ctx, req.NamespacedName, &instance); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
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

	helmNSName := fmt.Sprintf("x-%s-%s", instance.GetNamespace(), instance.GetName())
	if err := r.ensureRelease(ctx, instance, helmNSName, digest, revision); err != nil {
		return ctrl.Result{}, err
	}

	var release helmv2.HelmRelease
	if err := r.Get(ctx, client.ObjectKey{Namespace: helmNSName, Name: instance.GetName()}, &release); err != nil {
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
	); err != nil {
		return ctrl.Result{}, err
	}

	if err := r.Status().Apply(ctx, client.ApplyConfigurationFromUnstructured(statusPatch), client.FieldOwner("chrysopoeia:release-controller:status")); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

func (r *ReleaseController) ensureRelease(ctx context.Context, instance unstructured.Unstructured, helmNSName string, digest string, revision chrysopoeiav1.InstanceRevision) error {
	const saName = "instance-admin"
	ownerOpt := client.FieldOwner(fmt.Sprintf("release-controller:%s:%s:%s:%s", r.GVK.Group, r.GVK.Version, r.GVK.Kind, instance.GetName()))

	if err := r.Apply(ctx, corev1ac.Namespace(helmNSName), ownerOpt); err != nil {
		return err
	}

	if err := r.Apply(ctx, corev1ac.ServiceAccount(saName, helmNSName), ownerOpt); err != nil {
		return err
	}

	adminRole := rbacv1ac.RoleBinding(fmt.Sprintf("%s-admin", saName), helmNSName).
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
	if err := r.Apply(ctx, adminRole, ownerOpt); err != nil {
		return err
	}

	artifact := &sourcev1.OCIRepository{}
	artifact.SetGroupVersionKind(sourcev1.GroupVersion.WithKind("OCIRepository"))
	artifact.SetNamespace(helmNSName)
	artifact.SetName(fmt.Sprintf("artifact-%s", strings.TrimPrefix(digest, "sha256:")))
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

	// https://fluxcd.io/flux/components/helm/helmreleases/#recommended-settings
	release := &helmv2.HelmRelease{
		Spec: helmv2.HelmReleaseSpec{
			ChartRef: &helmv2.CrossNamespaceSourceReference{
				APIVersion: artifact.APIVersion,
				Kind:       artifact.Kind,
				Name:       artifact.GetName(),
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
			},
			Upgrade: &helmv2.Upgrade{
				Strategy: &helmv2.UpgradeStrategy{
					Name:          "RetryOnFailure",
					RetryInterval: &metav1.Duration{Duration: 5 * time.Minute},
				},
			},
		},
	}
	release.SetGroupVersionKind(helmv2.GroupVersion.WithKind("HelmRelease"))
	release.SetNamespace(helmNSName)
	release.SetName(instance.GetName())
	release.SetAnnotations(map[string]string{
		"chrysopoeia.io/instance-uid":       string(instance.GetUID()),
		"chrysopoeia.io/instance-name":      instance.GetName(),
		"chrysopoeia.io/instance-namespace": instance.GetNamespace(),
		"chrysopoeia.io/revision-name":      revision.GetName(),
	})
	release.SetLabels(map[string]string{
		"chrysopoeia.io/managed": "",
	})
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
		instanceName := a["chrysopoeia.io/instance-name"]
		instanceNamespace := a["chrysopoeia.io/instance-namespace"]
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
