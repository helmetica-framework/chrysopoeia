package controllers

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"

	chrysopoeiav1 "github.com/helmetica-framework/chrysopoeia/api/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

type InstanceManager struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder events.EventRecorder
}

//+kubebuilder:rbac:groups=apiextensions.k8s.io,resources=customresourcedefinitions,verbs=get;list;watch

func (r *InstanceManager) Reconcile(ctx context.Context, req InstanceRequest) (res ctrl.Result, err error) {
	l := log.FromContext(ctx).WithName("InstanceManager.Reconcile").WithValues("request", req)
	l.Info("Reconciling Instance")

	var instance unstructured.Unstructured
	instance.SetAPIVersion(req.APIVersion)
	instance.SetKind(req.Kind)
	if err := r.Get(ctx, req.NamespacedName, &instance); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
	if !instance.GetDeletionTimestamp().IsZero() {
		return ctrl.Result{}, nil
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

	shaSum := sha256.New()

	if _, err := shaSum.Write([]byte(version)); err != nil {
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
	rev.Spec.Version = version
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

	return ctrl.Result{}, nil
}

type InstanceRequest struct {
	APIVersion string
	Kind       string

	types.NamespacedName
}
