package controllers

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"time"

	sourcev1 "github.com/fluxcd/source-controller/api/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	metav1ac "k8s.io/client-go/applyconfigurations/meta/v1"
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
	chrysopoeiav1ac "github.com/helmetica-framework/chrysopoeia/api/v1/applyconfiguration/api/v1"
	"github.com/helmetica-framework/chrysopoeia/pkg/celvalues"
)

type RevisionManager struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder events.EventRecorder

	// GVK is the GroupVersionKind of the resource that this controller manages.
	// The controller is dynamically created for each GVK that is registered with the RevisionManagerManager.
	GVK schema.GroupVersionKind

	// SourceControllerHostnameOverride is an optional hostname override for the source controller,
	// used when fetching the chart whose cel: expressions resolve a claim's values.
	SourceControllerHostnameOverride string
}

// valuesResolvedCondition reports whether a claim's values could be resolved, meaning every cel:
// expression of its chart evaluated. It is deliberately not `Ready`: this controller cannot know
// whether the release is ready, which is the release controller's business.
const valuesResolvedCondition = "ValuesResolved"

//+kubebuilder:rbac:groups=helmetica.io,resources=instancerevisions,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=helmetica.io,resources=instancerevisions/status,verbs=get;update;patch

//+kubebuilder:rbac:groups=source.toolkit.fluxcd.io,resources=ocirepositories,verbs=get;list;watch;create;update;patch;delete

// NewRevisionManager returns a factory for RevisionManagers, one per managed GVK.
func NewRevisionManager(sourceControllerHostnameOverride string) func() DynamicReconciler {
	return func() DynamicReconciler {
		return &RevisionManager{SourceControllerHostnameOverride: sourceControllerHostnameOverride}
	}
}

// resolveVersion is the version a revision is built from: what the user pinned in spec.version,
// then what the framework selected in status.version, then what the instance is already running.
// It reports "" when none of them decide, which only a claim with no revision yet does; the caller
// falls back to the newest version the schema allows.
//
// current is what keeps a managed instance still. Without it, an edit to spec.values would rebuild
// the instance against whatever is newest at that moment, carrying an upgrade nobody asked for.
// With it, only a write to status.version moves an instance, and only adept writes that.
func resolveVersion(claim unstructured.Unstructured, current string) (string, error) {
	specv, _, err := unstructured.NestedString(claim.Object, "spec", "version")
	if err != nil {
		return "", fmt.Errorf("reading spec.version for %s/%s: %w", claim.GetNamespace(), claim.GetName(), err)
	}

	if specv != "" {
		return specv, nil
	}

	statusv, _, err := unstructured.NestedString(claim.Object, "status", "version")
	if err != nil {
		return "", fmt.Errorf("reading status.version for %s/%s: %w", claim.GetNamespace(), claim.GetName(), err)
	}

	if statusv != "" {
		return statusv, nil
	}

	return current, nil
}

// versionWithoutDigest is the version part of a revision's "version@digest".
func versionWithoutDigest(revisionVersion string) string {
	version, _, _ := strings.Cut(revisionVersion, "@")
	return version
}

// newestVersion is the newest version a claim kind allows: the first entry of the spec.version enum
// the CustomResourceDefinitionSource controller writes onto the generated CRD, which discovery sorts
// newest first. The CRD is matched rather than named, since the name is plural.group and only the
// CRD knows its own plural.
func newestVersion(crds []apiextensionsv1.CustomResourceDefinition, gvk schema.GroupVersionKind) (string, error) {
	c := slices.IndexFunc(crds, func(crd apiextensionsv1.CustomResourceDefinition) bool {
		return crd.Spec.Group == gvk.Group && crd.Spec.Names.Kind == gvk.Kind
	})
	if c < 0 {
		return "", fmt.Errorf("no CustomResourceDefinition for %s in %s", gvk.Kind, gvk.Group)
	}

	v := slices.IndexFunc(crds[c].Spec.Versions, func(version apiextensionsv1.CustomResourceDefinitionVersion) bool {
		return version.Name == gvk.Version
	})
	if v < 0 {
		return "", fmt.Errorf("%s has no version %s", crds[c].GetName(), gvk.Version)
	}

	props := crds[c].Spec.Versions[v].Schema
	if props == nil || props.OpenAPIV3Schema == nil {
		return "", fmt.Errorf("%s %s has no schema", gvk.Kind, gvk.Version)
	}

	spec, ok := props.OpenAPIV3Schema.Properties["spec"]
	if !ok {
		return "", fmt.Errorf("%s has no spec in its schema", gvk.Kind)
	}

	version, ok := spec.Properties["version"]
	if !ok || len(version.Enum) == 0 {
		return "", fmt.Errorf("%s has no versions to select from", gvk.Kind)
	}

	// The entries are raw JSON, so string(Raw) would keep the quotes.
	var newest string
	if err := json.Unmarshal(version.Enum[0].Raw, &newest); err != nil {
		return "", fmt.Errorf("reading the newest version of %s: %w", gvk.Kind, err)
	}

	return newest, nil
}

