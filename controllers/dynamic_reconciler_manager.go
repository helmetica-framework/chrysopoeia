package controllers

import (
	"context"
	"errors"
	"fmt"
	"sync"

	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const revisionControllerFinalizer = "chrysopoeia.io/revision-controller-cleanup"

type DynamicReconciler interface {
	reconcile.TypedReconciler[reconcile.Request]
	SetupDynamicControllerWithWatches(dynCtrl controller.TypedController[reconcile.Request], mgr ctrl.Manager, gvk schema.GroupVersionKind) error
}

// DynamicReconcilerManager reacts to the creation of new CRDs and creates a new controller for each GVK that is registered with it. It also stops the controller when the CRD is deleted.
// TODO: No supervision of the controllers is done, if a controller fails to start or crashes it will not be restarted.
// Normally the controller just exits and K8s will restart the pod, but here we probably don't want to influence other running controllers if one resource is faulty.
// For the PoC it is good enough I think since most startup error are probably due to bugs in code and will be fixed within the development cycle.
type DynamicReconcilerManager struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder events.EventRecorder

	ManagedReconcilers []func() DynamicReconciler

	ControllerLifetimeCtx context.Context

	manager ctrl.Manager

	// controllersMux protects the controllers map.
	// While this controller does not access the map concurrently the metrics collector does.
	controllersMux sync.Mutex
	// controllers holds the actual reconcilers, one for each managed resource.
	controllers map[controllersKey]stopper
}

type stopper struct {
	ctrls []*instanceController
}

func (s *stopper) stop() {
	for _, ctrl := range s.ctrls {
		ctrl.stop()
	}
}

func (s *stopper) wait() {
	for _, ctrl := range s.ctrls {
		<-ctrl.done
	}
}

type controllersKey struct {
	Group string
	Kind  string
}

//+kubebuilder:rbac:groups=apiextensions.k8s.io,resources=customresourcedefinitions,verbs=get;list;watch
//+kubebuilder:rbac:groups=apiextensions.k8s.io,resources=customresourcedefinitions/finalizers,verbs=update

func (r *DynamicReconcilerManager) Reconcile(ctx context.Context, req ctrl.Request) (res ctrl.Result, err error) {
	l := log.FromContext(ctx).WithName("DynamicReconcilerManager.Reconcile")
	l.Info("Reconciling Instance")

	var instance apiextv1.CustomResourceDefinition
	if err := r.Get(ctx, req.NamespacedName, &instance); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
	if !instance.GetDeletionTimestamp().IsZero() {
		l.Info("Instance is being deleted, stopping controller")
		gvk, err := extractGroupVersionKindFromCRD(instance)
		if err != nil {
			l.Error(err, "Failed to extract GVK from CRD")
			return ctrl.Result{}, err
		}
		if err := r.stopAndRemoveControllerFor(gvk); err != nil {
			l.Error(err, "Failed to stop and remove controller for instance")
			return ctrl.Result{}, err
		}

		return ctrl.Result{}, r.removeFinalizer(ctx, &instance)
	}

	var established bool
	for _, cond := range instance.Status.Conditions {
		if cond.Type == apiextv1.Established && cond.Status == apiextv1.ConditionTrue {
			established = true
			break
		}
	}
	if !established {
		l.Info("Instance is not yet established")
		return ctrl.Result{}, nil
	}

	gvk, err := extractGroupVersionKindFromCRD(instance)
	if err != nil {
		l.Error(err, "Failed to extract GVK from CRD")
		return ctrl.Result{}, err
	}

	if err := r.addFinalizer(ctx, &instance); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to add finalizer: %w", err)
	}

	if err := r.ensureInstanceControllerFor(ctx, gvk); err != nil {
		l.Error(err, "Failed to ensure instance controller")
		return ctrl.Result{}, err
	}

	l.Info("Finished reconciling")

	return ctrl.Result{}, nil
}

func (r *DynamicReconcilerManager) addFinalizer(ctx context.Context, instance *apiextv1.CustomResourceDefinition) error {
	if controllerutil.AddFinalizer(instance, revisionControllerFinalizer) {
		if err := r.Update(ctx, instance); err != nil {
			return fmt.Errorf("failed to patch finalizer: %w", err)
		}
	}
	return nil
}

func (r *DynamicReconcilerManager) removeFinalizer(ctx context.Context, instance *apiextv1.CustomResourceDefinition) error {
	if controllerutil.RemoveFinalizer(instance, revisionControllerFinalizer) {
		if err := r.Patch(ctx, instance, client.RawPatch(types.MergePatchType, []byte(`{"metadata":{"finalizers":[]}}`))); err != nil {
			return fmt.Errorf("failed to patch finalizer: %w", err)
		}
	}
	return nil
}

