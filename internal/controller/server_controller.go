package controller

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	techv1alpha1 "github.com/jigfox/technitium-operator/api/v1alpha1"
	"github.com/jigfox/technitium-operator/internal/technitium"
)

// CacheSyncer reports whether the informer caches are synced; the manager's
// cache implements it.
type CacheSyncer interface {
	WaitForCacheSync(ctx context.Context) bool
}

// ServerReconciler reconciles a TechnitiumServer: it checks the login, applies
// the settings and app configs and prunes unmanaged domains.
type ServerReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Provider ClientProvider
	Recorder events.EventRecorder
	DryRun   bool
	Interval time.Duration
	// Cache is asked before anything is pruned: the managed set is computed
	// from the cache, and a cache that is not synced yet looks empty. Nil means
	// the client reads live (tests).
	Cache CacheSyncer
}

// +kubebuilder:rbac:groups=technitium.jigfox.github.io,resources=technitiumservers,verbs=get;list;watch
// +kubebuilder:rbac:groups=technitium.jigfox.github.io,resources=technitiumservers/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=technitium.jigfox.github.io,resources=technitiumalloweddomains;technitiumblockeddomains,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch

func (r *ServerReconciler) shared() *shared {
	return &shared{Recorder: r.Recorder, DryRun: r.DryRun, Interval: r.Interval}
}

// problem is something that keeps a server from being Ready but does not stop
// the rest of the reconcile.
type problem struct{ reason, msg string }

// Reconcile makes the TechnitiumServer's Technitium match its spec.
func (r *ServerReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var server techv1alpha1.TechnitiumServer
	if err := r.Get(ctx, req.NamespacedName, &server); err != nil {
		if apierrors.IsNotFound(err) {
			r.Provider.Forget(ctx, req.NamespacedName)
		}
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !server.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}
	s := r.shared()

	orig := server.DeepCopy()
	pending := 0
	problems, err := r.sync(ctx, s, &server, &pending)
	server.Status.ObservedGeneration = server.Generation
	ready, reason, msg := true, techv1alpha1.ReasonReconciled, "server matches spec"
	switch {
	case err != nil:
		ready, reason, msg = false, techv1alpha1.ReasonTechnitium, err.Error()
		if IsCredentialsError(err) {
			reason = techv1alpha1.ReasonCredentials
		}
	case len(problems) > 0:
		ready, reason = false, problems[0].reason
		msgs := make([]string, len(problems))
		for i, p := range problems {
			msgs[i] = p.msg
		}
		msg = strings.Join(msgs, "; ")
	}
	s.setConditions(&server.Status.Conditions, server.Generation, ready, reason, msg, pending)
	if perr := r.Status().Patch(ctx, &server, client.MergeFrom(orig)); perr != nil {
		if apierrors.IsNotFound(perr) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, perr
	}
	switch {
	case err != nil && IsCredentialsError(err):
		return ctrl.Result{RequeueAfter: retryInterval}, nil
	case err != nil:
		return ctrl.Result{}, err
	case len(problems) > 0:
		return ctrl.Result{RequeueAfter: retryInterval}, nil
	}
	return ctrl.Result{RequeueAfter: s.interval()}, nil
}

// sync applies everything. A returned error stops the reconcile (the server is
// unreachable or refuses the login); problems are collected.
func (r *ServerReconciler) sync(ctx context.Context, s *shared, server *techv1alpha1.TechnitiumServer,
	pending *int) ([]problem, error) {
	c, err := r.Provider.ClientFor(ctx, server)
	if err != nil {
		return nil, err
	}
	settings, err := c.GetSettings(ctx)
	if err != nil {
		return nil, err
	}
	server.Status.Version = settings.Version

	var problems []problem
	if err := r.syncSettings(ctx, s, server, c, settings, pending); err != nil {
		problems = append(problems, problem{techv1alpha1.ReasonTechnitium, err.Error()})
	}
	problems = append(problems, r.syncApps(ctx, s, server, c, pending)...)
	for _, list := range []technitium.List{technitium.Allowed, technitium.Blocked} {
		if !r.pruneEnabled(server, list) {
			continue
		}
		if err := r.prune(ctx, s, server, c, list, pending); err != nil {
			problems = append(problems, problem{techv1alpha1.ReasonTechnitium, err.Error()})
		}
	}
	return problems, nil
}

