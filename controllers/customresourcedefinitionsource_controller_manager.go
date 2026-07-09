package controllers

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/Masterminds/semver/v3"
	imagereflectorv1 "github.com/fluxcd/image-reflector-controller/api/v1"
	sourcev1 "github.com/fluxcd/source-controller/api/v1"
	"helm.sh/helm/v4/pkg/chart/v2/loader"
	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	chrysopoeiav1 "github.com/helmetica-framework/chrysopoeia/api/v1"
	"github.com/helmetica-framework/chrysopoeia/pkg/breakagedetection"
	"github.com/helmetica-framework/chrysopoeia/pkg/schemagen"
)

type CustomResourceDefinitionSourceManager struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder events.EventRecorder

	// SourceControllerHostnameOverride is an optional hostname override for the source controller.
	// If set, it will be used to access the source controller instead of the hostname reported by the source controller.
	SourceControllerHostnameOverride string
	// ImageReflectorControllerHostname is the hostname used to access the image reflector controller to load tags for a OCI image.
	ImageReflectorControllerHostname string
}

//+kubebuilder:rbac:groups=chrysopoeia.io,resources=customresourcedefinitionsources,verbs=get;list;watch
//+kubebuilder:rbac:groups=chrysopoeia.io,resources=customresourcedefinitionsources/status,verbs=get;update;patch

//+kubebuilder:rbac:groups=apiextensions.k8s.io,resources=customresourcedefinitions,verbs=get;list;watch;create;update;patch

//+kubebuilder:rbac:groups=source.toolkit.fluxcd.io,resources=ocirepositories,verbs=get;list;watch
//+kubebuilder:rbac:groups=image.toolkit.fluxcd.io,resources=imagerepositories,verbs=get;list;watch

