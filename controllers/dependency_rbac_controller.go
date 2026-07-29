package controllers

import (
	"context"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	rbacv1ac "k8s.io/client-go/applyconfigurations/rbac/v1"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

type DependencyRBACManager struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder events.EventRecorder
}

//+kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=clusterroles,verbs=bind
//+kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=rolebindings,verbs=get;list;watch;create;update;patch
//+kubebuilder:rbac:groups="",resources=serviceaccounts,verbs=get;list;watch
//+kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch

func (r *DependencyRBACManager) Reconcile(ctx context.Context, req reconcile.Request) (res ctrl.Result, err error) {
	l := log.FromContext(ctx).WithName("DependencyRBACManager.Reconcile")
	l.Info("Reconciling")

	var ns corev1.Namespace
	if err := r.Get(ctx, req.NamespacedName, &ns); err != nil {
		l.Error(err, "unable to fetch Namespace")
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if ns.DeletionTimestamp != nil {
		l.Info("Namespace is being deleted, skipping RBAC management")
		return ctrl.Result{}, nil
	}

	for _, req := range extractRequiresLabel(ns) {
		l.Info("Namespace requires", "requirement", req)
		var providers corev1.NamespaceList
		if err := r.List(ctx, &providers, client.MatchingLabels{"provides.helmetica.io/" + req: ""}); err != nil {
			l.Error(err, "unable to list provider Namespaces")
			return ctrl.Result{}, err
		}
		for _, provider := range providers.Items {
			l.Info("Found provider", "provider", provider.Name)

			var serviceAccounts corev1.ServiceAccountList
			if err := r.List(ctx, &serviceAccounts, client.InNamespace(provider.Name)); err != nil {
				l.Error(err, "unable to list ServiceAccounts in provider Namespace")
				return ctrl.Result{}, err
			}

			subjects := make([]*rbacv1ac.SubjectApplyConfiguration, 0, len(serviceAccounts.Items))
			for _, sa := range serviceAccounts.Items {
				subjects = append(subjects,
					rbacv1ac.Subject().
						WithKind("ServiceAccount").
						WithName(sa.Name).
						WithNamespace(sa.Namespace),
				)
			}

			prb := rbacv1ac.
				RoleBinding(strings.Join([]string{"chrysopoeia", "provider", provider.Name, req}, ":"), ns.Name).
				WithLabels(map[string]string{
					"chrysopoeia.io/managed": "",
				}).
				WithRoleRef(
					rbacv1ac.RoleRef().
						WithAPIGroup("rbac.authorization.k8s.io").
						WithKind("ClusterRole").
						WithName("admin"),
				).
				WithSubjects(subjects...)

			if err := r.Apply(ctx, prb, client.ForceOwnership, client.FieldOwner("chrysopoeia-dependency-rbac-manager")); err != nil {
				l.Error(err, "unable to apply RoleBinding")
				return ctrl.Result{}, err
			}
		}
	}

	return ctrl.Result{}, nil
}

func (r *DependencyRBACManager) SetupWithManager(name string, mgr ctrl.Manager) error {
	// Cache is already filtered to only include CRDs that have the label "chrysopoeia.io/managed"
	return builder.ControllerManagedBy(mgr).
		For(&corev1.Namespace{}).
		Named(name).
		Complete(r)
}

func extractRequiresLabel(ns corev1.Namespace) []string {
	const requiresLabelPrefix = "requires.helmetica.io/"

	requires := make([]string, 0, len(ns.Labels))
	for k := range ns.Labels {
		if strings.HasPrefix(k, requiresLabelPrefix) {
			requires = append(requires, strings.TrimPrefix(k, requiresLabelPrefix))
		}
	}
	return requires
}
