/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"k8s.io/utils/clock"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	platformv1 "github.com/samueltorres/stack-operator/api/v1"
)

// claimFinalizer makes deleting a StackClaim release its Stack. A cluster-scoped
// Stack cannot rely on garbage collection here: it is already owned by its pool,
// and dependents are only collected once all of their owners are gone.
const claimFinalizer = "platform.my.domain/release-stack"

// StackClaimReconciler runs a StackClaim's lifecycle once the StackPool
// controller has bound it: it keeps the lease's expiry in line with the
// claim's ttl, reports the claim's phase, and releases the Stack when the
// claim is deleted.
type StackClaimReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Clock    clock.PassiveClock
	Recorder events.EventRecorder
}

// +kubebuilder:rbac:groups=platform.my.domain,resources=stackclaims,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=platform.my.domain,resources=stackclaims/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=platform.my.domain,resources=stackclaims/finalizers,verbs=update
// +kubebuilder:rbac:groups=platform.my.domain,resources=stackpools,verbs=get;list;watch
// +kubebuilder:rbac:groups=platform.my.domain,resources=stacks,verbs=get;list;watch;patch;delete
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch

// Reconcile moves a StackClaim through Pending, Bound and finally Expired or Lost.
func (r *StackClaimReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var claim platformv1.StackClaim
	if err := r.Get(ctx, req.NamespacedName, &claim); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !claim.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, r.release(ctx, &claim)
	}
	if controllerutil.AddFinalizer(&claim, claimFinalizer) {
		if err := r.Update(ctx, &claim); err != nil {
			return ctrl.Result{}, err
		}
	}
	if claim.IsFinished() {
		return ctrl.Result{}, nil // Terminal: the claim never comes back.
	}

	pool, err := r.poolOf(ctx, &claim)
	if err != nil {
		return ctrl.Result{}, err
	}
	stack, err := r.stackOf(ctx, &claim)
	if err != nil {
		return ctrl.Result{}, err
	}

	base := claim.DeepCopy()
	if stack != nil && stack.IsClaimedBy(&claim) && pool != nil {
		if err := r.syncLease(ctx, &claim, pool, stack); err != nil {
			return ctrl.Result{}, err
		}
	}
	r.setPhase(ctx, &claim, pool, stack)

	if !equality.Semantic.DeepEqual(base.Status, claim.Status) {
		if err := r.Status().Patch(ctx, &claim, optimisticMergeFrom(base)); err != nil {
			return ctrl.Result{}, err
		}
	}

	if claim.Status.Phase == platformv1.StackClaimBound {
		return ctrl.Result{RequeueAfter: claim.Status.ExpiresAt.Sub(r.Clock.Now())}, nil
	}
	return ctrl.Result{}, nil
}

// poolOf returns the claim's pool, or nil if it does not exist.
func (r *StackClaimReconciler) poolOf(ctx context.Context, claim *platformv1.StackClaim) (*platformv1.StackPool, error) {
	var pool platformv1.StackPool
	if err := r.Get(ctx, types.NamespacedName{Name: claim.Spec.PoolName}, &pool); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("getting StackPool %s: %w", claim.Spec.PoolName, err)
	}
	return &pool, nil
}

// stackOf returns the Stack the claim reserved, or nil if it has none or the
// Stack is gone.
func (r *StackClaimReconciler) stackOf(ctx context.Context, claim *platformv1.StackClaim) (*platformv1.Stack, error) {
	if claim.Status.StackName == "" {
		return nil, nil
	}
	var stack platformv1.Stack
	if err := r.Get(ctx, types.NamespacedName{Name: claim.Status.StackName}, &stack); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("getting Stack %s: %w", claim.Status.StackName, err)
	}
	return &stack, nil
}

// syncLease applies the claim's ttl, as allowed by its pool, to the lease. A
// raised ttl extends the lease; it is always counted from when the claim was
// bound. The Stack's expiresAt is what actually tears it down, so it is kept in
// line, overriding any manual edit.
func (r *StackClaimReconciler) syncLease(ctx context.Context, claim *platformv1.StackClaim, pool *platformv1.StackPool, stack *platformv1.Stack) error {
	ttl := effectiveTTL(claim, pool)
	expiresAt := ttl.expiresAt(*claim.Status.BoundAt)
	claim.Status.ExpiresAt = &expiresAt

	clamped := metav1.Condition{
		Type:               platformv1.StackClaimConditionTTLClamped,
		Status:             metav1.ConditionFalse,
		Reason:             platformv1.StackClaimReasonWithinMaxTTL,
		Message:            fmt.Sprintf("Holding the Stack for %s", ttl.duration),
		ObservedGeneration: claim.Generation,
	}
	if ttl.clamped {
		clamped.Status, clamped.Reason = metav1.ConditionTrue, platformv1.StackClaimReasonClamped
		clamped.Message = fmt.Sprintf("Requested ttl %s exceeds the pool's maxTTL; holding the Stack for %s",
			claim.Spec.TTL.Duration, ttl.duration)
	}
	meta.SetStatusCondition(&claim.Status.Conditions, clamped)

	if stack.Spec.ExpiresAt.Equal(&expiresAt) || !stack.DeletionTimestamp.IsZero() {
		return nil
	}
	base := stack.DeepCopy()
	stack.Spec.ExpiresAt = &expiresAt
	if err := r.Patch(ctx, stack, optimisticMergeFrom(base)); err != nil {
		return fmt.Errorf("updating expiry of Stack %s: %w", stack.Name, err)
	}
	logf.FromContext(ctx).Info("Updated Stack expiry", "stack", stack.Name, "expiresAt", expiresAt)
	return nil
}

