package controllers

import (
	"context"
	"fmt"
	"slices"
	"strings"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	chrysopoeiav1 "github.com/helmetica-framework/chrysopoeia/api/v1"
)

// DependencyGroupManager validates the claims DependencyGroups make on their CRDs. A CRD may belong
// to a single group only, so that two operators cannot end up managing the same resources.
type DependencyGroupManager struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder events.EventRecorder
}

//+kubebuilder:rbac:groups=helmetica.io,resources=dependencygroups,verbs=get;list;watch
//+kubebuilder:rbac:groups=helmetica.io,resources=dependencygroups/status,verbs=get;update;patch

func (r *DependencyGroupManager) Reconcile(ctx context.Context, req reconcile.Request) (ctrl.Result, error) {
	l := log.FromContext(ctx).WithName("DependencyGroupManager.Reconcile")
	l.Info("Reconciling DependencyGroup")

	var group chrysopoeiav1.DependencyGroup
	if err := r.Get(ctx, req.NamespacedName, &group); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !group.DeletionTimestamp.IsZero() {
		l.Info("DependencyGroup is being deleted, not validating its claims")
		return ctrl.Result{}, nil
	}

	var groups chrysopoeiav1.DependencyGroupList
	if err := r.List(ctx, &groups); err != nil {
		return ctrl.Result{}, fmt.Errorf("unable to list DependencyGroups: %w", err)
	}

	state := chrysopoeiav1.DependencyGroupAccepted
	condition := metav1.Condition{
		Type:               "Accepted",
		Status:             metav1.ConditionTrue,
		Reason:             "CRDsUnclaimed",
		Message:            "This group holds the claim on all of its CRDs",
		ObservedGeneration: group.Generation,
	}

	if conflicts := conflictingClaims(group, groups.Items); len(conflicts) > 0 {
		l.Info("DependencyGroup claims CRDs of another group", "conflicts", conflicts)

		state = chrysopoeiav1.DependencyGroupRejected
		condition.Status = metav1.ConditionFalse
		condition.Reason = "CRDsClaimedByOtherGroup"
		condition.Message = strings.Join(conflicts, "; ")

		if group.Status.State != state {
			r.Recorder.Eventf(&group, nil, "Warning", "Rejected", "Validate", "%s", condition.Message)
		}
	}

	stateChanged := group.Status.State != state
	group.Status.State = state
	if apimeta.SetStatusCondition(&group.Status.Conditions, condition) || stateChanged {
		if err := r.Status().Update(ctx, &group); err != nil {
			return ctrl.Result{}, fmt.Errorf("unable to update DependencyGroup status: %w", err)
		}
	}

	return ctrl.Result{}, nil
}

func (r *DependencyGroupManager) SetupWithManager(name string, mgr ctrl.Manager) error {
	return builder.ControllerManagedBy(mgr).
		For(&chrysopoeiav1.DependencyGroup{}).
		// A group's state depends on every other group's claims: a group that is deleted, or that
		// drops a CRD, can free a CRD another group was rejected for.
		Watches(&chrysopoeiav1.DependencyGroup{}, handler.EnqueueRequestsFromMapFunc(r.enqueueAllGroups)).
		Named(name).
		Complete(r)
}

func (r *DependencyGroupManager) enqueueAllGroups(ctx context.Context, _ client.Object) []reconcile.Request {
	var groups chrysopoeiav1.DependencyGroupList
	if err := r.List(ctx, &groups); err != nil {
		log.FromContext(ctx).Error(err, "unable to list DependencyGroups to re-validate their claims")
		return nil
	}

	requests := make([]reconcile.Request, 0, len(groups.Items))
	for _, group := range groups.Items {
		requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&group)})
	}
	return requests
}

// conflictingClaims returns a message for every CRD of group that another group claims with
// precedence. The older group wins a CRD and ties are broken by name, so that the winner is the same
// no matter which of the groups is being reconciled.
func conflictingClaims(group chrysopoeiav1.DependencyGroup, groups []chrysopoeiav1.DependencyGroup) []string {
	var conflicts []string
	for _, crd := range group.Spec.CRDs {
		for _, other := range groups {
			if other.Name == group.Name || !other.DeletionTimestamp.IsZero() {
				continue
			}
			if !claimsCRD(other, crd.Name) || !claimPrecedes(other, group) {
				continue
			}
			conflicts = append(conflicts,
				fmt.Sprintf("CRD %q is already claimed by DependencyGroup %q", crd.Name, other.Name))
		}
	}
	return conflicts
}

func claimsCRD(group chrysopoeiav1.DependencyGroup, name string) bool {
	return slices.ContainsFunc(group.Spec.CRDs, func(crd chrysopoeiav1.DependencyGroupCRD) bool {
		return crd.Name == name
	})
}

func claimPrecedes(a, b chrysopoeiav1.DependencyGroup) bool {
	if !a.CreationTimestamp.Equal(&b.CreationTimestamp) {
		return a.CreationTimestamp.Before(&b.CreationTimestamp)
	}
	return a.Name < b.Name
}
