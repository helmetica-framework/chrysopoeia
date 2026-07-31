package controllers

import (
	"context"
	"errors"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	rbacv1ac "k8s.io/client-go/applyconfigurations/rbac/v1"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

type DependencyRBACManager struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder events.EventRecorder
}

const RequiresLabelPrefix = "requires.helmetica.io/"
const ProvidesLabelPrefix = "provides.helmetica.io/"
const dependencyRBACManagerName = "chrysopoeia-dependency-rbac-manager"

//+kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=clusterroles,verbs=bind;escalate
//+kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=clusterroles;clusterrolebindings;rolebindings,verbs=get;list;watch;create;update;patch
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

	if err := r.ensureProviderRoles(ctx, ns, extractProvidesLabel(ns)); err != nil {
		l.Error(err, "unable to ensure provider roles")
		return ctrl.Result{}, err
	}

	for _, req := range extractRequiresLabel(ns) {
		l.Info("Namespace requires", "requirement", req)
		var providers corev1.NamespaceList
		if err := r.List(ctx, &providers, client.MatchingLabels{ProvidesLabelPrefix + req: ""}); err != nil {
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

			if err := r.Apply(ctx, prb, client.ForceOwnership, client.FieldOwner(dependencyRBACManagerName)); err != nil {
				l.Error(err, "unable to apply RoleBinding")
				return ctrl.Result{}, err
			}
		}
	}

	return ctrl.Result{}, nil
}

func (r *DependencyRBACManager) SetupWithManager(name string, mgr ctrl.Manager) error {
	sel, err := labels.Parse("chrysopoeia.io/managed")
	if err != nil {
		return err
	}

	return builder.ControllerManagedBy(mgr).
		For(&corev1.Namespace{}, builder.WithPredicates(labelSelectorPredicate(sel))).
		Named(name).
		Complete(r)
}

func (r *DependencyRBACManager) ensureProviderRoles(ctx context.Context, providerNamespace corev1.Namespace, provides []string) error {
	roleName := strings.Join([]string{"chrysopoeia", "provider", "scopedlist", providerNamespace.Name}, ":")

	slcr := rbacv1ac.ClusterRole(roleName)
	if len(provides) > 0 {
		resourceNames := make([]string, len(provides))
		for i, p := range provides {
			resourceNames[i] = RequiresLabelPrefix + p
		}
		slcr = slcr.WithRules(
			rbacv1ac.PolicyRule().
				WithAPIGroups("*").
				WithResources("*").
				WithVerbs("scopedlist").
				WithResourceNames(resourceNames...),
		)
	}

	var saList corev1.ServiceAccountList
	if err := r.List(ctx, &saList, client.InNamespace(providerNamespace.Name)); err != nil {
		return err
	}
	sas := make([]*rbacv1ac.SubjectApplyConfiguration, 0, len(saList.Items))
	for _, sa := range saList.Items {
		sas = append(sas,
			rbacv1ac.Subject().
				WithKind("ServiceAccount").
				WithName(sa.Name).
				WithNamespace(sa.Namespace),
		)
	}
	slcrb := rbacv1ac.
		ClusterRoleBinding(roleName).
		WithLabels(map[string]string{
			"chrysopoeia.io/managed": "",
		}).
		WithRoleRef(
			rbacv1ac.RoleRef().
				WithAPIGroup("rbac.authorization.k8s.io").
				WithKind("ClusterRole").
				WithName(roleName),
		).
		WithSubjects(sas...)

	return errors.Join(
		r.Apply(ctx, slcr, client.ForceOwnership, client.FieldOwner(dependencyRBACManagerName)),
		r.Apply(ctx, slcrb, client.ForceOwnership, client.FieldOwner(dependencyRBACManagerName)),
	)
}

func extractRequiresLabel(ns corev1.Namespace) []string {
	return extractPrefixedLabel(ns, RequiresLabelPrefix)
}

func extractProvidesLabel(ns corev1.Namespace) []string {
	return extractPrefixedLabel(ns, ProvidesLabelPrefix)
}

func extractPrefixedLabel(ns corev1.Namespace, prefix string) []string {
	values := make([]string, 0, len(ns.Labels))
	for k := range ns.Labels {
		if strings.HasPrefix(k, prefix) {
			values = append(values, strings.TrimPrefix(k, prefix))
		}
	}
	return values
}

// labelSelectorPredicate returns a predicate that matches objects with the given label.
func labelSelectorPredicate(sel labels.Selector) predicate.Predicate {
	return predicate.NewPredicateFuncs(func(o client.Object) bool {
		return sel.Matches(labels.Set(o.GetLabels()))
	})
}
