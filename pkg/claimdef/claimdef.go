package claimdef

import (
	"context"
	"fmt"

	imagereflectorv1 "github.com/fluxcd/image-reflector-controller/api/v1"
	fluxmeta "github.com/fluxcd/pkg/apis/meta"
	sourcev1 "github.com/fluxcd/source-controller/api/v1"
	corev1 "k8s.io/api/core/v1"
	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	chrysopoeiav1 "github.com/helmetica-framework/chrysopoeia/api/v1"
)

type ClaimSource struct {
	ClaimDefinition         chrysopoeiav1.ClaimDefinition
	OCIRepository           sourcev1.OCIRepository
	OCIRepositoryCertSecret *corev1.Secret
	ImageRepository         *imagereflectorv1.ImageRepository
}

// FindDefinitionForClaim finds the [chrysopoeiav1.ClaimDefinition] and related objects for a claim.
// The claim needs the GVK filled.
func FindDefinitionForClaim(ctx context.Context, c client.Client, claim client.Object) (ClaimSource, error) {
	gvk := claim.GetObjectKind().GroupVersionKind()
	if (gvk == schema.GroupVersionKind{}) {
		return ClaimSource{}, fmt.Errorf("missing GVK for claim")
	}
	gvr, err := c.RESTMapper().RESTMapping(gvk.GroupKind(), gvk.Version)
	if err != nil {
		return ClaimSource{}, fmt.Errorf("unable to map GVK to GVR: %w", err)
	}
	var crd apiextv1.CustomResourceDefinition
	if err := c.Get(ctx, client.ObjectKey{Name: fmt.Sprintf("%s.%s", gvr.Resource.Resource, gvr.Resource.Group)}, &crd); err != nil {
		return ClaimSource{}, fmt.Errorf("unable to load CRD: %w", err)
	}
	crdsName := crd.Annotations["chrysopoeia.io/source-ref-name"]
	crdsNamespace := crd.Annotations["chrysopoeia.io/source-ref-namespace"]
	if crdsName == "" || crdsNamespace == "" {
		return ClaimSource{}, fmt.Errorf("unable to retrieve ClaimDefinition from CustomResourceDefinition; name or namespace missing ns=%q name%q", crdsNamespace, crdsName)
	}
	var cd chrysopoeiav1.ClaimDefinition
	cdkey := client.ObjectKey{Namespace: crdsNamespace, Name: crdsName}
	if err := c.Get(ctx, cdkey, &cd); err != nil {
		return ClaimSource{}, fmt.Errorf("unable to load ClaimDefinition %s: %w", cdkey, err)
	}
	var ocirep sourcev1.OCIRepository
	ocirepkey := client.ObjectKey{Namespace: cd.Namespace, Name: cd.Spec.Reference.Name}
	if err := c.Get(ctx, ocirepkey, &ocirep); err != nil {
		return ClaimSource{}, fmt.Errorf("unable to load OCIRepository %s: %w", ocirepkey, err)
	}
	cs := ClaimSource{
		ClaimDefinition: cd,
		OCIRepository:   ocirep,
	}
	if csn := ptr.Deref(ocirep.Spec.CertSecretRef, fluxmeta.LocalObjectReference{}).Name; csn != "" {
		var certsec corev1.Secret
		certseckey := client.ObjectKey{Namespace: cd.Namespace, Name: csn}
		if err := c.Get(ctx, certseckey, &certsec); err != nil {
			return ClaimSource{}, fmt.Errorf("unable to load Secret %s: %w", certseckey, err)
		}
		cs.OCIRepositoryCertSecret = &certsec
	}
	if v := cd.Spec.VersionDiscovery.Reference.Name; v != "" {
		var imgrep imagereflectorv1.ImageRepository
		imgrepkey := client.ObjectKey{Namespace: cd.Namespace, Name: v}
		if err := c.Get(ctx, imgrepkey, &imgrep); err != nil {
			return ClaimSource{}, fmt.Errorf("unable to load ImageRepository %s: %w", imgrepkey, err)
		}
		cs.ImageRepository = &imgrep
	}
	return cs, nil
}
