package controllers

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	metav1ac "k8s.io/client-go/applyconfigurations/meta/v1"
	networkingv1ac "k8s.io/client-go/applyconfigurations/networking/v1"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// DependencyNetworkPolicyManager allows pods in `manages` namespaces access to `requires` namespaces.
type DependencyNetworkPolicyManager struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder events.EventRecorder
}

const dependencyNetworkPolicyManagerName = "chrysopoeia-dependency-netpol-manager"

//+kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch

func (r *DependencyNetworkPolicyManager) Reconcile(ctx context.Context, req reconcile.Request) (res ctrl.Result, err error) {
	l := log.FromContext(ctx).WithName("DependencyNetworkPolicyManager.Reconcile")
	l.Info("Reconciling")

	var ns corev1.Namespace
	if err := r.Get(ctx, req.NamespacedName, &ns); err != nil {
		l.Error(err, "unable to fetch Namespace")
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if ns.DeletionTimestamp != nil {
		l.Info("Namespace is being deleted, skipping NetPol management")
		return ctrl.Result{}, nil
	}

	requires := extractRequiresLabel(ns)
	igrs := make([]*networkingv1ac.NetworkPolicyIngressRuleApplyConfiguration, len(requires))
	for i, req := range requires {
		igrs[i] = networkingv1ac.NetworkPolicyIngressRule().
			WithFrom(
				networkingv1ac.NetworkPolicyPeer().
					WithNamespaceSelector(
						metav1ac.LabelSelector().
							WithMatchExpressions(
								metav1ac.LabelSelectorRequirement().
									WithOperator(metav1.LabelSelectorOpExists).
									WithKey(ManagesLabelPrefix + req),
							),
					),
			)
	}

	np := networkingv1ac.NetworkPolicy("chrysopoeia-depedency-manages", ns.Name).
		WithSpec(
			networkingv1ac.NetworkPolicySpec().
				WithIngress(igrs...).
				WithPolicyTypes(networkingv1.PolicyTypeIngress),
		)

	return ctrl.Result{}, r.Apply(ctx, np, client.FieldOwner(dependencyNetworkPolicyManagerName))
}

func (r *DependencyNetworkPolicyManager) SetupWithManager(name string, mgr ctrl.Manager) error {
	sel, err := labels.Parse("chrysopoeia.io/managed")
	if err != nil {
		return err
	}

	return builder.ControllerManagedBy(mgr).
		For(&corev1.Namespace{}, builder.WithPredicates(labelSelectorPredicate(sel))).
		Named(name).
		Complete(r)
}
