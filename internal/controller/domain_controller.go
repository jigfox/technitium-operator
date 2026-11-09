package controller

import (
	"context"
	"fmt"
	"slices"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	techv1alpha1 "github.com/jigfox/technitium-operator/api/v1alpha1"
	"github.com/jigfox/technitium-operator/internal/technitium"
)

// DomainReconciler reconciles TechnitiumAllowedDomain (List = Allowed) or
// TechnitiumBlockedDomain (List = Blocked) resources: the two kinds differ only
// in the list they manage.
type DomainReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Provider ClientProvider
	Recorder events.EventRecorder
	DryRun   bool
	Interval time.Duration
	List     technitium.List
}

// +kubebuilder:rbac:groups=technitium.jigfox.github.io,resources=technitiumalloweddomains;technitiumblockeddomains,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=technitium.jigfox.github.io,resources=technitiumalloweddomains/status;technitiumblockeddomains/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=technitium.jigfox.github.io,resources=technitiumalloweddomains/finalizers;technitiumblockeddomains/finalizers,verbs=update
// +kubebuilder:rbac:groups=technitium.jigfox.github.io,resources=technitiumservers,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch

func (r *DomainReconciler) shared() *shared {
	return &shared{Recorder: r.Recorder, DryRun: r.DryRun, Interval: r.Interval}
}

func (r *DomainReconciler) kind() string {
	if r.List == technitium.Allowed {
		return "allowed"
	}
	return "blocked"
}

func (r *DomainReconciler) newObject() client.Object { return newDomainObject(r.List) }

func newDomainObject(list technitium.List) client.Object {
	if list == technitium.Allowed {
		return &techv1alpha1.TechnitiumAllowedDomain{}
	}
	return &techv1alpha1.TechnitiumBlockedDomain{}
}

func specOf(o client.Object) *techv1alpha1.DomainSpec {
	switch t := o.(type) {
	case *techv1alpha1.TechnitiumAllowedDomain:
		return &t.Spec
	case *techv1alpha1.TechnitiumBlockedDomain:
		return &t.Spec
	}
	panic(fmt.Sprintf("not a domain resource: %T", o))
}

func statusOf(o client.Object) *techv1alpha1.DomainStatus {
	switch t := o.(type) {
	case *techv1alpha1.TechnitiumAllowedDomain:
		return &t.Status
	case *techv1alpha1.TechnitiumBlockedDomain:
		return &t.Status
	}
	panic(fmt.Sprintf("not a domain resource: %T", o))
}

// listDomainResources returns the domain resources of one list in a namespace.
func listDomainResources(ctx context.Context, c client.Reader, list technitium.List,
	namespace string) ([]client.Object, error) {
	var out []client.Object
	if list == technitium.Allowed {
		var l techv1alpha1.TechnitiumAllowedDomainList
		if err := c.List(ctx, &l, client.InNamespace(namespace)); err != nil {
			return nil, err
		}
		for i := range l.Items {
			out = append(out, &l.Items[i])
		}
		return out, nil
	}
	var l techv1alpha1.TechnitiumBlockedDomainList
	if err := c.List(ctx, &l, client.InNamespace(namespace)); err != nil {
		return nil, err
	}
	for i := range l.Items {
		out = append(out, &l.Items[i])
	}
	return out, nil
}

// Reconcile makes Technitium's list contain the domain.
func (r *DomainReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	obj := r.newObject()
	if err := r.Get(ctx, req.NamespacedName, obj); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	s := r.shared()

	if !obj.GetDeletionTimestamp().IsZero() {
		return ctrl.Result{}, r.finalize(ctx, s, obj)
	}
	if controllerutil.AddFinalizer(obj, Finalizer) {
		if err := r.Update(ctx, obj); err != nil {
			return ctrl.Result{}, err
		}
	}

	orig := obj.DeepCopyObject().(client.Object)
	pending := 0
	ready, reason, msg, err := r.sync(ctx, s, obj, &pending)
	status := statusOf(obj)
	status.ObservedGeneration = obj.GetGeneration()
	if err != nil {
		reason, msg = techv1alpha1.ReasonTechnitium, err.Error()
		if IsCredentialsError(err) {
			reason = techv1alpha1.ReasonCredentials
		}
	}
	s.setConditions(&status.Conditions, obj.GetGeneration(), ready, reason, msg, pending)
	if perr := r.Status().Patch(ctx, obj, client.MergeFrom(orig)); perr != nil {
		if apierrors.IsNotFound(perr) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, perr
	}
	switch {
	case err != nil && IsCredentialsError(err):
		// Only a change of the Secret or the server helps.
		return ctrl.Result{RequeueAfter: retryInterval}, nil
	case err != nil:
		return ctrl.Result{}, err
	case reason == techv1alpha1.ReasonServerNotFound:
		// Creating the server triggers a reconcile; this is the fallback.
		return ctrl.Result{RequeueAfter: retryInterval}, nil
	}
	return ctrl.Result{RequeueAfter: s.interval()}, nil
}

