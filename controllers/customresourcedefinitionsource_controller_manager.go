package controllers

import (
	"context"
	"net/http"
	"net/url"

	"helm.sh/helm/v4/pkg/chart/v2/loader"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	sourcev1 "github.com/fluxcd/source-controller/api/v1"
	chrysopoeiav1 "github.com/helmetica-framework/chrysopoeia/api/v1"
	"github.com/helmetica-framework/chrysopoeia/pkg/schemagen"
)

type CustomResourceDefinitionSourceManager struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder events.EventRecorder

	// SourceControllerHostnameOverride is an optional hostname override for the source controller.
	// If set, it will be used to access the source controller instead of the hostname reported by the source controller.
	SourceControllerHostnameOverride string
}

//+kubebuilder:rbac:groups=chrysopoeia.io,resources=customresourcedefinitionsources,verbs=get;list;watch

func (r *CustomResourceDefinitionSourceManager) Reconcile(ctx context.Context, req reconcile.Request) (ctrl.Result, error) {
	l := log.FromContext(ctx).WithName("CustomResourceDefinitionSourceManager.Reconcile")
	l.Info("Reconciling CustomResourceDefinitionSource")

	var source chrysopoeiav1.CustomResourceDefinitionSource
	if err := r.Get(ctx, req.NamespacedName, &source); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
	if !source.DeletionTimestamp.IsZero() {
		l.Info("CustomResourceDefinitionSource is being deleted, stopping controller")
		return ctrl.Result{}, nil
	}

	var ociRepo sourcev1.OCIRepository
	if err := r.Get(ctx, client.ObjectKey{Namespace: source.Namespace, Name: source.Spec.Reference.Name}, &ociRepo); err != nil {
		l.Error(err, "Failed to get OCIRepository", "OCIRepository", source.Spec.Reference.Name)
		return ctrl.Result{}, err
	}

	if !apimeta.IsStatusConditionTrue(ociRepo.Status.Conditions, sourcev1.ArtifactInStorageCondition) {
		l.Info("OCIRepository is not yet ready")
		return ctrl.Result{}, nil
	}

	chartURL := ociRepo.Status.Artifact.URL
	if chartURL == "" {
		l.Info("OCIRepository does not have a valid artifact URL")
		return ctrl.Result{}, nil
	}

	if r.SourceControllerHostnameOverride != "" {
		l.Info("Overriding source controller hostname", "OriginalURL", chartURL, "Override", r.SourceControllerHostnameOverride)
		parsedURL, err := url.Parse(chartURL)
		if err != nil {
			l.Error(err, "Failed to parse chart URL", "ArtifactURL", chartURL)
			return ctrl.Result{}, err
		}
		parsedURL.Host = r.SourceControllerHostnameOverride
		chartURL = parsedURL.String()
	}

	l.Info("CustomResourceDefinitionSource is ready", "OCIRepository", source.Spec.Reference.Name, "ArtifactURL", chartURL)

	res, err := http.Get(chartURL)
	if err != nil {
		l.Error(err, "Failed to fetch artifact", "ArtifactURL", chartURL)
		return ctrl.Result{}, err
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		l.Error(nil, "Failed to fetch artifact", "ArtifactURL", chartURL, "StatusCode", res.StatusCode)
		return ctrl.Result{}, nil
	}

	l.Info("Successfully requested artifact", "ArtifactURL", chartURL)

	chart, err := loader.LoadArchive(res.Body)
	if err != nil {
		l.Error(err, "Failed to load chart", "ArtifactURL", chartURL)
		return ctrl.Result{}, err
	}

	crd, err := schemagen.GenerateCRD(*chart)
	if err != nil {
		l.Error(err, "Failed to generate CRD from chart", "ArtifactURL", chartURL)
		return ctrl.Result{}, err
	}

	l.Info("Successfully generated CRD from chart", "ArtifactURL", chartURL)

	u, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&crd)
	if err != nil {
		l.Error(err, "Failed to convert CRD to unstructured", "ArtifactURL", chartURL)
		return ctrl.Result{}, err
	}

	if err := r.Apply(ctx, client.ApplyConfigurationFromUnstructured(&unstructured.Unstructured{Object: u}), client.FieldOwner("chrysopoeia-controller")); err != nil {
		l.Error(err, "Failed to apply CRD", "ArtifactURL", chartURL)
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

func (r *CustomResourceDefinitionSourceManager) SetupWithManager(name string, mgr ctrl.Manager) error {
	return builder.ControllerManagedBy(mgr).
		For(&chrysopoeiav1.CustomResourceDefinitionSource{}).
		Watches(&sourcev1.OCIRepository{}, handler.EnqueueRequestsFromMapFunc(ociRepositoryToCustomResourceDefinitionSourceMapFunc(mgr.GetClient()))).
		Named(name).
		Complete(r)
}

func ociRepositoryToCustomResourceDefinitionSourceMapFunc(c client.Client) func(ctx context.Context, o client.Object) []reconcile.Request {
	return func(ctx context.Context, o client.Object) []reconcile.Request {
		var crds chrysopoeiav1.CustomResourceDefinitionSourceList
		if err := c.List(ctx, &crds, client.InNamespace(o.GetNamespace())); err != nil {
			log.FromContext(ctx).Error(err, "Failed to list CustomResourceDefinitionSources")
			return nil
		}
		var requests []reconcile.Request
		for _, crd := range crds.Items {
			if crd.Spec.Reference.Name == o.GetName() {
				requests = append(requests, reconcile.Request{
					NamespacedName: client.ObjectKey{
						Name:      crd.Name,
						Namespace: crd.Namespace,
					},
				})
			}
		}
		return requests
	}
}
