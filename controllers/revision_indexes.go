package controllers

import (
	"context"

	chrysopoeiav1 "github.com/helmetica-framework/chrysopoeia/api/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const ownerUIDField = "metadata.ownerReferences.uid"

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
