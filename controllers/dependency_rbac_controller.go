package controllers

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	rbacv1ac "k8s.io/client-go/applyconfigurations/rbac/v1"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	chrysopoeiav1 "github.com/helmetica-framework/chrysopoeia/api/v1"
)

type DependencyRBACManager struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder events.EventRecorder
}

const RequiresLabelPrefix = "requires.helmetica.io/"
const ManagesLabelPrefix = "manages.helmetica.io/"
const dependencyRBACManagerName = "chrysopoeia-dependency-rbac-manager"

//+kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=clusterroles;roles,verbs=bind;escalate
//+kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=clusterroles;clusterrolebindings;rolebindings,verbs=get;list;watch;create;update;patch
//+kubebuilder:rbac:groups="",resources=serviceaccounts,verbs=get;list;watch
//+kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch

// Reconcile connects RBAC so provider namespaces can access manifests in namespaces that require them.
// It can reconcile any namespace.
// If the reconciled namespace has a provider label it ensures correct `scopedlist` and cluster wide permissions.
// If the reconciled namespace has a requires label it ensures all service accounts in namespaces with the provider label get access to the reconciled namespace.
// If the namespace has no label nothing happens.
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

	managesGroups, err := r.dependencyGroupsForNames(ctx, extractManagesLabel(ns))
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to load 'manages' dependency groups: %w", err)
	}
	requiresGroups, err := r.dependencyGroupsForNames(ctx, extractRequiresLabel(ns))
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to load 'requires' dependency groups: %w", err)
	}

	if err := r.ensureScopedListPermissions(ctx, ns, managesGroups); err != nil {
		return ctrl.Result{}, fmt.Errorf("unable to ensure provider roles: %w", err)
	}

	if err := r.ensureClusterScopedRules(ctx, ns, managesGroups); err != nil {
		return ctrl.Result{}, fmt.Errorf("unable to ensure provider roles: %w", err)
	}

	if err := r.ensureRequiresRules(ctx, ns, requiresGroups); err != nil {
		return ctrl.Result{}, fmt.Errorf("unable to ensure provider roles: %w", err)
	}

	return ctrl.Result{}, nil
}

