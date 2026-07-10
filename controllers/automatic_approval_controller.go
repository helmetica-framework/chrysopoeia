package controllers

import (
	"context"
	"fmt"
	"slices"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
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

type AutomaticApprovalManager struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder events.EventRecorder

	// GVK is the GroupVersionKind of the resource that this controller manages.
	GVK schema.GroupVersionKind
}

//+kubebuilder:rbac:groups=helmetica.io,resources=instancerevisions,verbs=get;list;watch;update;patch
//+kubebuilder:rbac:groups=helmetica.io,resources=instancerevisions/status,verbs=get;update;patch

func NewAutomaticApprovalManager() DynamicReconciler {
	return &AutomaticApprovalManager{}
}

func (r *AutomaticApprovalManager) Reconcile(ctx context.Context, req reconcile.Request) (res ctrl.Result, err error) {
	l := log.FromContext(ctx).WithName("AutomaticApprovalManager.Reconcile").WithValues("request", req)
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

	if instance.Object["spec"] == nil {
		l.Info("Instance does not have a spec, skipping automatic approval")
		return ctrl.Result{}, nil
	}
	if strategy, _, _ := unstructured.NestedString(instance.Object, "spec", "approval", "strategy"); strategy != "Automatic" {
		l.Info("Instance does not have automatic approval strategy, skipping")
		return ctrl.Result{}, nil
	}

	wantedRevisionName, _, _ := unstructured.NestedString(instance.Object, "status", "latestRevision")
	if wantedRevisionName == "" {
		l.Info("Instance does not have a latest revision, skipping automatic approval")
		return ctrl.Result{}, nil
	}
	var wantedRevision chrysopoeiav1.InstanceRevision
	if err := r.Get(ctx, client.ObjectKey{Namespace: req.Namespace, Name: wantedRevisionName}, &wantedRevision); err != nil {
		return ctrl.Result{}, err
	}

	var revisions chrysopoeiav1.InstanceRevisionList
	if err := r.List(ctx, &revisions, client.InNamespace(req.Namespace), client.MatchingFields{ownerUIDField: string(instance.GetUID())}); err != nil {
		return ctrl.Result{}, err
	}
	sortByApprovalNewestFirst(revisions.Items)

	if len(revisions.Items) == 0 || revisions.Items[0].Name != wantedRevisionName || revisions.Items[0].Spec.ApprovedAt == nil {
		wantedRevision.Spec.ApprovedAt = new(metav1.Now())
		if err := r.Update(ctx, &wantedRevision); err != nil {
			return ctrl.Result{}, err
		}
		l.Info("Approved latest revision", "revision", wantedRevision.Name)
	}

	return ctrl.Result{}, nil
}

func (r *AutomaticApprovalManager) SetupDynamicControllerWithWatches(dynCtrl controller.TypedController[reconcile.Request], mgr ctrl.Manager, gvk schema.GroupVersionKind) error {
	r.Client = mgr.GetClient()
	r.Scheme = mgr.GetScheme()
	r.Recorder = mgr.GetEventRecorder(fmt.Sprintf("automatic-approval-controller-%s-%s-%s", gvk.Group, gvk.Version, gvk.Kind))
	r.GVK = gvk

	target := &unstructured.Unstructured{}
	target.SetGroupVersionKind(gvk)

	if err := dynCtrl.Watch(source.TypedKind(mgr.GetCache(), client.Object(target), &handler.TypedEnqueueRequestForObject[client.Object]{})); err != nil {
		return fmt.Errorf("failed to watch target resource: %w", err)
	}
	if err := dynCtrl.Watch(source.TypedKind(mgr.GetCache(), &chrysopoeiav1.InstanceRevision{}, handler.TypedEnqueueRequestForOwner[*chrysopoeiav1.InstanceRevision](mgr.GetScheme(), mgr.GetRESTMapper(), target, handler.OnlyControllerOwner()))); err != nil {
		return fmt.Errorf("failed to watch InstanceRevision resource: %w", err)
	}

	return nil
}

// sortByCreationTimestampNewestFirst sorts the given slice of InstanceRevision objects by their creation timestamp in descending order (newest first).
func sortByCreationTimestampNewestFirst(revisions []chrysopoeiav1.InstanceRevision) {
	slices.SortFunc(revisions, func(a, b chrysopoeiav1.InstanceRevision) int {
		if a.CreationTimestamp.Time.Before(b.CreationTimestamp.Time) {
			return -1
		}
		if a.CreationTimestamp.Time.After(b.CreationTimestamp.Time) {
			return 1
		}
		return 0
	})
	slices.Reverse(revisions)
}

// sortByApprovalNewestFirst sorts the given slice of InstanceRevision objects by their approval timestamp in descending order (newest first).
func sortByApprovalNewestFirst(revisions []chrysopoeiav1.InstanceRevision) {
	slices.SortFunc(revisions, func(a, b chrysopoeiav1.InstanceRevision) int {
		if a.Spec.ApprovedAt != nil && b.Spec.ApprovedAt != nil {
			if a.Spec.ApprovedAt.Time.Before(b.Spec.ApprovedAt.Time) {
				return -1
			}
			if a.Spec.ApprovedAt.Time.After(b.Spec.ApprovedAt.Time) {
				return 1
			}
		} else if a.Spec.ApprovedAt != nil {
			return 1
		} else if b.Spec.ApprovedAt != nil {
			return -1
		}
		return 0
	})
	slices.Reverse(revisions)
}