func (r *CustomResourceDefinitionSourceManager) Reconcile(ctx context.Context, req reconcile.Request) (res ctrl.Result, err error) {
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

	var statusUpdated bool
	statusCondition := metav1.Condition{
		Type:               "Ready",
		Status:             metav1.ConditionFalse,
		Reason:             "UnknownError",
		Message:            "CustomResourceDefinitionSource is not ready due to an unknown error",
		ObservedGeneration: source.Generation,
	}
	defer func() {
		if apimeta.SetStatusCondition(&source.Status.Conditions, statusCondition) || statusUpdated {
			if err := r.Status().Update(ctx, &source); err != nil {
				l.Error(err, "Failed to update CustomResourceDefinitionSource status")
				res = ctrl.Result{}
			}
		}
	}()

	var ociRepo sourcev1.OCIRepository
	if err := r.Get(ctx, client.ObjectKey{Namespace: source.Namespace, Name: source.Spec.Reference.Name}, &ociRepo); err != nil {
		l.Error(err, "Failed to get OCIRepository", "OCIRepository", source.Spec.Reference.Name)
		if apierrors.IsNotFound(err) {
			statusCondition.Reason = "OCIRepositoryNotFound"
			statusCondition.Message = fmt.Sprintf("OCIRepository %s not found", source.Spec.Reference.Name)
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	if !apimeta.IsStatusConditionTrue(ociRepo.Status.Conditions, sourcev1.ArtifactInStorageCondition) {
		l.Info("OCIRepository is not yet ready")
		statusCondition.Reason = "OCIRepositoryNotReady"
		statusCondition.Message = fmt.Sprintf("OCIRepository %s is not ready", source.Spec.Reference.Name)
		return ctrl.Result{}, nil
	}

	chartURL := ociRepo.Status.Artifact.URL
	if chartURL == "" {
		l.Info("OCIRepository does not have an artifact URL")
		statusCondition.Reason = "OCIRepositoryNoArtifactURL"
		statusCondition.Message = fmt.Sprintf("OCIRepository %s does not have an artifact URL", source.Spec.Reference.Name)
		return ctrl.Result{}, fmt.Errorf("OCIRepository does not have a valid artifact URL")
	}

	l.Info("OCIRepository is ready", "OCIRepository", source.Spec.Reference.Name, "ArtifactURL", chartURL)

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

	httpResp, err := http.Get(chartURL)
	if err != nil {
		l.Error(err, "Failed to fetch artifact", "ArtifactURL", chartURL)
		statusCondition.Reason = "ArtifactFetchFailed"
		statusCondition.Message = fmt.Sprintf("Failed to fetch artifact from %s: %v", chartURL, err)
		return ctrl.Result{}, err
	}
	defer httpResp.Body.Close()

	if httpResp.StatusCode != http.StatusOK {
		l.Error(nil, "Failed to fetch artifact", "ArtifactURL", chartURL, "StatusCode", httpResp.StatusCode)
		statusCondition.Reason = "ArtifactFetchFailed"
		statusCondition.Message = fmt.Sprintf("Failed to fetch artifact from %s: status code %d", chartURL, httpResp.StatusCode)
		return ctrl.Result{}, fmt.Errorf("failed to fetch artifact: %s", chartURL)
	}

	l.Info("Successfully requested artifact", "ArtifactURL", chartURL)

	chart, err := loader.LoadArchive(httpResp.Body)
	if err != nil {
		l.Error(err, "Failed to load chart", "ArtifactURL", chartURL)
		statusCondition.Reason = "ChartLoadFailed"
		statusCondition.Message = fmt.Sprintf("Failed to load chart from %s: %v", chartURL, err)
		return ctrl.Result{}, err
	}

	crd, err := schemagen.GenerateCRD(*chart,
		schemagen.WithGroup(fmt.Sprintf("%s.helmetica-bundles.io", source.Name)),
		schemagen.WithNames(source.Spec.CRDNames))
	if err != nil {
		l.Error(err, "Failed to generate CRD from chart", "ArtifactURL", chartURL)
		statusCondition.Reason = "CRDGenerationFailed"
		statusCondition.Message = fmt.Sprintf("Failed to generate CRD from chart %s: %v", chartURL, err)
		return ctrl.Result{}, err
	}

	l.Info("Successfully generated CRD from chart", "ArtifactURL", chartURL)

	if source.Spec.VersionDiscovery.Reference.Name != "" {
		l.Info("Tag discovery is enabled, fetching ImageRepository for tag discovery", "ImageRepository", source.Spec.VersionDiscovery.Reference.Name)
		var versionDiscoveryRepo imagereflectorv1.ImageRepository
		if err := r.Get(ctx, client.ObjectKey{Namespace: source.Namespace, Name: source.Spec.VersionDiscovery.Reference.Name}, &versionDiscoveryRepo); err != nil {
			l.Error(err, "Failed to get ImageRepository for tag discovery", "ImageRepository", source.Spec.VersionDiscovery.Reference.Name)
			if apierrors.IsNotFound(err) {
				statusCondition.Reason = "VersionDiscoveryImageRepositoryNotFound"
				statusCondition.Message = fmt.Sprintf("ImageRepository %s for tag discovery not found", source.Spec.VersionDiscovery.Reference.Name)
				return ctrl.Result{}, nil
			}
			return ctrl.Result{}, err
		}
		if !apimeta.IsStatusConditionTrue(versionDiscoveryRepo.Status.Conditions, "Ready") {
			l.Info("ImageRepository is not yet ready")
			statusCondition.Reason = "ImageRepositoryNotReady"
			statusCondition.Message = fmt.Sprintf("ImageRepository %s is not ready", source.Spec.VersionDiscovery.Reference.Name)
			return ctrl.Result{}, nil
		}

		l.Info("ImageRepository is ready", "ImageRepository", versionDiscoveryRepo.Name)

		url := new(url.URL)
		url.Scheme = "http"
		url.Host = r.ImageReflectorControllerHostname
		// TODO(bastjan): Test for gzipped file for tag discovery, and handle accordingly
		// The controller switches to a gzipped file if the uncompressed file is larger than a few KB.
		url.Path = fmt.Sprintf("/imagerepository/%s/%s/tags.txt", versionDiscoveryRepo.Namespace, versionDiscoveryRepo.Name)

		resp, err := http.Get(url.String())
		if err != nil {
			l.Error(err, "Failed to fetch tags from image reflector controller", "URL", url.String())
			statusCondition.Reason = "VersionDiscoveryFetchFailed"
			statusCondition.Message = fmt.Sprintf("Failed to fetch tags from image reflector controller: %v", err)
			return ctrl.Result{}, err
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			l.Error(nil, "Failed to fetch tags from image reflector controller", "URL", url.String(), "StatusCode", resp.StatusCode)
			statusCondition.Reason = "VersionDiscoveryFetchFailed"
			statusCondition.Message = fmt.Sprintf("Failed to fetch tags from image reflector controller: status code %d", resp.StatusCode)
			return ctrl.Result{}, fmt.Errorf("failed to fetch tags from image reflector controller: status code %d", resp.StatusCode)
		}

		tags, err := parseTagsTxt(resp.Body, strings.TrimPrefix(versionDiscoveryRepo.Status.LastScanResult.Revision, "sha256:"))
		if err != nil {
			l.Error(err, "Failed to parse tags from image reflector controller", "URL", url.String())
			statusCondition.Reason = "VersionDiscoveryParseFailed"
			statusCondition.Message = fmt.Sprintf("Failed to parse tags from image reflector controller: %v", err)
			return ctrl.Result{}, err
		}

		l.Info("Successfully fetched and parsed tags from image reflector controller", "URL", url.String(), "TagsCount", len(tags))

		chartVersion, err := semver.StrictNewVersion(strings.ReplaceAll(chart.Metadata.Version, "_", "+"))
		if err != nil {
			l.Error(err, "Failed to parse chart version", "ChartVersion", chart.Metadata.Version)
			statusCondition.Reason = "InvalidChartVersion"
			statusCondition.Message = fmt.Sprintf("Invalid strict chart version: %s", chart.Metadata.Version)
			return ctrl.Result{}, err
		}
		versionConstraint, err := semver.NewConstraint(fmt.Sprintf("<= %s, ~%d", chartVersion.String(), chartVersion.Major()))
		if err != nil {
			l.Error(err, "Cannot create semver constraint", "ChartVersion", chart.Metadata.Version)
			statusCondition.Reason = "InvalidChartVersion"
			statusCondition.Message = fmt.Sprintf("Invalid strict chart version: %s", chart.Metadata.Version)
			return ctrl.Result{}, err
		}

		if len(crd.Spec.Versions) != 1 {
			l.Error(nil, "CRD has more than one version, cannot apply version discovery", "CRDName", crd.Name)
			statusCondition.Reason = "CRDMultipleVersions"
			statusCondition.Message = fmt.Sprintf("CRD %s has more than one version, cannot apply version discovery", crd.Name)
			return ctrl.Result{}, fmt.Errorf("CRD has more than one version, cannot apply version discovery")
		}

		if spec, ok := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"]; ok {
			if ociRepoProp, ok := spec.Properties["ociUrl"]; ok {
				urlJSON, err := json.Marshal(ociRepo.Spec.URL)
				if err != nil {
					l.Error(err, "Failed to marshal OCIRepository URL to JSON", "OCIRepositoryURL", ociRepo.Spec.URL)
					return ctrl.Result{}, err
				}
				ociRepoProp.Default = &apiextv1.JSON{Raw: urlJSON}
				spec.Properties["ociUrl"] = ociRepoProp
			}
			if versionProp, ok := spec.Properties["version"]; ok {
				versionProp.Enum = make([]apiextv1.JSON, 0, len(tags))
				for _, tag := range tags {
					if !versionConstraint.Check(tag) {
						l.Info("Skipping tag that does not satisfy chart version constraint", "Tag", tag.String(), "ChartVersion", chart.Metadata.Version)
						continue
					}
					tagJSON, err := json.Marshal(tag)
					if err != nil {
						l.Error(err, "Failed to marshal tag to JSON", "Tag", tag.String())
						statusCondition.Reason = "VersionDiscoveryMarshalFailed"
						statusCondition.Message = fmt.Sprintf("Failed to marshal tag %s to JSON: %v", tag.String(), err)
						return ctrl.Result{}, err
					}
					versionProp.Enum = append(versionProp.Enum, apiextv1.JSON{Raw: tagJSON})
				}
				spec.Properties["version"] = versionProp
				crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"] = spec
			} else {
				l.Error(nil, "CRD spec does not have a version property, cannot apply version discovery", "CRDName", crd.Name)
				statusCondition.Reason = "CRDNoVersionProperty"
				statusCondition.Message = fmt.Sprintf("CRD %s spec does not have a version property, cannot apply version discovery", crd.Name)
				return ctrl.Result{}, fmt.Errorf("CRD spec does not have a version property, cannot apply version discovery")
			}
		} else {
			l.Error(nil, "CRD spec does not have a spec property, cannot apply version discovery", "CRDName", crd.Name)
			statusCondition.Reason = "CRDNoSpecProperty"
			statusCondition.Message = fmt.Sprintf("CRD %s does not have a spec property, cannot apply version discovery", crd.Name)
			return ctrl.Result{}, fmt.Errorf("CRD does not have a spec property, cannot apply version discovery")
		}

		l.Info("Successfully applied version discovery to CRD", "CRDName", crd.Name, "VersionsCount", len(tags))
	}

	if crd.Labels == nil {
		crd.Labels = make(map[string]string)
	}
	crd.Labels["chrysopoeia.io/managed"] = ""

	if crd.Annotations == nil {
		crd.Annotations = make(map[string]string)
	}
	crd.Annotations["chrysopoeia.io/artifact-ref-apiVersion"] = ociRepo.APIVersion
	crd.Annotations["chrysopoeia.io/artifact-ref-kind"] = ociRepo.Kind
	crd.Annotations["chrysopoeia.io/artifact-ref-name"] = ociRepo.Name
	crd.Annotations["chrysopoeia.io/artifact-ref-namespace"] = ociRepo.Namespace
	crd.Annotations["chrysopoeia.io/artifact-ref-generation"] = strconv.Itoa(int(ociRepo.Status.ObservedGeneration))
	crd.Annotations["chrysopoeia.io/artifact-ref-lastUpdateTime"] = ociRepo.Status.Artifact.LastUpdateTime.Format(time.RFC3339)
	crd.Annotations["chrysopoeia.io/artifact-ref-revision"] = ociRepo.Status.Artifact.Revision

	origCRD := apiextv1.CustomResourceDefinition{}
	if err := r.Get(ctx, client.ObjectKey{Name: crd.Name}, &origCRD); err != nil {
		if !apierrors.IsNotFound(err) {
			l.Error(err, "Failed to get existing CRD", "CRDName", crd.Name)
			statusCondition.Reason = "CRDFetchFailed"
			statusCondition.Message = fmt.Sprintf("Failed to fetch existing CRD %s: %v", crd.Name, err)
			return ctrl.Result{}, err
		}
	} else {
		if crd.Spec.Names.Singular == "" {
			crd.Spec.Names.Singular = origCRD.Spec.Names.Singular
		}
		if crd.Spec.Names.Plural == "" {
			crd.Spec.Names.Plural = origCRD.Spec.Names.Plural
		}
		if crd.Spec.Names.ListKind == "" {
			crd.Spec.Names.ListKind = origCRD.Spec.Names.ListKind
		}
		warnings, errors := breakagedetection.Check(origCRD, crd)
		if len(errors) > 0 {
			l.Error(nil, "Breaking changes detected in CRD update, refusing to apply", "CRDName", crd.Name, "Errors", errors)
			r.Recorder.Eventf(&origCRD, &source, "Warning", "BreakingChangesDetected", "Update", fmt.Sprintf("Breaking changes detected in CRD update: %s", strings.Join(errors, "; ")))
			statusCondition.Reason = "BreakingChangesDetected"
			statusCondition.Message = fmt.Sprintf("Breaking changes detected in CRD update: %s", strings.Join(errors, "; "))
			return ctrl.Result{}, fmt.Errorf("breaking changes detected in CRD update: %s", strings.Join(errors, "; "))
		}
		if len(warnings) > 0 {
			l.Info("Warnings detected in CRD update", "CRDName", crd.Name, "Warnings", warnings)
		}
	}

	u, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&crd)
	if err != nil {
		l.Error(err, "Failed to convert CRD to unstructured", "ArtifactURL", chartURL)
		return ctrl.Result{}, err
	}

	if err := r.Apply(ctx, client.ApplyConfigurationFromUnstructured(&unstructured.Unstructured{Object: u}), client.FieldOwner("chrysopoeia-controller")); err != nil {
		l.Error(err, "Failed to apply CRD", "ArtifactURL", chartURL)
		statusCondition.Reason = "CRDApplyFailed"
		statusCondition.Message = fmt.Sprintf("Failed to apply CRD from chart %s: %v", chartURL, err)
		return ctrl.Result{}, err
	}

	l.Info("Successfully applied CRD from chart", "ArtifactURL", chartURL)

	statusCondition.Reason = "CustomResourceDefinitionReady"
	statusCondition.Message = "CustomResourceDefinition is ready"
	statusCondition.Status = metav1.ConditionTrue

	if source.Status.AppliedReferenceGeneration != ociRepo.Status.ObservedGeneration || source.Status.AppliedReferenceRevision != ociRepo.Status.Artifact.Revision {
		source.Status.AppliedReferenceGeneration = ociRepo.Status.ObservedGeneration
		source.Status.AppliedReferenceRevision = ociRepo.Status.Artifact.Revision
		statusUpdated = true
	}

	l.Info("CustomResourceDefinitionSource reconciled successfully", "CRDName", crd.Name)

	return ctrl.Result{}, nil
}

