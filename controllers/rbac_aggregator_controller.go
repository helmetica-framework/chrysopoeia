package controllers

import (
	"context"
	"errors"
	"strings"

	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/runtime"
	rbacv1ac "k8s.io/client-go/applyconfigurations/rbac/v1"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

type RBACAggregatorManager struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder events.EventRecorder
}

//+kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=clusterroles,verbs=get;list;watch;create;update;patch
//+kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=clusterroles,verbs=escalate

func (r *RBACAggregatorManager) Reconcile(ctx context.Context, _ reconcile.Request) (res ctrl.Result, err error) {
	l := log.FromContext(ctx).WithName("RBACAggregatorManager.Reconcile")
	l.Info("Reconciling")

	// Cache is already filtered to only include CRDs that have the label "chrysopoeia.io/managed"
	var crds apiextv1.CustomResourceDefinitionList
	if err := r.List(ctx, &crds); err != nil {
		return ctrl.Result{}, err
	}

	adminRules := make([]*rbacv1ac.PolicyRuleApplyConfiguration, len(crds.Items))
	editRules := make([]*rbacv1ac.PolicyRuleApplyConfiguration, len(crds.Items))
	viewRules := make([]*rbacv1ac.PolicyRuleApplyConfiguration, len(crds.Items))
	for i, crd := range crds.Items {
		resource := crd.Spec.Names.Plural
		statusSubresource := strings.Join([]string{resource, "status"}, "/")
		adminRules[i] = rbacv1ac.PolicyRule().WithAPIGroups(crd.Spec.Group).WithResources(resource, statusSubresource).WithVerbs("*")
		editRules[i] = rbacv1ac.PolicyRule().WithAPIGroups(crd.Spec.Group).WithResources(resource, statusSubresource).WithVerbs("create", "update", "patch", "delete")
		viewRules[i] = rbacv1ac.PolicyRule().WithAPIGroups(crd.Spec.Group).WithResources(resource, statusSubresource).WithVerbs("get", "list", "watch")
	}

	adminCR := rbacv1ac.
		ClusterRole("chrysopoeia-dynamic-crds-admin").
		WithLabels(map[string]string{
			"chrysopoeia.io/managed":                       "true",
			"rbac.authorization.k8s.io/aggregate-to-admin": "true",
		}).
		WithRules(adminRules...)

	editCR := rbacv1ac.
		ClusterRole("chrysopoeia-dynamic-crds-edit").
		WithLabels(map[string]string{
			"chrysopoeia.io/managed":                      "true",
			"rbac.authorization.k8s.io/aggregate-to-edit": "true",
		}).
		WithRules(editRules...)

	viewCR := rbacv1ac.
		ClusterRole("chrysopoeia-dynamic-crds-view").
		WithLabels(map[string]string{
			"chrysopoeia.io/managed":                      "true",
			"rbac.authorization.k8s.io/aggregate-to-view": "true",
		}).
		WithRules(viewRules...)

	owner := client.FieldOwner("rbac-aggregator-manager")
	if err := errors.Join(
		r.Apply(ctx, adminCR, owner),
		r.Apply(ctx, editCR, owner),
		r.Apply(ctx, viewCR, owner),
	); err != nil {
		return ctrl.Result{}, err
	}

	l.Info("Reconciled with %d CRDs", "count", len(crds.Items))

	return ctrl.Result{}, nil
}

func (r *RBACAggregatorManager) SetupWithManager(name string, mgr ctrl.Manager) error {
	// Cache is already filtered to only include CRDs that have the label "chrysopoeia.io/managed"
	return builder.ControllerManagedBy(mgr).
		For(&apiextv1.CustomResourceDefinition{}).
		Named(name).
		Complete(r)
}