// server returns the TechnitiumServer a domain resource refers to.
func (r *DomainReconciler) server(ctx context.Context, obj client.Object) (*techv1alpha1.TechnitiumServer, error) {
	var server techv1alpha1.TechnitiumServer
	key := types.NamespacedName{Namespace: obj.GetNamespace(), Name: specOf(obj).ServerRef.Name}
	if err := r.Get(ctx, key, &server); err != nil {
		return nil, err
	}
	return &server, nil
}

// sync adopts or adds the entry. It fills the status.
func (r *DomainReconciler) sync(ctx context.Context, s *shared, obj client.Object,
	pending *int) (ready bool, reason, msg string, err error) {
	spec, status := specOf(obj), statusOf(obj)
	domain := techv1alpha1.NormalizeDomain(spec.Domain)
	if domain == "" {
		return false, techv1alpha1.ReasonInvalid, "spec.domain is empty", nil
	}
	server, err := r.server(ctx, obj)
	if apierrors.IsNotFound(err) {
		return false, techv1alpha1.ReasonServerNotFound,
			fmt.Sprintf("TechnitiumServer %q not found", spec.ServerRef.Name), nil
	}
	if err != nil {
		return false, "", "", err
	}
	c, err := r.Provider.ClientFor(ctx, server)
	if err != nil {
		return false, "", "", err
	}
	entries, err := c.ListDomains(ctx, r.List)
	if err != nil {
		return false, "", "", err
	}

	if slices.Contains(entries, domain) {
		if status.Domain == "" {
			status.Adopted = true
			s.Recorder.Eventf(obj, nil, "Normal", "Adopted", "Adopt",
				"Adopted existing %s entry %q", r.kind(), domain)
		}
		status.Domain = domain
		return true, techv1alpha1.ReasonReconciled, "entry exists", nil
	}
	did, err := s.run(ctx, obj, pending, "Add", fmt.Sprintf("add %q to the %s list", domain, r.kind()),
		func() error { return c.AddDomain(ctx, r.List, domain) })
	if err != nil {
		return false, "", "", err
	}
	if did {
		status.Domain = domain
	}
	return true, techv1alpha1.ReasonReconciled, "entry exists", nil
}

// finalize deletes the entry, unless the policy says to orphan it.
func (r *DomainReconciler) finalize(ctx context.Context, s *shared, obj client.Object) error {
	if !controllerutil.ContainsFinalizer(obj, Finalizer) {
		return nil
	}
	spec, status := specOf(obj), statusOf(obj)
	if spec.EffectiveDeletionPolicy() == techv1alpha1.DeletionPolicyDelete && status.Domain != "" {
		if err := r.deleteEntry(ctx, s, obj, status.Domain); err != nil {
			return err
		}
	}
	controllerutil.RemoveFinalizer(obj, Finalizer)
	return client.IgnoreNotFound(r.Update(ctx, obj))
}

func (r *DomainReconciler) deleteEntry(ctx context.Context, s *shared, obj client.Object, domain string) error {
	server, err := r.server(ctx, obj)
	if apierrors.IsNotFound(err) {
		// Waiting would block the deletion forever; delete the domain
		// resources before their server to avoid this.
		logf.FromContext(ctx).Info("TechnitiumServer is gone, leaving the entry", "domain", domain)
		s.Recorder.Eventf(obj, nil, "Warning", "ServerNotFound", "Delete",
			"TechnitiumServer %q not found: %q stays in the %s list", specOf(obj).ServerRef.Name, domain, r.kind())
		return nil
	}
	if err != nil {
		return err
	}
	c, err := r.Provider.ClientFor(ctx, server)
	if err != nil {
		return err
	}
	pending := 0
	_, err = s.run(ctx, obj, &pending, "Delete", fmt.Sprintf("delete %q from the %s list", domain, r.kind()),
		func() error { return c.DeleteDomain(ctx, r.List, domain) })
	return err
}

// SetupWithManager sets up the controller with the Manager.
func (r *DomainReconciler) SetupWithManager(mgr ctrl.Manager) error {
	name := r.kind() + "domain"
	return ctrl.NewControllerManagedBy(mgr).
		For(r.newObject()).
		// A TechnitiumServer that appears or changes releases the resources
		// waiting for it.
		Watches(&techv1alpha1.TechnitiumServer{}, handler.EnqueueRequestsFromMapFunc(r.forServer)).
		Named(name).
		Complete(r)
}

func (r *DomainReconciler) forServer(ctx context.Context, o client.Object) []reconcile.Request {
	items, err := listDomainResources(ctx, r.Client, r.List, o.GetNamespace())
	if err != nil {
		logf.FromContext(ctx).Error(err, "Listing domain resources failed")
		return nil
	}
	var reqs []reconcile.Request
	for _, it := range items {
		if specOf(it).ServerRef.Name == o.GetName() {
			reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(it)})
		}
	}
	return reqs
}
