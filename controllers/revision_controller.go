package controllers

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	sourcev1 "github.com/fluxcd/source-controller/api/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	chrysopoeiav1 "github.com/helmetica-framework/chrysopoeia/api/v1"
)

type RevisionManager struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder events.EventRecorder

	// GVK is the GroupVersionKind of the resource that this controller manages.
	// The controller is dynamically created for each GVK that is registered with the RevisionManagerManager.
	GVK schema.GroupVersionKind
}

//+kubebuilder:rbac:groups=helmetica.io,resources=instancerevisions,verbs=get;list;watch;create;update;patch
//+kubebuilder:rbac:groups=helmetica.io,resources=instancerevisions/status,verbs=get;update;patch

//+kubebuilder:rbac:groups=source.toolkit.fluxcd.io,resources=ocirepositories,verbs=get;list;watch;create;update;patch

func NewRevisionManager() DynamicReconciler {
	return &RevisionManager{}
}

func (r *RevisionManager) Reconcile(ctx context.Context, req reconcile.Request) (res ctrl.Result, err error) {
	l := log.FromContext(ctx).WithName("RevisionManager.Reconcile").WithValues("request", req)
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

	ociUrl, _, err := unstructured.NestedString(instance.Object, "spec", "ociUrl")
	if err != nil {
		l.Error(err, "Failed to get ociUrl from instance")
		return ctrl.Result{}, err
	}
	version, _, err := unstructured.NestedString(instance.Object, "spec", "version")
	if err != nil {
		l.Error(err, "Failed to get version from instance")
		return ctrl.Result{}, err
	}
	values, _, err := unstructured.NestedMap(instance.Object, "spec", "values")
	if err != nil {
		l.Error(err, "Failed to get values from instance")
		return ctrl.Result{}, err
	}

	ociRepo, err := r.ensureOCIRepository(ctx, instance, ociUrl, version)
	if err != nil {
		l.Error(err, "Failed to ensure OCIRepository")
		return ctrl.Result{}, err
	}
	if !apimeta.IsStatusConditionTrue(ociRepo.Status.Conditions, "Ready") {
		l.Info("OCIRepository not yet ready, requeuing")
		return ctrl.Result{}, nil
	}

	versionWithDigest := ociRepo.Status.Artifact.Revision

	shaSum := sha256.New()

	if _, err := shaSum.Write([]byte(ociUrl)); err != nil {
		l.Error(err, "Failed to write ociUrl to shaSum")
		return ctrl.Result{}, err
	}
	if _, err := shaSum.Write([]byte(versionWithDigest)); err != nil {
		l.Error(err, "Failed to write version to shaSum")
		return ctrl.Result{}, err
	}
	if err := json.NewEncoder(shaSum).Encode(values); err != nil {
		l.Error(err, "Failed to write values to shaSum")
		return ctrl.Result{}, err
	}

	var rev chrysopoeiav1.InstanceRevision
	rev.APIVersion = chrysopoeiav1.GroupVersion.String()
	rev.Kind = "InstanceRevision"
	rev.SetNamespace(instance.GetNamespace())
	rev.SetName(strings.Join([]string{instance.GetName(), fmt.Sprintf("%x", shaSum.Sum(nil))}, "-"))
	if err := controllerutil.SetControllerReference(&instance, &rev, r.Scheme); err != nil {
		l.Error(err, "Failed to set controller reference")
		return ctrl.Result{}, err
	}
	rev.Spec.Version = versionWithDigest
	rev.Spec.OCIUrl = ociUrl
	rev.Spec.Values.Raw, err = json.Marshal(values)
	if err != nil {
		l.Error(err, "Failed to marshal values")
		return ctrl.Result{}, err
	}

	u, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&rev)
	if err != nil {
		l.Error(err, "Failed to convert CRD to unstructured")
		return ctrl.Result{}, err
	}

	if err := r.Apply(ctx, client.ApplyConfigurationFromUnstructured(&unstructured.Unstructured{Object: u}), client.FieldOwner("chrysopoeia-controller")); err != nil {
		l.Error(err, "Failed to apply instance revision")
		return ctrl.Result{}, err
	}

	revField, _, err := unstructured.NestedString(instance.Object, "status", "latestRevision")
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to get latestRevision from instance status: %w", err)
	}
	if revField != rev.GetName() {
		if err := unstructured.SetNestedField(instance.Object, rev.GetName(), "status", "latestRevision"); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to set latestRevision in instance status: %w", err)
		}
		if err := r.Status().Update(ctx, &instance); err != nil {
			l.Error(err, "Failed to update instance status")
			return ctrl.Result{}, err
		}
	}

	return ctrl.Result{}, nil
}

