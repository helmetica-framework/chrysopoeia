package controllers

import (
	"context"
	"errors"
	"strings"

	"k8s.io/apimachinery/pkg/runtime"
	rbacv1ac "k8s.io/client-go/applyconfigurations/rbac/v1"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	chrysopoeiav1 "github.com/helmetica-framework/chrysopoeia/api/v1"
)

// ProvidesRBACAggregatorManager adds all provided CRDs to the aggregate clusterroles for admin, edit, and view.
// TODO we probs switch to something more fine grained when reworking the dependency system.
type ProvidesRBACAggregatorManager struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder events.EventRecorder
}

//+kubebuilder:rbac:groups=helmetica.io,resources=customresourcedefinitionsources,verbs=get;list;watch

func (r *ProvidesRBACAggregatorManager) Reconcile(ctx context.Context, _ reconcile.Request) (res ctrl.Result, err error) {
	l := log.FromContext(ctx).WithName("ProvidesRBACAggregatorManager.Reconcile")
	l.Info("Reconciling")

	// Cache is already filtered to only include CRDs that have the label "chrysopoeia.io/managed"
	var crds chrysopoeiav1.CustomResourceDefinitionSourceList
	if err := r.List(ctx, &crds); err != nil {
		return ctrl.Result{}, err
	}

	providedCRDs := make(map[string][]string)
	for _, crd := range crds.Items {
		for _, provided := range crd.Spec.Provides {
			resource, group, ok := strings.Cut(provided.Name, ".")
			if !ok {
				l.Error(errors.New("invalid provided CRD format"), "CRD must be in the format <resource>.<group>", "crd", crd, "action", "skip")
				continue
			}
			providedCRDs[group] = append(providedCRDs[group], resource)
		}
	}

	adminRules := make([]*rbacv1ac.PolicyRuleApplyConfiguration, 0, len(providedCRDs))
	editRules := make([]*rbacv1ac.PolicyRuleApplyConfiguration, 0, len(providedCRDs))
	viewRules := make([]*rbacv1ac.PolicyRuleApplyConfiguration, 0, len(providedCRDs))
	for group, resources := range providedCRDs {
		// TODO I don't think we should allow users with editor or admin access to update status subresources, we probably need a separate role to bind operators to.
		resourcesWithStatus := make([]string, 0, len(resources)*2)
		for _, resource := range resources {
			resourcesWithStatus = append(resourcesWithStatus, resource, strings.Join([]string{resource, "status"}, "/"))
		}
		adminRules = append(adminRules, rbacv1ac.PolicyRule().WithAPIGroups(group).WithResources(resourcesWithStatus...).WithVerbs("*"))
		editRules = append(editRules, rbacv1ac.PolicyRule().WithAPIGroups(group).WithResources(resourcesWithStatus...).WithVerbs("create", "update", "patch", "delete"))
		viewRules = append(viewRules, rbacv1ac.PolicyRule().WithAPIGroups(group).WithResources(resourcesWithStatus...).WithVerbs("get", "list", "watch"))
	}

	adminCR := rbacv1ac.
		ClusterRole("chrysopoeia-provider-crds-admin").
		WithLabels(map[string]string{
			"chrysopoeia.io/managed":                       "true",
			"rbac.authorization.k8s.io/aggregate-to-admin": "true",
		}).
		WithRules(adminRules...)

	editCR := rbacv1ac.
		ClusterRole("chrysopoeia-provider-crds-edit").
		WithLabels(map[string]string{
			"chrysopoeia.io/managed":                      "true",
			"rbac.authorization.k8s.io/aggregate-to-edit": "true",
		}).
		WithRules(editRules...)

	viewCR := rbacv1ac.
		ClusterRole("chrysopoeia-provider-crds-view").
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

func (r *ProvidesRBACAggregatorManager) SetupWithManager(name string, mgr ctrl.Manager) error {
	// Cache is already filtered to only include CRDs that have the label "chrysopoeia.io/managed"
	return builder.ControllerManagedBy(mgr).
		For(&chrysopoeiav1.CustomResourceDefinitionSource{}).
		Named(name).
		Complete(r)
}