func (r *ServerReconciler) syncSettings(ctx context.Context, s *shared, server *techv1alpha1.TechnitiumServer,
	c technitium.Client, live technitium.Settings, pending *int) error {
	if server.Spec.Settings == nil || server.Spec.Settings.BlockListURLs == nil {
		return nil
	}
	want := *server.Spec.Settings.BlockListURLs
	if slices.Equal(want, live.BlockListURLs) {
		return nil
	}
	_, err := s.run(ctx, server, pending, "SetBlockLists",
		fmt.Sprintf("set %d block list URL(s)", len(want)),
		func() error { return c.SetBlockListURLs(ctx, want) })
	return err
}

func (r *ServerReconciler) syncApps(ctx context.Context, s *shared, server *techv1alpha1.TechnitiumServer,
	c technitium.Client, pending *int) []problem {
	var problems []problem
	for _, app := range server.Spec.Apps {
		live, err := c.GetAppConfig(ctx, app.Name)
		if errors.Is(err, technitium.ErrNotFound) {
			problems = append(problems, problem{techv1alpha1.ReasonAppError,
				fmt.Sprintf("app %q is not installed", app.Name)})
			continue
		}
		if err != nil {
			problems = append(problems, problem{techv1alpha1.ReasonAppError, fmt.Sprintf("app %q: %v", app.Name, err)})
			continue
		}
		merged, changed, err := mergeAppConfig(live, app.Config.Raw)
		if err != nil {
			problems = append(problems, problem{techv1alpha1.ReasonAppError, fmt.Sprintf("app %q: %v", app.Name, err)})
			continue
		}
		if !changed {
			continue
		}
		if _, err := s.run(ctx, server, pending, "SetAppConfig", fmt.Sprintf("update the config of app %q", app.Name),
			func() error { return c.SetAppConfig(ctx, app.Name, merged) }); err != nil {
			problems = append(problems, problem{techv1alpha1.ReasonAppError, err.Error()})
		}
	}
	return problems
}

func (r *ServerReconciler) pruneEnabled(server *techv1alpha1.TechnitiumServer, list technitium.List) bool {
	if list == technitium.Allowed {
		return server.Spec.PruneUnmanaged.AllowedDomains
	}
	return server.Spec.PruneUnmanaged.BlockedDomains
}

// prune deletes the entries of a list that no domain resource of this server
// covers.
func (r *ServerReconciler) prune(ctx context.Context, s *shared, server *techv1alpha1.TechnitiumServer,
	c technitium.Client, list technitium.List, pending *int) error {
	// The managed set comes from the cache. If it is not synced, it is
	// incomplete and every entry would look unmanaged.
	if r.Cache != nil && !r.Cache.WaitForCacheSync(ctx) {
		return errors.New("not pruning: the cache is not synced")
	}
	items, err := listDomainResources(ctx, r.Client, list, server.Namespace)
	if err != nil {
		return fmt.Errorf("not pruning: %w", err)
	}
	managed := map[string]bool{}
	for _, it := range items {
		// Resources being deleted still count: with Orphan their entry must
		// survive until the resource is gone.
		if spec := specOf(it); spec.ServerRef.Name == server.Name {
			managed[techv1alpha1.NormalizeDomain(spec.Domain)] = true
		}
	}
	entries, err := c.ListDomains(ctx, list)
	if err != nil {
		return err
	}
	var errs []error
	for _, entry := range entries {
		if managed[entry] {
			continue
		}
		_, err := s.run(ctx, server, pending, "Prune", fmt.Sprintf("delete unmanaged %q from the %s list", entry, list),
			func() error { return c.DeleteDomain(ctx, list, entry) })
		if err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// SetupWithManager sets up the controller with the Manager.
func (r *ServerReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// A domain resource that changes may make an entry unmanaged (or managed),
	// so its server is reconciled for the prune.
	forDomain := handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, o client.Object) []reconcile.Request {
		key := client.ObjectKey{Namespace: o.GetNamespace(), Name: specOf(o).ServerRef.Name}
		var server techv1alpha1.TechnitiumServer
		if err := r.Get(ctx, key, &server); err != nil {
			return nil
		}
		if !server.Spec.PruneUnmanaged.AllowedDomains && !server.Spec.PruneUnmanaged.BlockedDomains {
			return nil
		}
		return []reconcile.Request{{NamespacedName: key}}
	})
	return ctrl.NewControllerManagedBy(mgr).
		For(&techv1alpha1.TechnitiumServer{}).
		Watches(&techv1alpha1.TechnitiumAllowedDomain{}, forDomain).
		Watches(&techv1alpha1.TechnitiumBlockedDomain{}, forDomain).
		Named("technitiumserver").
		Complete(r)
}
