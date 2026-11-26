/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	homunculusv1 "github.com/helmetica-framework/chrysopoeia/test/e2e/testcontroller/api/v1"
	homunculusv1ac "github.com/helmetica-framework/chrysopoeia/test/e2e/testcontroller/api/v1/applyconfiguration/api/v1"
)

const Finalizer = "homunculus.helmetica.io/cleanup"

// TestReconciler reconciles a Test object
type TestReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	ControllerId string
}

// +kubebuilder:rbac:groups=homunculus.helmetica.io,resources=tests,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=homunculus.helmetica.io,resources=tests/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=homunculus.helmetica.io,resources=tests/finalizers,verbs=update

func (r *TestReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	l := logf.FromContext(ctx).WithName("TestReconciler.Reconcile")
	l.Info("Reconciling Test")

	var ti homunculusv1.Test
	if err := r.Get(ctx, req.NamespacedName, &ti); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("failed to get Test: %w", err)
	}
	if ti.DeletionTimestamp != nil && !ti.DeletionTimestamp.IsZero() {
		time.Sleep(time.Second) // The home office work simulation :D
		if err := r.Apply(ctx, acFromInstance(ti).WithFinalizers(), client.FieldOwner("test-finalizer-manager")); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to patch resource status: %w", err)
		}
		return ctrl.Result{}, nil
	}

	if err := r.Apply(ctx, acFromInstance(ti).WithFinalizers(Finalizer), client.FieldOwner("test-finalizer-manager")); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to patch resource status: %w", err)
	}

	if err := r.Status().Apply(ctx, acFromInstance(ti).WithStatus(homunculusv1ac.TestStatus().WithControllerID(r.ControllerId)), client.FieldOwner("test-status-manager")); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to patch resource status: %w", err)
	}

	return ctrl.Result{}, nil
}

// acFromInstance returns an applyconfiguration from the given instance.
// It sets the UID field to ensure the instance is not accidentally recreate because the cache has not yet caught up.
func acFromInstance(i homunculusv1.Test) *homunculusv1ac.TestApplyConfiguration {
	return homunculusv1ac.Test(i.Name, i.Namespace).WithUID(i.UID)
}

// SetupWithManager sets up the controller with the Manager.
func (r *TestReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&homunculusv1.Test{}).
		Named("test").
		Complete(r)
}