func extractGroupVersionKindFromCRD(crd apiextv1.CustomResourceDefinition) (schema.GroupVersionKind, error) {
	group := crd.Spec.Group
	kind := crd.Spec.Names.Kind
	var version string
	for _, v := range crd.Spec.Versions {
		if v.Storage {
			version = v.Name
			break
		}
	}
	if version == "" {
		return schema.GroupVersionKind{}, errors.New("CRD has no storage version")
	}
	return schema.GroupVersionKind{
		Group:   group,
		Version: version,
		Kind:    kind,
	}, nil
}

func (r *DynamicReconcilerManager) SetupWithManager(name string, mgr ctrl.Manager) error {
	r.manager = mgr
	return builder.ControllerManagedBy(mgr).
		For(&apiextv1.CustomResourceDefinition{}).
		Named(name).
		Complete(r)
}

func (r *DynamicReconcilerManager) ensureInstanceControllerFor(ctx context.Context, gvk schema.GroupVersionKind) error {
	key := controllersKey{
		Group: gvk.Group,
		Kind:  gvk.Kind,
	}

	r.controllersMux.Lock()
	defer r.controllersMux.Unlock()

	if _, ok := r.controllers[key]; ok {
		return nil
	}

	l := log.FromContext(ctx).WithName("RevisionManagerManager.ensureInstanceControllerFor").WithValues("gvk", gvk)
	l.Info("Creating reconcilers for resource")

	ctrls := make([]*instanceController, 0, len(r.ManagedReconcilers))
	for _, newReconciler := range r.ManagedReconcilers {
		instanceCtrlCtx, instanceCtrlCancel := context.WithCancel(r.ControllerLifetimeCtx)
		reconciler := newReconciler()

		dynCtrl, err := controller.NewTypedUnmanaged(
			"instance-controller-"+gvk.Group+"-"+gvk.Version+"-"+gvk.Kind,
			controller.TypedOptions[reconcile.Request]{
				// It's fine to re-use the same metric on CRD recreate
				SkipNameValidation: new(true),
				Reconciler:         reconciler,
				Logger:             r.manager.GetLogger(),
			})
		if err != nil {
			instanceCtrlCancel()
			return fmt.Errorf("failed to create dynamic controller: %w", err)
		}
		instanceCtrl := &instanceController{
			ctrl: dynCtrl,
			stop: instanceCtrlCancel,
			done: make(chan struct{}),
		}

		target := &unstructured.Unstructured{}
		target.SetGroupVersionKind(gvk)

		if err := reconciler.SetupDynamicControllerWithWatches(dynCtrl, r.manager, gvk); err != nil {
			instanceCtrlCancel()
			return fmt.Errorf("failed to setup dynamic controller with watches: %w", err)
		}

		go func() {
			err := dynCtrl.Start(instanceCtrlCtx)
			if err == nil {
				err = ErrStopped
			}
			instanceCtrl.startErr = err
			close(instanceCtrl.done)
		}()

		ctrls = append(ctrls, instanceCtrl)
	}

	if r.controllers == nil {
		r.controllers = make(map[controllersKey]stopper)
	}
	r.controllers[key] = stopper{ctrls: ctrls}
	return nil
}

func (r *DynamicReconcilerManager) stopAndRemoveControllerFor(gvk schema.GroupVersionKind) error {
	key := controllersKey{
		Group: gvk.Group,
		Kind:  gvk.Kind,
	}

	r.controllersMux.Lock()
	defer r.controllersMux.Unlock()
	_, ok := r.controllers[key]
	if !ok {
		return nil
	}

	rc, ok := r.controllers[key]
	if !ok {
		return nil
	}
	rc.stop()
	rc.wait()
	delete(r.controllers, key)

	var obj unstructured.Unstructured
	obj.SetGroupVersionKind(gvk)
	if err := r.manager.GetCache().RemoveInformer(context.TODO(), &obj); err != nil {
		return fmt.Errorf("failed to remove informer: %w", err)
	}

	return nil
}

type instanceController struct {
	ctrl controller.TypedController[reconcile.Request]
	stop func()

	// done is closed when the controller is stopped or fails to start.
	// Use [StartErr] instead of this field.
	// An error is stored in startErr if this channel is closed.
	done chan struct{}

	// startErr holds any error encountered during Start().
	// Use [StartErr] instead of this field.
	// If Start() returned nil, startErr will be [ErrStopped].
	// startErr is only valid after done is closed.
	startErr error
}

// ErrStopped is returned by [instanceController.StartErr] if the controller was stopped without error.
var ErrStopped = errors.New("controller stopped")

// StartErr returns any error encountered during Start() or [ErrStopped] if the controller has stopped without error.
// If the controller is still running, StartErr returns nil.
func (rc *instanceController) StartErr() error {
	select {
	case <-rc.done:
		// controller has stopped, return any start error
		return rc.startErr
	default:
		// controller is still running
		return nil
	}
}