func (r *RevisionManager) ControllerName() string {
	return "revision-controller"
}

func (r *RevisionManager) SetupDynamicControllerWithWatches(dynCtrl controller.TypedController[reconcile.Request], mgr ctrl.Manager, gvk schema.GroupVersionKind) error {
	r.Client = mgr.GetClient()
	r.Scheme = mgr.GetScheme()
	r.Recorder = mgr.GetEventRecorder(fmt.Sprintf("revision-controller-%s-%s-%s", gvk.Group, gvk.Version, gvk.Kind))
	r.GVK = gvk

	target := &unstructured.Unstructured{}
	target.SetGroupVersionKind(gvk)

	if err := dynCtrl.Watch(source.TypedKind(mgr.GetCache(), client.Object(target), &handler.TypedEnqueueRequestForObject[client.Object]{})); err != nil {
		return fmt.Errorf("failed to watch target resource: %w", err)
	}
	if err := dynCtrl.Watch(source.TypedKind(mgr.GetCache(), &sourcev1.OCIRepository{}, handler.TypedEnqueueRequestForOwner[*sourcev1.OCIRepository](mgr.GetScheme(), mgr.GetRESTMapper(), target))); err != nil {
		return fmt.Errorf("failed to watch OCIRepository resource: %w", err)
	}
	if err := dynCtrl.Watch(source.TypedKind(mgr.GetCache(), &chrysopoeiav1.InstanceRevision{}, handler.TypedEnqueueRequestForOwner[*chrysopoeiav1.InstanceRevision](mgr.GetScheme(), mgr.GetRESTMapper(), target, handler.OnlyControllerOwner()))); err != nil {
		return fmt.Errorf("failed to watch InstanceRevision resource: %w", err)
	}

	return nil
}

// ensureOCIRepository ensures that an OCIRepository exists for the given instance and returns it.
// callers should check the status of the returned OCIRepository to ensure that it is ready before proceeding with any further actions.
func (r *RevisionManager) ensureOCIRepository(ctx context.Context, instance unstructured.Unstructured, ociUrl, version string) (*sourcev1.OCIRepository, error) {
	var ociRepo sourcev1.OCIRepository
	ociRepo.SetGroupVersionKind(sourcev1.GroupVersion.WithKind("OCIRepository"))
	ociRepo.SetNamespace(instance.GetNamespace())
	ociRepo.SetName(strings.Join([]string{"chrysopoeia", fmt.Sprintf("%x", sha256.Sum256([]byte(ociUrl)))[0:10], version}, "-"))
	ociRepo.Spec.URL = ociUrl
	ociRepo.Spec.Reference = &sourcev1.OCIRepositoryRef{Tag: version}
	ociRepo.Spec.Interval = metav1.Duration{Duration: 24 * time.Hour}

	if err := controllerutil.SetOwnerReference(&instance, &ociRepo, r.Scheme); err != nil {
		return nil, fmt.Errorf("failed to set owner reference: %w", err)
	}

	ac, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&ociRepo)
	if err != nil {
		return nil, fmt.Errorf("failed to convert OCIRepository to unstructured apply config: %w", err)
	}

	if err := r.Apply(ctx, client.ApplyConfigurationFromUnstructured(&unstructured.Unstructured{Object: ac}), client.FieldOwner(fmt.Sprintf("chrysopoeia-controller:%s", instance.GetName()))); err != nil {
		return nil, fmt.Errorf("failed to apply OCIRepository: %w", err)
	}

	if err := r.Get(ctx, client.ObjectKeyFromObject(&ociRepo), &ociRepo); err != nil && !apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("failed to get OCIRepository: %w", err)
	}

	return &ociRepo, nil
}