const (
	sourceReferenceIndexField           = ".spec.reference.name"
	versionDiscoveryReferenceIndexField = ".spec.versionDiscovery.reference.name"
)

func (r *CustomResourceDefinitionSourceManager) SetupWithManager(name string, mgr ctrl.Manager) error {
	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &chrysopoeiav1.CustomResourceDefinitionSource{}, sourceReferenceIndexField, func(rawObj client.Object) []string {
		source := rawObj.(*chrysopoeiav1.CustomResourceDefinitionSource)
		if source.Spec.Reference.Name == "" {
			return nil
		}
		return []string{source.Spec.Reference.Name}
	}); err != nil {
		return fmt.Errorf("Failed to setup indexer for source reference: %w", err)
	}
	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &chrysopoeiav1.CustomResourceDefinitionSource{}, versionDiscoveryReferenceIndexField, func(rawObj client.Object) []string {
		source := rawObj.(*chrysopoeiav1.CustomResourceDefinitionSource)
		if source.Spec.VersionDiscovery.Reference.Name == "" {
			return nil
		}
		return []string{source.Spec.VersionDiscovery.Reference.Name}
	}); err != nil {
		return fmt.Errorf("Failed to setup indexer for version discovery reference: %w", err)
	}

	return builder.ControllerManagedBy(mgr).
		For(&chrysopoeiav1.CustomResourceDefinitionSource{}).
		Watches(&sourcev1.OCIRepository{}, handler.EnqueueRequestsFromMapFunc(ociRepositoryToCustomResourceDefinitionSourceMapFunc(mgr.GetClient()))).
		Watches(&imagereflectorv1.ImageRepository{}, handler.EnqueueRequestsFromMapFunc(imageRepositoryToCustomResourceDefinitionSourceMapFunc(mgr.GetClient()))).
		Named(name).
		Complete(r)
}

