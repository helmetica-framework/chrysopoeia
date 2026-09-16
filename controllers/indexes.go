package controllers

import (
	"context"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	chrysopoeiav1 "github.com/helmetica-framework/chrysopoeia/api/v1"
)

const (
	ownerUIDField     = "metadata.ownerReferences.uid"
	crdGroupKindField = "spec.group+spec.names.kind"
)

func SetupInstanceRevisionOwnerFieldIndex(mgr ctrl.Manager) error {
	return mgr.GetFieldIndexer().IndexField(context.Background(), &chrysopoeiav1.InstanceRevision{}, ownerUIDField, func(rawObj client.Object) []string {
		refs := rawObj.GetOwnerReferences()
		uids := make([]string, len(refs))
		for i, ref := range refs {
			uids[i] = string(ref.UID)
		}
		return uids
	})
}

// crdGroupKindIndexKey is the key a CustomResourceDefinition is indexed and looked up under.
func crdGroupKindIndexKey(gk schema.GroupKind) string {
	return gk.String()
}

// indexCRDByGroupKind is the index function, shared with the tests so they index the way the
// manager does.
func indexCRDByGroupKind(rawObj client.Object) []string {
	crd, ok := rawObj.(*apiextensionsv1.CustomResourceDefinition)
	if !ok {
		return nil
	}
	return []string{crdGroupKindIndexKey(schema.GroupKind{Group: crd.Spec.Group, Kind: crd.Spec.Names.Kind})}
}

// SetupCustomResourceDefinitionGroupKindFieldIndex indexes CRDs by the group and kind they serve, so
// a claim can find its own CRD without the cache copying out every other one.
func SetupCustomResourceDefinitionGroupKindFieldIndex(mgr ctrl.Manager) error {
	return mgr.GetFieldIndexer().IndexField(context.Background(), &apiextensionsv1.CustomResourceDefinition{}, crdGroupKindField, indexCRDByGroupKind)
}
