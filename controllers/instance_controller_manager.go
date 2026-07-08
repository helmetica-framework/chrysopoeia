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
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/source"
)

type InstanceManagerManager struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder events.EventRecorder

	ControllerLifetimeCtx context.Context

	manager ctrl.Manager

	// controllersMux protects the controllers map.
	// While this controller does not access the map concurrently the metrics collector does.
	controllersMux sync.Mutex
	// controllers holds the actual reconcilers, one for each managed resource.
	controllers map[controllersKey]*instanceController
}

type controllersKey struct {
	APIVersion string
	Kind       string
}

//+kubebuilder:rbac:groups=apiextensions.k8s.io,resources=customresourcedefinitions,verbs=get;list;watch

func (r *InstanceManagerManager) Reconcile(ctx context.Context, req ctrl.Request) (res ctrl.Result, err error) {
	l := log.FromContext(ctx).WithName("InstanceManagerManager.Reconcile")
	l.Info("Reconciling Instance")

	var instance apiextv1.CustomResourceDefinition
	if err := r.Get(ctx, req.NamespacedName, &instance); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
	if !instance.GetDeletionTimestamp().IsZero() {
		// BIG FAT TODO: Shutdown controller for this resource if it exists.
		l.Info("Instance is being deleted, stopping controller")
		return ctrl.Result{}, nil
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

	for _, version := range instance.Spec.Versions {
		if !version.Served {
			continue
		}
		r.ensureInstanceControllerFor(ctx, instance.Spec.Group, version.Name, instance.Spec.Names.Kind)
	}

	l.Info("Finished reconciling")

	return ctrl.Result{}, nil
}

func (r *InstanceManagerManager) SetupWithManager(name string, mgr ctrl.Manager) error {
	r.manager = mgr
	return builder.ControllerManagedBy(mgr).
		For(&apiextv1.CustomResourceDefinition{}).
		Named(name).
		Complete(r)
}

func (r *InstanceManagerManager) ensureInstanceControllerFor(ctx context.Context, group, version, kind string) error {
	key := controllersKey{
		APIVersion: group + "/" + version,
		Kind:       kind,
	}

	r.controllersMux.Lock()
	defer r.controllersMux.Unlock()

	if _, ok := r.controllers[key]; ok {
		return nil
	}

	l := log.FromContext(ctx).WithName("InstanceManagerManager.ensureInstanceControllerFor").WithValues("group", group, "version", version, "kind", kind)
	l.Info("Creating new controller for resource")

	instanceCtrlCtx, instanceCtrlCancel := context.WithCancel(r.ControllerLifetimeCtx)
	reconciler := &InstanceManager{
		Client:   r.Client,
		Scheme:   r.Scheme,
		Recorder: r.Recorder,
	}
	dynCtrl, err := controller.NewTypedUnmanaged(
		"instance-controller-"+group+"-"+version+"-"+kind,
		controller.TypedOptions[InstanceRequest]{
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
		ctrl:       dynCtrl,
		stop:       instanceCtrlCancel,
		reconciler: reconciler,
		done:       make(chan struct{}),
	}

	target := &unstructured.Unstructured{}
	target.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   group,
		Version: version,
		Kind:    kind,
	})

	if err := dynCtrl.Watch(source.TypedKind(r.manager.GetCache(), client.Object(target), handler.TypedEnqueueRequestsFromMapFunc(func(_ context.Context, o client.Object) []InstanceRequest {
		return []InstanceRequest{{
			APIVersion: group + "/" + version,
			Kind:       kind,
			NamespacedName: types.NamespacedName{
				Name:      o.GetName(),
				Namespace: o.GetNamespace(),
			},
		}}
	}))); err != nil {
		instanceCtrlCancel()
		return fmt.Errorf("failed to watch target resource: %w", err)
	}

	go func() {
		err := dynCtrl.Start(instanceCtrlCtx)
		if err == nil {
			err = ErrStopped
		}
		instanceCtrl.startErr = err
		close(instanceCtrl.done)
	}()

	if r.controllers == nil {
		r.controllers = make(map[controllersKey]*instanceController)
	}
	r.controllers[key] = instanceCtrl
	return nil
}

func (r *InstanceManagerManager) stopAndRemoveControllerFor(key controllersKey) error {
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
	delete(r.controllers, key)
	return nil
}

type instanceController struct {
	ctrl       controller.TypedController[InstanceRequest]
	reconciler *InstanceManager
	stop       func()

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