func (r *DependencyRBACManager) ensureRequiresRules(ctx context.Context, providerNs corev1.Namespace, requires []chrysopoeiav1.DependencyGroup) error {
	l := log.FromContext(ctx).WithName("DependencyRBACManager.ensureRequiresRules")

	for _, req := range requires {
		l.Info("Namespace requires", "requirement", req)
		var providers corev1.NamespaceList
		if err := r.List(ctx, &providers, client.MatchingLabels{ManagesLabelPrefix + req.Name: ""}); err != nil {
			return fmt.Errorf("unable to list provider namespaces: %w", err)
		}
		for _, provider := range providers.Items {
			l.Info("Found provider", "provider", provider.Name)

			var serviceAccounts corev1.ServiceAccountList
			if err := r.List(ctx, &serviceAccounts, client.InNamespace(provider.Name)); err != nil {
				return fmt.Errorf("unable to list service accounts in provider namespace: %w", err)
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

			roleName := strings.Join([]string{"chrysopoeia", "provider", provider.Name, req.Name}, ":")

			pr := rbacv1ac.
				Role(roleName, providerNs.Name).
				WithLabels(map[string]string{
					"chrysopoeia.io/managed": "",
				})

			for _, rule := range req.Spec.Rules {
				pr = pr.WithRules(applyConfigFromPolicyRule(rule))
			}

			prb := rbacv1ac.
				RoleBinding(roleName, providerNs.Name).
				WithLabels(map[string]string{
					"chrysopoeia.io/managed": "",
				}).
				WithRoleRef(
					rbacv1ac.RoleRef().
						WithAPIGroup("rbac.authorization.k8s.io").
						WithKind("ClusterRole").
						WithName(roleName),
				).
				WithSubjects(subjects...)

			if err := errors.Join(
				r.Apply(ctx, pr, client.ForceOwnership, client.FieldOwner(dependencyRBACManagerName)),
				r.Apply(ctx, prb, client.ForceOwnership, client.FieldOwner(dependencyRBACManagerName)),
			); err != nil {
				return fmt.Errorf("unable to apply role or rolebinding: %w", err)
			}
		}
	}

	return nil
}

func (r *DependencyRBACManager) SetupWithManager(name string, mgr ctrl.Manager) error {
	sel, err := labels.Parse("chrysopoeia.io/managed")
	if err != nil {
		return err
	}

	return builder.ControllerManagedBy(mgr).
		For(&corev1.Namespace{}, builder.WithPredicates(labelSelectorPredicate(sel))).
		Watches(&corev1.ServiceAccount{}, handler.EnqueueRequestsFromMapFunc(mapToManagedNamespace(r.Client))).
		Watches(&chrysopoeiav1.DependencyGroup{}, handler.EnqueueRequestsFromMapFunc(mapToManagedNamespaces(r.Client))).
		Named(name).
		Complete(r)
}

// mapToManagedNamespace returns a reconcile request for the objects namespace if the object namespace has the "chrysopoeia.io/managed" label
func mapToManagedNamespace(c client.Reader) handler.MapFunc {
	return func(ctx context.Context, obj client.Object) []reconcile.Request {
		nsKey := types.NamespacedName{Name: obj.GetName()}
		var ns corev1.Namespace
		if err := c.Get(ctx, nsKey, &ns); err != nil {
			return nil
		} else if _, ok := ns.Labels["chrysopoeia.io/managed"]; !ok {
			return nil
		}
		return []reconcile.Request{{NamespacedName: nsKey}}
	}
}

// mapToManagedNamespaces returns a reconcile request all namespaces with the chrysopoeia.io/managed label
func mapToManagedNamespaces(c client.Reader) handler.MapFunc {
	return func(ctx context.Context, _ client.Object) []reconcile.Request {
		var nsl corev1.NamespaceList
		if err := c.List(ctx, &nsl, client.HasLabels{"chrysopoeia.io/managed"}); err != nil {
			log.FromContext(ctx).WithName("mapToManagedNamespaces").Error(err, "failed mapper")
			return nil
		}
		requests := make([]reconcile.Request, len(nsl.Items))
		for i, ns := range nsl.Items {
			requests[i] = reconcile.Request{Name: ns.Name}
		}
		return requests
	}
}

func (r *DependencyRBACManager) ensureScopedListPermissions(ctx context.Context, providerNamespace corev1.Namespace, manages []chrysopoeiav1.DependencyGroup) error {
	roleName := strings.Join([]string{"chrysopoeia", "provider", "scopedlist", providerNamespace.Name}, ":")

	slcr := rbacv1ac.ClusterRole(roleName)
	for _, p := range manages {
		for _, crdRef := range p.Spec.CRDs {
			resource, group, found := strings.Cut(crdRef.Name, ".")
			if !found {
				continue
			}
			slcr = slcr.WithRules(
				rbacv1ac.PolicyRule().
					WithAPIGroups(group).
					WithResources(resource).
					WithVerbs("scopedlist").
					WithResourceNames(RequiresLabelPrefix + p.Name),
			)
		}
		for _, rule := range slices.Concat(p.Spec.ClusterRules, p.Spec.Rules) {
			if (slices.Contains(rule.Verbs, "list") || slices.Contains(rule.Verbs, "watch") || slices.Contains(rule.Verbs, "*")) &&
				len(rule.ResourceNames) == 0 &&
				len(rule.NonResourceURLs) == 0 {
				slcr = slcr.WithRules(
					rbacv1ac.PolicyRule().
						WithAPIGroups(rule.APIGroups...).
						WithResources(rule.Resources...).
						WithVerbs("scopedlist").
						WithResourceNames(RequiresLabelPrefix + p.Name),
				)
			}
		}
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

func (r *DependencyRBACManager) ensureClusterScopedRules(ctx context.Context, providerNamespace corev1.Namespace, manages []chrysopoeiav1.DependencyGroup) error {
	roleName := strings.Join([]string{"chrysopoeia", "provider", "clusterscoped", providerNamespace.Name}, ":")

	slcr := rbacv1ac.ClusterRole(roleName)
	for _, p := range manages {
		for _, rule := range p.Spec.ClusterRules {
			slcr = slcr.WithRules(applyConfigFromPolicyRule(rule))
		}
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

// dependencyGroupsForNames returns all dependency groups it can find for the given names and logs not found errors
func (r *DependencyRBACManager) dependencyGroupsForNames(ctx context.Context, names []string) ([]chrysopoeiav1.DependencyGroup, error) {
	l := log.FromContext(ctx).WithName("dependencyGroupsForNames")

	var dgl chrysopoeiav1.DependencyGroupList
	if err := r.List(ctx, &dgl); err != nil {
		return nil, fmt.Errorf("failed to load dependency groups: %w", err)
	}
	found := make([]chrysopoeiav1.DependencyGroup, 0, len(dgl.Items))
	for _, name := range names {
		idx := slices.IndexFunc(dgl.Items, func(dg chrysopoeiav1.DependencyGroup) bool { return dg.Name == name })
		if idx == -1 {
			l.Error(errors.New("not found"), "depedency group not found", "depedency_group", name)
			continue
		}
		found = append(found, dgl.Items[idx])
	}
	slices.SortFunc(found, func(a, b chrysopoeiav1.DependencyGroup) int { return strings.Compare(a.Name, b.Name) })
	return found, nil
}

func extractRequiresLabel(ns corev1.Namespace) []string {
	return extractPrefixedLabel(ns, RequiresLabelPrefix)
}

func extractManagesLabel(ns corev1.Namespace) []string {
	return extractPrefixedLabel(ns, ManagesLabelPrefix)
}

func extractPrefixedLabel(ns corev1.Namespace, prefix string) []string {
	values := make([]string, 0, len(ns.Labels))
	for k := range ns.Labels {
		if after, ok := strings.CutPrefix(k, prefix); ok {
			values = append(values, after)
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

func applyConfigFromPolicyRule(rule rbacv1.PolicyRule) *rbacv1ac.PolicyRuleApplyConfiguration {
	return rbacv1ac.PolicyRule().
		WithAPIGroups(rule.APIGroups...).
		WithResources(rule.Resources...).
		WithVerbs(rule.Verbs...).
		WithResourceNames(rule.ResourceNames...).
		WithNonResourceURLs(rule.NonResourceURLs...)
}