func (r *RevisionManager) currentVersion(ctx context.Context, claim unstructured.Unstructured) (string, error) {
	name, _, err := unstructured.NestedString(claim.Object, "status", "latestRevision")
	if err != nil {
		return "", fmt.Errorf("reading status.latestRevision: %w", err)
	}
	if name == "" {
		return "", nil
	}

	var rev chrysopoeiav1.InstanceRevision
	if err := r.Get(ctx, client.ObjectKey{Namespace: claim.GetNamespace(), Name: name}, &rev); err != nil {
		if apierrors.IsNotFound(err) {
			return "", nil
		}
		return "", fmt.Errorf("getting revision %s: %w", name, err)
	}

	return versionWithoutDigest(rev.Spec.Version), nil
}

func (r *RevisionManager) Reconcile(ctx context.Context, req reconcile.Request) (res ctrl.Result, err error) {
	l := log.FromContext(ctx).WithName("RevisionManager.Reconcile").WithValues("request", req)
	l.Info("Reconciling Claim")

	var claim unstructured.Unstructured
	claim.SetAPIVersion(r.GVK.GroupVersion().String())
	claim.SetKind(r.GVK.Kind)
	if err := r.Get(ctx, req.NamespacedName, &claim); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
	if !claim.GetDeletionTimestamp().IsZero() {
		return ctrl.Result{}, nil
	}

	// Everything this reconcile learns about the claim is recorded on the in-memory object and
	// written once, at the end or on the way out. NestedMap returns a deep copy, so this snapshot
	// stays put while the object below is edited.
	statusBefore, _, err := unstructured.NestedMap(claim.Object, "status")
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to read the status of the claim: %w", err)
	}

	ociUrl, _, err := unstructured.NestedString(claim.Object, "spec", "ociUrl")
	if err != nil {
		l.Error(err, "Failed to get ociUrl from instance")
		return ctrl.Result{}, err
	}

	current, err := r.currentVersion(ctx, claim)
	if err != nil {
		l.Error(err, "Failed to get the version the instance is running")
		return ctrl.Result{}, err
	}

	version, err := resolveVersion(claim, current)
	if err != nil {
		l.Error(err, "Failed to resolve the version from the instance")
		return ctrl.Result{}, err
	}

	if version == "" {
		var crds apiextensionsv1.CustomResourceDefinitionList
		if err := r.List(ctx, &crds); err != nil {
			l.Error(err, "Failed to list CustomResourceDefinitions")
			return ctrl.Result{}, err
		}

		version, err = newestVersion(crds.Items, claim.GroupVersionKind())
		if err != nil {
			l.Error(err, "Failed to get the newest version the instance may run")
			return ctrl.Result{}, err
		}
	}

	values, _, err := unstructured.NestedMap(claim.Object, "spec", "values")
	if err != nil {
		l.Error(err, "Failed to get values from instance")
		return ctrl.Result{}, err
	}

	ociRepo, err := r.ensureOCIRepository(ctx, claim, ociUrl, version)
	if err != nil {
		l.Error(err, "Failed to ensure OCIRepository")
		return ctrl.Result{}, err
	}
	if !apimeta.IsStatusConditionTrue(ociRepo.Status.Conditions, "Ready") {
		l.Info("OCIRepository not yet ready, requeuing")
		return ctrl.Result{}, nil
	}

	versionWithDigest := ociRepo.Status.Artifact.Revision

	preprocessor, err := r.preprocessorFor(ctx, ociRepo)
	if err != nil {
		return ctrl.Result{}, err
	}

	values, err = preprocessor.Apply(values, celvalues.Claim{
		Name:        claim.GetName(),
		Namespace:   claim.GetNamespace(),
		Labels:      claim.GetLabels(),
		Annotations: claim.GetAnnotations(),
	})
	if err != nil {
		l.Error(err, "Failed to resolve the values of the claim")
		if cerr := recordValuesResolved(&claim, err); cerr != nil {
			return ctrl.Result{}, errors.Join(err, cerr)
		}
		if ferr := r.flushClaimStatus(ctx, &claim, statusBefore); ferr != nil {
			return ctrl.Result{}, errors.Join(err, ferr)
		}
		return ctrl.Result{}, err
	}
	if err := recordValuesResolved(&claim, nil); err != nil {
		return ctrl.Result{}, err
	}

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

	revName := strings.Join([]string{claim.GetName(), fmt.Sprintf("%x", shaSum.Sum(nil))}, "-")
	rev := chrysopoeiav1ac.
		InstanceRevision(
			revName,
			claim.GetNamespace(),
		).
		WithOwnerReferences(
			metav1ac.OwnerReference().
				WithAPIVersion(claim.GetAPIVersion()).
				WithKind(claim.GetKind()).
				WithName(claim.GetName()).
				WithUID(claim.GetUID()).
				WithController(true),
		).
		WithSpec(
			chrysopoeiav1ac.
				InstanceRevisionSpec().
				WithVersion(versionWithDigest).
				WithOCIUrl(ociUrl),
		)

	if values != nil {
		valuesRaw, err := json.Marshal(values)
		if err != nil {
			l.Error(err, "Failed to marshal values")
			return ctrl.Result{}, err
		}
		rev.Spec = rev.Spec.WithValues(apiextensionsv1.JSON{Raw: valuesRaw})
	}

	if err := r.Apply(ctx, rev, client.FieldOwner("chrysopoeia-controller")); err != nil {
		l.Error(err, "Failed to apply instance revision")
		return ctrl.Result{}, err
	}

	if err := unstructured.SetNestedField(claim.Object, revName, "status", "latestRevision"); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to set latestRevision in instance status: %w", err)
	}
	if err := r.flushClaimStatus(ctx, &claim, statusBefore); err != nil {
		l.Error(err, "Failed to update instance status")
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

func (r *RevisionManager) ControllerName() string {
	return "revision-controller"
}

func (r *RevisionManager) SetupDynamicControllerWithWatches(dynCtrl controller.TypedController[reconcile.Request], mgr ctrl.Manager, gvk schema.GroupVersionKind) error {
	r.Client = mgr.GetClient()
	r.Scheme = mgr.GetScheme()
	r.Recorder = mgr.GetEventRecorder(fmt.Sprintf("%s-%s-%s-%s", r.ControllerName(), gvk.Group, gvk.Version, gvk.Kind))
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

// preprocessorFor returns the compiled cel: expressions of the chart behind an artifact.
//
// The chart is fetched and compiled on every reconcile. Caching it by
// ociRepo.Status.Artifact.Revision, which carries the digest and so changes whenever the chart
// does, is the upgrade if that ever shows up in a profile.
func (r *RevisionManager) preprocessorFor(ctx context.Context, ociRepo *sourcev1.OCIRepository) (*celvalues.Preprocessor, error) {
	chart, err := fetchChart(ctx, ociRepo.Status.Artifact.URL, r.SourceControllerHostnameOverride)
	if err != nil {
		return nil, fmt.Errorf("fetching chart: %w", err)
	}

	return celvalues.NewFromChart(chart)
}

// setClaimCondition sets cond on the claim's status, replacing an existing condition of the same
// type and stamping the claim's current generation into ObservedGeneration.
//
// It edits the object in memory only; the caller writes it back. Only status.conditions is
// touched, so the fields the release controller owns survive. A status.conditions that is present
// but malformed is an error rather than a silent reset: overwriting it would drop conditions
// another controller wrote.
func setClaimCondition(claim *unstructured.Unstructured, cond metav1.Condition) error {
	raw, found, err := unstructured.NestedFieldNoCopy(claim.Object, "status", "conditions")
	if err != nil {
		return fmt.Errorf("failed to read the conditions of the claim: %w", err)
	}

	var conditions []metav1.Condition
	if found {
		list, ok := raw.([]any)
		if !ok {
			return fmt.Errorf("the claim's status.conditions is a %T, not a list", raw)
		}
		for i, item := range list {
			object, ok := item.(map[string]any)
			if !ok {
				return fmt.Errorf("the claim's status.conditions[%d] is a %T, not an object", i, item)
			}
			var existing metav1.Condition
			if err := runtime.DefaultUnstructuredConverter.FromUnstructured(object, &existing); err != nil {
				return fmt.Errorf("failed to read the claim's status.conditions[%d]: %w", i, err)
			}
			conditions = append(conditions, existing)
		}
	}

	cond.ObservedGeneration = claim.GetGeneration()

	apimeta.SetStatusCondition(&conditions, cond)

	updated := make([]any, 0, len(conditions))
	for i := range conditions {
		object, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&conditions[i])
		if err != nil {
			return fmt.Errorf("failed to write the condition %q of the claim: %w", conditions[i].Type, err)
		}
		updated = append(updated, object)
	}

	return unstructured.SetNestedSlice(claim.Object, updated, "status", "conditions")
}

// recordValuesResolved records on the claim whether its values could be resolved. A nil failure
// means they were.
//
// The claim is the chart author's and the user's only view of what went wrong: without this, a
// broken expression leaves them with no InstanceRevision and no reason for it.
func recordValuesResolved(claim *unstructured.Unstructured, failure error) error {
	condition := metav1.Condition{
		Type:   valuesResolvedCondition,
		Status: metav1.ConditionTrue,
		Reason: "ValuesResolved",
	}
	if failure != nil {
		condition.Status = metav1.ConditionFalse
		condition.Reason = "ValuesPreprocessingFailed"
		condition.Message = failure.Error()
	}

	if err := setClaimCondition(claim, condition); err != nil {
		return fmt.Errorf("failed to set the %s condition: %w", valuesResolvedCondition, err)
	}
	return nil
}

// flushClaimStatus writes the claim's status if this reconcile changed it, and does nothing if it
// did not.
func (r *RevisionManager) flushClaimStatus(ctx context.Context, claim *unstructured.Unstructured, before map[string]any) error {
	after, _, err := unstructured.NestedMap(claim.Object, "status")
	if err != nil {
		return fmt.Errorf("failed to read the status of the claim: %w", err)
	}
	if reflect.DeepEqual(before, after) {
		return nil
	}
	return r.Status().Update(ctx, claim)
}
