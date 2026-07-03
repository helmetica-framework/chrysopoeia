package controllers

import (
	"context"
	"errors"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	sourcev1 "github.com/fluxcd/source-controller/api/v1"
	chrysopoeiav1 "github.com/helmetica-framework/chrysopoeia/api/v1"
)

type BundleSourceManager struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder events.EventRecorder
}

//+kubebuilder:rbac:groups=chrysopoeia.io,resources=bundlesources,verbs=get;list;watch

func (r *BundleSourceManager) Reconcile(ctx context.Context, req reconcile.Request) (ctrl.Result, error) {
	l := log.FromContext(ctx).WithName("BundleSourceManager.Reconcile")
	l.Info("Reconciling BundleSource")

	var source chrysopoeiav1.BundleSource
	if err := r.Get(ctx, req.NamespacedName, &source); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
	if !source.DeletionTimestamp.IsZero() {
		l.Info("BundleSource is being deleted, stopping controller")
		return ctrl.Result{}, nil
	}

	var errs []error
	for _, majorVersion := range source.Spec.MajorVersions {
		var sourceRef sourcev1.OCIRepository
		sourceRef.SetGroupVersionKind(sourcev1.GroupVersion.WithKind("OCIRepository"))

		sourceRef.Namespace = source.Namespace
		sourceRef.Name = fmt.Sprintf("%s-%d", source.Name, majorVersion)

		sourceRef.Spec.URL = source.Spec.OCIURI
		sourceRef.Spec.Reference = &sourcev1.OCIRepositoryRef{
			SemVer: fmt.Sprintf("%d.x", majorVersion),
		}

		if err := controllerutil.SetControllerReference(&source, &sourceRef, r.Scheme); err != nil {
			errs = append(errs, fmt.Errorf("unable to set controller reference for OCIRepository %s: %w", sourceRef.Name, err))
			continue
		}

		u, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&sourceRef)
		if err != nil {
			errs = append(errs, fmt.Errorf("unable to convert OCIRepository %s to unstructured: %w", sourceRef.Name, err))
			continue
		}

		if err := r.Apply(ctx, client.ApplyConfigurationFromUnstructured(&unstructured.Unstructured{Object: u}), client.FieldOwner("bundle-source-manager")); err != nil {
			errs = append(errs, fmt.Errorf("unable to apply OCIRepository %s: %w", sourceRef.Name, err))
			continue
		}
	}
	if err := errors.Join(errs...); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

func (r *BundleSourceManager) SetupWithManager(name string, mgr ctrl.Manager) error {
	return builder.ControllerManagedBy(mgr).
		For(&chrysopoeiav1.BundleSource{}).
		Owns(&sourcev1.OCIRepository{}).
		Named(name).
		Complete(r)
}