// setPhase derives the claim's phase and Bound condition from its Stack.
func (r *StackClaimReconciler) setPhase(ctx context.Context, claim *platformv1.StackClaim, pool *platformv1.StackPool, stack *platformv1.Stack) {
	phase, reason, message := platformv1.StackClaimPending, platformv1.StackClaimReasonWaitingForStack,
		fmt.Sprintf("Waiting for a Stack from StackPool %s", claim.Spec.PoolName)

	expired := claim.Status.ExpiresAt != nil && !r.Clock.Now().Before(claim.Status.ExpiresAt.Time)
	switch {
	case claim.Status.StackName == "" && pool == nil:
		reason, message = platformv1.StackClaimReasonPoolNotFound, fmt.Sprintf("StackPool %s does not exist", claim.Spec.PoolName)
	case claim.Status.StackName == "":
		// Pending: waiting for the pool to hand out a Stack.
	case expired:
		phase, reason, message = platformv1.StackClaimExpired, platformv1.StackClaimReasonExpired,
			fmt.Sprintf("Lease on Stack %s expired", claim.Status.StackName)
	case stack == nil || !stack.DeletionTimestamp.IsZero() || !stack.IsClaimedBy(claim):
		phase, reason, message = platformv1.StackClaimLost, platformv1.StackClaimReasonStackLost,
			fmt.Sprintf("Stack %s was deleted before the lease expired", claim.Status.StackName)
	case stack.HasFailed():
		phase, reason, message = platformv1.StackClaimLost, platformv1.StackClaimReasonStackFailed,
			fmt.Sprintf("Stack %s failed", stack.Name)
	default:
		phase, reason, message = platformv1.StackClaimBound, platformv1.StackClaimReasonBound,
			fmt.Sprintf("Bound to Stack %s", stack.Name)
	}

	bound := metav1.ConditionFalse
	if phase == platformv1.StackClaimBound {
		bound = metav1.ConditionTrue
	}
	meta.SetStatusCondition(&claim.Status.Conditions, metav1.Condition{
		Type:               platformv1.StackClaimConditionBound,
		Status:             bound,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: claim.Generation,
	})

	if phase != claim.Status.Phase && claim.Status.Phase != "" {
		logf.FromContext(ctx).Info("StackClaim changed phase", "from", claim.Status.Phase, "to", phase, "reason", reason)
		eventType := corev1.EventTypeNormal
		if phase == platformv1.StackClaimLost {
			eventType = corev1.EventTypeWarning
		}
		r.Recorder.Eventf(claim, stack, eventType, reason, "Lease", message)
	}
	claim.Status.Phase = phase
}

// release tears down the claim's Stack and lets the claim go.
func (r *StackClaimReconciler) release(ctx context.Context, claim *platformv1.StackClaim) error {
	if !controllerutil.ContainsFinalizer(claim, claimFinalizer) {
		return nil
	}

	stack, err := r.stackOf(ctx, claim)
	if err != nil {
		return err
	}
	// A Stack that points at the claim before the claim recorded it is torn
	// down by its pool once the claim is gone.
	if stack != nil && stack.IsClaimedBy(claim) && stack.DeletionTimestamp.IsZero() {
		if err := r.Delete(ctx, stack, client.Preconditions{UID: &stack.UID}); client.IgnoreNotFound(err) != nil {
			return fmt.Errorf("deleting Stack %s: %w", stack.Name, err)
		}
		logf.FromContext(ctx).Info("Released Stack", "stack", stack.Name)
		r.Recorder.Eventf(claim, stack, corev1.EventTypeNormal, "Released", "Release", "Released Stack %s", stack.Name)
	}

	controllerutil.RemoveFinalizer(claim, claimFinalizer)
	return r.Update(ctx, claim)
}

// SetupWithManager sets up the controller with the Manager.
func (r *StackClaimReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&platformv1.StackClaim{}).
		Watches(&platformv1.Stack{}, handler.EnqueueRequestsFromMapFunc(r.claimOfStack)).
		Watches(&platformv1.StackPool{}, handler.EnqueueRequestsFromMapFunc(r.claimsOfPool),
			builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Named("stackclaim").
		Complete(r)
}

// claimOfStack maps a Stack to the claim it is bound to.
func (r *StackClaimReconciler) claimOfStack(_ context.Context, obj client.Object) []reconcile.Request {
	ref := obj.(*platformv1.Stack).Spec.ClaimRef
	if ref == nil {
		return nil
	}
	return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: ref.Name}}}
}

// claimsOfPool maps a StackPool to its claims, whose ttl limits may have changed.
func (r *StackClaimReconciler) claimsOfPool(ctx context.Context, obj client.Object) []reconcile.Request {
	claims, err := claimsInPool(ctx, r.Client, obj.GetName())
	if err != nil {
		logf.FromContext(ctx).Error(err, "Failed to list StackClaims of StackPool", "pool", obj.GetName())
		return nil
	}
	requests := make([]reconcile.Request, 0, len(claims))
	for _, claim := range claims {
		requests = append(requests, reconcile.Request{NamespacedName: types.NamespacedName{Name: claim.Name}})
	}
	return requests
}
