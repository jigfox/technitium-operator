// Package controller holds the reconcilers of the Technitium resources.
package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	techv1alpha1 "github.com/jigfox/technitium-operator/api/v1alpha1"
	"github.com/jigfox/technitium-operator/internal/technitium"
)

const (
	// Finalizer is set on every domain resource so Technitium is cleaned up
	// first.
	Finalizer = "technitium.jigfox.github.io/finalizer"

	// DefaultInterval is how often a resource is re-checked for drift (changes
	// made in the Technitium UI raise no event).
	DefaultInterval = 5 * time.Minute

	// retryInterval is how soon a resource is retried after a problem only a
	// change elsewhere (a Secret, a TechnitiumServer) can fix.
	retryInterval = time.Minute

	reasonDryRun = "DryRun"
)

// ClientProvider hands out a Technitium client for a TechnitiumServer.
type ClientProvider interface {
	// ClientFor returns the client for the server's current URL and
	// credentials. The client is shared and must not be closed by the caller.
	// Problems with the credentials are returned as *CredentialsError.
	ClientFor(ctx context.Context, server *techv1alpha1.TechnitiumServer) (technitium.Client, error)
	// Forget closes and drops the client of a server that no longer exists.
	Forget(ctx context.Context, key types.NamespacedName)
}

// CredentialsError means the Secret named by a TechnitiumServer cannot be used.
type CredentialsError struct{ Err error }

func (e *CredentialsError) Error() string { return e.Err.Error() }
func (e *CredentialsError) Unwrap() error { return e.Err }

// shared holds what the reconcilers need in common.
type shared struct {
	Recorder events.EventRecorder
	// DryRun computes and reports every change but sends no mutation.
	DryRun bool
	// Interval is the periodic resync used to correct drift.
	Interval time.Duration
}

func (s *shared) interval() time.Duration {
	if s.Interval <= 0 {
		return DefaultInterval
	}
	return s.Interval
}

// run performs one mutation, or in dry-run mode only logs it and emits an
// Event. It returns whether the mutation was really sent; skipped mutations are
// counted in pending.
func (s *shared) run(ctx context.Context, obj client.Object, pending *int, reason, msg string,
	fn func() error) (bool, error) {
	log := logf.FromContext(ctx)
	if s.DryRun {
		*pending++
		log.Info("dry-run: would "+msg, "action", reason)
		s.Recorder.Eventf(obj, nil, corev1.EventTypeNormal, reasonDryRun, reason, "DryRun: would %s", msg)
		return false, nil
	}
	if err := fn(); err != nil {
		return false, fmt.Errorf("%s: %w", msg, err)
	}
	log.Info(msg, "action", reason)
	s.Recorder.Eventf(obj, nil, corev1.EventTypeNormal, reason, reason, "%s", capitalize(msg))
	return true, nil
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// setConditions writes Ready and (in dry-run) DryRun on the conditions slice.
func (s *shared) setConditions(conds *[]metav1.Condition, generation int64, ready bool,
	reason, msg string, pending int) {
	if s.DryRun && pending > 0 {
		ready, reason = false, techv1alpha1.ReasonDryRunPending
		msg = fmt.Sprintf("dry-run: %d change(s) not applied", pending)
	}
	status := metav1.ConditionFalse
	if ready {
		status = metav1.ConditionTrue
	}
	meta.SetStatusCondition(conds, metav1.Condition{
		Type: techv1alpha1.ConditionReady, Status: status, Reason: reason, Message: msg,
		ObservedGeneration: generation,
	})
	if !s.DryRun {
		meta.RemoveStatusCondition(conds, techv1alpha1.ConditionDryRun)
		return
	}
	dryMsg := "dry-run: no changes needed"
	if pending > 0 {
		dryMsg = fmt.Sprintf("dry-run: %d change(s) would be made", pending)
	}
	meta.SetStatusCondition(conds, metav1.Condition{
		Type: techv1alpha1.ConditionDryRun, Status: metav1.ConditionTrue, Reason: techv1alpha1.ReasonDryRun,
		Message: dryMsg, ObservedGeneration: generation,
	})
}