func ociRepositoryToCustomResourceDefinitionSourceMapFunc(c client.Client) func(ctx context.Context, o client.Object) []reconcile.Request {
	return func(ctx context.Context, o client.Object) []reconcile.Request {
		var crds chrysopoeiav1.CustomResourceDefinitionSourceList
		if err := c.List(ctx, &crds, client.InNamespace(o.GetNamespace()), client.MatchingFields{sourceReferenceIndexField: o.GetName()}); err != nil {
			log.FromContext(ctx).Error(err, "Failed to list CustomResourceDefinitionSources")
			return nil
		}
		requests := make([]reconcile.Request, len(crds.Items))
		for i, crd := range crds.Items {
			requests[i].Name = crd.Name
			requests[i].Namespace = crd.Namespace
		}
		return requests
	}
}

func imageRepositoryToCustomResourceDefinitionSourceMapFunc(c client.Client) func(ctx context.Context, o client.Object) []reconcile.Request {
	return func(ctx context.Context, o client.Object) []reconcile.Request {
		var crds chrysopoeiav1.CustomResourceDefinitionSourceList
		if err := c.List(ctx, &crds, client.InNamespace(o.GetNamespace()), client.MatchingFields{versionDiscoveryReferenceIndexField: o.GetName()}); err != nil {
			log.FromContext(ctx).Error(err, "Failed to list CustomResourceDefinitionSources")
			return nil
		}
		requests := make([]reconcile.Request, len(crds.Items))
		for i, crd := range crds.Items {
			requests[i].Name = crd.Name
			requests[i].Namespace = crd.Namespace
		}
		return requests
	}
}

func parseTagsTxt(r io.Reader, expectedSHA256 string) ([]*semver.Version, error) {
	sha256 := sha256.New()

	br := bufio.NewReader(io.TeeReader(r, sha256))
	var tags []*semver.Version

	more := true
	for more {
		line, err := br.ReadString('\n')
		if err != nil && err != io.EOF {
			return nil, err
		}
		if err == io.EOF {
			more = false
		}
		ver, err := semver.StrictNewVersion(strings.TrimSpace(line))
		if err != nil {
			continue // Ignore invalid semver tags
		}
		tags = append(tags, ver)
	}

	slices.SortFunc(tags, func(a, b *semver.Version) int {
		return b.Compare(a)
	})

	actualSHA256 := fmt.Sprintf("%x", sha256.Sum(nil))
	if actualSHA256 != expectedSHA256 {
		return nil, fmt.Errorf("SHA256 mismatch: expected %s, got %s", expectedSHA256, actualSHA256)
	}

	return tags, nil
}
