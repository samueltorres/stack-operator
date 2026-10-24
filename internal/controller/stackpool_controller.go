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
	"math"
	"sync"
	"time"

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
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	platformv1 "github.com/samueltorres/stack-operator/api/v1"
)

const (
	defaultFailureThreshold  = 3
	defaultFailureBackoff    = 30 * time.Second
	defaultMaxFailureBackoff = 10 * time.Minute

	// expectationsRequeue is a safety net for when the cache event that would
	// normally retrigger a waiting pool never arrives.
	expectationsRequeue = 5 * time.Second
)

// StackPoolReconciler keeps each StackPool stocked with warm Stacks and binds
// pending StackClaims to them.
//
// It is the only component that allocates Stacks, and controller-runtime never
// reconciles the same pool concurrently, so allocation within a pool is
// serialized. The binding protocol (see bind) stays correct even when the
// cache is stale or another replica races it: a Stack can never be handed to
// two claims, and a claim never ends up holding two Stacks.
type StackPoolReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Clock    clock.PassiveClock
	Recorder events.EventRecorder

	// FailureThreshold is how many Stacks from the current template may fail
	// to provision in a row before the pool stops provisioning.
	FailureThreshold int32
	// FailureBackoff is how long the pool waits after the first failure before
	// provisioning again. It doubles with each further failure.
	FailureBackoff time.Duration
	// MaxFailureBackoff caps FailureBackoff.
	MaxFailureBackoff time.Duration

	initOnce     sync.Once
	expectations *expectations
}

// +kubebuilder:rbac:groups=platform.my.domain,resources=stackpools,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=platform.my.domain,resources=stackpools/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=platform.my.domain,resources=stackpools/finalizers,verbs=update
// +kubebuilder:rbac:groups=platform.my.domain,resources=stackclaims,verbs=get;list;watch
// +kubebuilder:rbac:groups=platform.my.domain,resources=stackclaims/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=platform.my.domain,resources=stacks,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch

// Reconcile drives a StackPool towards its desired number of warm Stacks and
// hands Stacks out to its pending claims, oldest claim first.
func (r *StackPoolReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	r.init()
	log := logf.FromContext(ctx)

	var pool platformv1.StackPool
	if err := r.Get(ctx, req.NamespacedName, &pool); err != nil {
		if apierrors.IsNotFound(err) {
			r.expectations.forget(req.Name)
			forgetPoolMetrics(req.Name)
		}
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !pool.DeletionTimestamp.IsZero() {
		// Owner references cascade the deletion to every Stack in the pool.
		return ctrl.Result{}, nil
	}

	stacks, err := r.stacksOf(ctx, &pool)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("listing Stacks: %w", err)
	}
	claims, err := claimsInPool(ctx, r.Client, pool.Name)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("listing StackClaims: %w", err)
	}

	hash := templateHash(&pool.Spec.Template)
	view := classify(hash, stacks, claims)
	base := pool.DeepCopy()
	if pool.Status.TemplateHash != hash {
		// A new template deserves a fresh start.
		pool.Status.ConsecutiveFailures, pool.Status.LastFailureTime = 0, nil
	}
	untilWarmChange := r.setDesiredWarm(ctx, &pool)

	// Bindings interrupted half-way only need the claim to record its Stack.
	// This is safe whatever the cache state, so it always runs.
	if err := r.completeBindings(ctx, view); err != nil {
		return ctrl.Result{}, err
	}

	// Everything below acts on counts. Acting on a cache that has not caught up
	// with our own creations and deletions would over-provision, so wait.
	if !r.expectations.satisfied(pool.Name, stacks) {
		log.V(1).Info("Waiting for the cache to observe earlier Stack changes")
		return ctrl.Result{RequeueAfter: expectationsRequeue}, r.updateStatus(ctx, base, &pool, hash, view)
	}

	if err := r.deleteOrphans(ctx, &pool, view); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.bindPendingClaims(ctx, &pool, view); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.recycleFailedStacks(ctx, &pool, hash, view); err != nil {
		return ctrl.Result{}, err
	}

	backoff := r.provisioningBackoff(&pool)
	plan := planScaling(view, pool.Status.DesiredWarm, pool.Spec.Max, backoff == 0 && !r.isDegraded(&pool))
	if err := r.applyScaling(ctx, &pool, hash, view, plan); err != nil {
		return ctrl.Result{}, err
	}

	if err := r.updateStatus(ctx, base, &pool, hash, view); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: soonest(backoff, untilWarmChange)}, nil
}

// setDesiredWarm records the warm target in force on the pool's status, and
// returns how long until it changes, or zero if it never does.
func (r *StackPoolReconciler) setDesiredWarm(ctx context.Context, pool *platformv1.StackPool) time.Duration {
	log := logf.FromContext(ctx)
	now := r.Clock.Now()

	target, err := resolveWarm(&pool.Spec, now)
	if err != nil {
		// API validation rejects bad schedules, so the operator's time zone
		// database must lack the pool's zone. Keep to spec.warm meanwhile.
		log.Error(err, "Failed to evaluate warm schedules, falling back to spec.warm")
		r.Recorder.Eventf(pool, nil, corev1.EventTypeWarning, "InvalidWarmSchedule", "Schedule",
			"Ignoring warm schedules: %v", err)
	}

	status := &pool.Status
	if target.schedule != status.ActiveWarmSchedule {
		source := "spec.warm"
		if target.schedule != "" {
			source = "warm schedule " + target.schedule
		}
		log.Info("Changed warm target", "warm", target.warm, "schedule", target.schedule)
		r.Recorder.Eventf(pool, nil, corev1.EventTypeNormal, "WarmTargetChanged", "Schedule",
			"Keeping %d warm Stacks, as set by %s", target.warm, source)
	}
	status.DesiredWarm, status.ActiveWarmSchedule = target.warm, target.schedule

	if target.until.IsZero() {
		return 0
	}
	return target.until.Sub(now)
}

// soonest returns the shortest of the positive delays, or zero if none is.
func soonest(delays ...time.Duration) time.Duration {
	var d time.Duration
	for _, delay := range delays {
		if delay > 0 && (d == 0 || delay < d) {
			d = delay
		}
	}
	return d
}

// stacksOf lists the Stacks the pool controls.
func (r *StackPoolReconciler) stacksOf(ctx context.Context, pool *platformv1.StackPool) ([]platformv1.Stack, error) {
	var list platformv1.StackList
	if err := r.List(ctx, &list, client.MatchingLabels{platformv1.LabelPool: pool.Name}); err != nil {
		return nil, err
	}
	stacks := list.Items[:0]
	for _, s := range list.Items {
		// A predecessor pool with the same name may have left Stacks behind.
		if metav1.IsControlledBy(&s, pool) {
			stacks = append(stacks, s)
		}
	}
	return stacks, nil
}

// completeBindings records each half-bound Stack on the claim it points at.
func (r *StackPoolReconciler) completeBindings(ctx context.Context, view *poolView) error {
	for _, b := range view.halfBound {
		if err := r.recordBinding(ctx, b.claim, b.stack, now(r.Clock)); err != nil {
			return fmt.Errorf("completing binding of StackClaim %s to Stack %s: %w", b.claim.Name, b.stack.Name, err)
		}
	}
	return nil
}

// deleteOrphans tears down Stacks bound to a claim that does not hold them:
// the claim was deleted mid-binding, or a race bound it to another Stack.
// A Stack's claimRef cannot be undone, so these Stacks cannot go back to the
// pool.
func (r *StackPoolReconciler) deleteOrphans(ctx context.Context, pool *platformv1.StackPool, view *poolView) error {
	for _, stack := range view.orphaned {
		if err := r.deleteStack(ctx, pool, stack); err != nil {
			return err
		}
		logf.FromContext(ctx).Info("Deleted Stack bound to a StackClaim that does not hold it",
			"stack", stack.Name, "claim", stack.Spec.ClaimRef.Name)
	}
	view.orphaned = nil
	return nil
}

// bindPendingClaims serves pending claims, oldest first, from the pool's ready
// Stacks. Stacks built from the current template are preferred; during a
// rollout, Stacks from the previous template still serve claims rather than
// leaving CI waiting for provisioning.
func (r *StackPoolReconciler) bindPendingClaims(ctx context.Context, pool *platformv1.StackPool, view *poolView) error {
	for len(view.pending) > 0 {
		var stack *platformv1.Stack
		switch {
		case len(view.ready) > 0:
			stack, view.ready = view.ready[0], view.ready[1:]
		case len(view.staleReady) > 0:
			stack, view.staleReady = view.staleReady[0], view.staleReady[1:]
		default:
			return nil
		}
		claim := view.pending[0]
		view.pending = view.pending[1:]

		if err := r.bind(ctx, pool, claim, stack); err != nil {
			return fmt.Errorf("binding StackClaim %s to Stack %s: %w", claim.Name, stack.Name, err)
		}
		view.claimed = append(view.claimed, stack)
	}
	return nil
}

// bind hands the Stack to the claim in two writes, each conditional on the
// object being unchanged since it was read.
//
// The Stack goes first. Its claimRef is the source of truth and can be set
// only once, so a Stack can never go to two claims: a stale view of the Stack
// fails with a conflict and nothing has changed.
//
// The claim then records the Stack. A stale view of the claim fails here, after
// the Stack was already taken; the next pass sees a Stack pointing at a claim
// that holds another one and tears it down (see classify). If the process dies
// between the writes, the next pass completes the binding.
func (r *StackPoolReconciler) bind(ctx context.Context, pool *platformv1.StackPool, claim *platformv1.StackClaim, stack *platformv1.Stack) error {
	boundAt := now(r.Clock)
	expiresAt := effectiveTTL(claim, pool).expiresAt(boundAt)

	base := stack.DeepCopy()
	stack.Spec.ClaimRef = &platformv1.ClaimReference{Name: claim.Name, UID: claim.UID}
	stack.Spec.ExpiresAt = &expiresAt
	if err := r.Patch(ctx, stack, optimisticMergeFrom(base)); err != nil {
		return err
	}
	if err := r.recordBinding(ctx, claim, stack, boundAt); err != nil {
		return err
	}

	claimWaitSeconds.WithLabelValues(pool.Name).Observe(boundAt.Sub(claim.CreationTimestamp.Time).Seconds())
	logf.FromContext(ctx).Info("Bound StackClaim to Stack", "claim", claim.Name, "stack", stack.Name, "expiresAt", expiresAt)
	r.Recorder.Eventf(claim, stack, corev1.EventTypeNormal, "Bound", "Bind", "Bound to Stack %s", stack.Name)
	return nil
}

// recordBinding records on the claim the Stack that points at it. From here
// on the StackClaim controller keeps the lease in line with the claim's ttl.
func (r *StackPoolReconciler) recordBinding(ctx context.Context, claim *platformv1.StackClaim, stack *platformv1.Stack, boundAt metav1.Time) error {
	base := claim.DeepCopy()
	claim.Status.StackName = stack.Name
	claim.Status.BoundAt = &boundAt
	claim.Status.ExpiresAt = stack.Spec.ExpiresAt
	return r.Status().Patch(ctx, claim, optimisticMergeFrom(base))
}

// recycleFailedStacks deletes unclaimed Stacks that failed to provision and
// tracks consecutive failures of the current template, which drive the
// provisioning backoff and the circuit breaker.
func (r *StackPoolReconciler) recycleFailedStacks(ctx context.Context, pool *platformv1.StackPool, hash string, view *poolView) error {
	status := &pool.Status
	if status.LastFailureTime != nil && view.readySince(hash, status.LastFailureTime.Time) {
		// A Stack provisioned fine after the last failure: the template works.
		status.ConsecutiveFailures, status.LastFailureTime = 0, nil
	}

	for _, stack := range view.failed {
		if err := r.deleteStack(ctx, pool, stack); err != nil {
			return err
		}
		logf.FromContext(ctx).Info("Deleted Stack that failed to provision", "stack", stack.Name)
		r.Recorder.Eventf(pool, stack, corev1.EventTypeWarning, "StackFailed", "Delete",
			"Deleted Stack %s, which failed to provision", stack.Name)

		if stack.Labels[platformv1.LabelTemplateHash] == hash {
			status.ConsecutiveFailures++
			status.LastFailureTime = new(now(r.Clock))
		}
	}
	view.failed = nil

	if r.isDegraded(pool) && !meta.IsStatusConditionTrue(status.Conditions, platformv1.StackPoolConditionDegraded) {
		r.Recorder.Eventf(pool, nil, corev1.EventTypeWarning, "ProvisioningHalted", "Provision",
			"Halted provisioning after %d consecutive failures", status.ConsecutiveFailures)
	}
	return nil
}

// isDegraded reports whether the circuit breaker tripped.
func (r *StackPoolReconciler) isDegraded(pool *platformv1.StackPool) bool {
	return pool.Status.ConsecutiveFailures >= r.FailureThreshold
}

// provisioningBackoff returns how long the pool must still wait before
// provisioning again after recent failures, or zero if it may provision now.
func (r *StackPoolReconciler) provisioningBackoff(pool *platformv1.StackPool) time.Duration {
	failures := pool.Status.ConsecutiveFailures
	if failures == 0 || pool.Status.LastFailureTime == nil {
		return 0
	}
	backoff := r.FailureBackoff * time.Duration(math.Pow(2, float64(failures-1)))
	if backoff > r.MaxFailureBackoff || backoff <= 0 {
		backoff = r.MaxFailureBackoff
	}
	return max(0, backoff-r.Clock.Since(pool.Status.LastFailureTime.Time))
}

// applyScaling carries out a scaling plan.
func (r *StackPoolReconciler) applyScaling(ctx context.Context, pool *platformv1.StackPool, hash string, view *poolView, plan scalePlan) error {
	log := logf.FromContext(ctx)

	for _, stack := range plan.delete {
		if err := r.deleteStack(ctx, pool, stack); err != nil {
			return err
		}
		view.remove(stack)
		log.Info("Deleted surplus Stack", "stack", stack.Name)
	}

	for range plan.create {
		stack, err := r.newStack(pool, hash)
		if err != nil {
			return err
		}
		if err := r.Create(ctx, stack); err != nil {
			return fmt.Errorf("creating Stack: %w", err)
		}
		r.expectations.expectCreate(pool.Name, stack.Name)
		view.provisioning = append(view.provisioning, stack)
		log.Info("Created Stack", "stack", stack.Name)
		r.Recorder.Eventf(pool, stack, corev1.EventTypeNormal, "Created", "Provision", "Created Stack %s", stack.Name)
	}
	return nil
}

// newStack builds a Stack from the pool's template.
func (r *StackPoolReconciler) newStack(pool *platformv1.StackPool, hash string) (*platformv1.Stack, error) {
	tmpl := pool.Spec.Template.DeepCopy()

	labels := tmpl.Metadata.Labels
	if labels == nil {
		labels = map[string]string{}
	}
	labels[platformv1.LabelPool] = pool.Name
	labels[platformv1.LabelTemplateHash] = hash

	stack := &platformv1.Stack{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: pool.Name + "-",
			Labels:       labels,
			Annotations:  tmpl.Metadata.Annotations,
		},
		Spec: tmpl.Spec,
	}
	if err := controllerutil.SetControllerReference(pool, stack, r.Scheme); err != nil {
		return nil, err
	}
	return stack, nil
}

// deleteStack deletes a Stack and remembers to wait for the cache to see it go.
func (r *StackPoolReconciler) deleteStack(ctx context.Context, pool *platformv1.StackPool, stack *platformv1.Stack) error {
	if err := r.Delete(ctx, stack, client.Preconditions{UID: &stack.UID}); client.IgnoreNotFound(err) != nil {
		return fmt.Errorf("deleting Stack %s: %w", stack.Name, err)
	}
	r.expectations.expectDelete(pool.Name, stack.UID)
	return nil
}

// updateStatus publishes the pool's state, as left by this reconcile.
func (r *StackPoolReconciler) updateStatus(ctx context.Context, base, pool *platformv1.StackPool, hash string, view *poolView) error {
	status := &pool.Status
	status.ObservedGeneration = pool.Generation
	status.TemplateHash = hash
	status.Warm = int32(len(view.ready) + len(view.staleReady))
	status.Provisioning = int32(len(view.provisioning) + len(view.staleProvisioning))
	status.Claimed = int32(len(view.claimed))
	status.PendingClaims = int32(len(view.pending))

	ready := metav1.Condition{
		Type:    platformv1.StackPoolConditionReady,
		Status:  metav1.ConditionTrue,
		Reason:  platformv1.StackPoolReasonWarm,
		Message: fmt.Sprintf("%d of %d warm Stacks ready", len(view.ready), status.DesiredWarm),
	}
	if int32(len(view.ready)) < status.DesiredWarm {
		ready.Status, ready.Reason = metav1.ConditionFalse, platformv1.StackPoolReasonWarmingUp
	}

	degraded := metav1.Condition{
		Type:    platformv1.StackPoolConditionDegraded,
		Status:  metav1.ConditionFalse,
		Reason:  platformv1.StackPoolReasonProvisioningHealthy,
		Message: "Stacks are provisioning successfully",
	}
	switch backoff := r.provisioningBackoff(pool); {
	case r.isDegraded(pool):
		degraded.Status, degraded.Reason = metav1.ConditionTrue, platformv1.StackPoolReasonProvisioningFailing
		degraded.Message = fmt.Sprintf("Provisioning halted after %d consecutive failures; "+
			"change spec.template (for example by bumping an annotation) to try again", status.ConsecutiveFailures)
	case backoff > 0:
		degraded.Message = fmt.Sprintf("Backing off for %s after %d consecutive failures",
			backoff.Round(time.Second), status.ConsecutiveFailures)
	}

	for _, c := range []metav1.Condition{ready, degraded} {
		c.ObservedGeneration = pool.Generation
		meta.SetStatusCondition(&status.Conditions, c)
	}

	recordPoolMetrics(pool)
	if equality.Semantic.DeepEqual(base.Status, pool.Status) {
		return nil
	}
	return r.Status().Patch(ctx, pool, optimisticMergeFrom(base))
}

func (r *StackPoolReconciler) init() {
	r.initOnce.Do(func() {
		if r.FailureThreshold == 0 {
			r.FailureThreshold = defaultFailureThreshold
		}
		if r.FailureBackoff == 0 {
			r.FailureBackoff = defaultFailureBackoff
		}
		if r.MaxFailureBackoff == 0 {
			r.MaxFailureBackoff = defaultMaxFailureBackoff
		}
		r.expectations = newExpectations(r.Clock)
	})
}

// SetupWithManager sets up the controller with the Manager.
func (r *StackPoolReconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.init()
	return ctrl.NewControllerManagedBy(mgr).
		For(&platformv1.StackPool{}).
		Owns(&platformv1.Stack{}).
		Watches(&platformv1.StackClaim{}, handler.EnqueueRequestsFromMapFunc(
			func(_ context.Context, obj client.Object) []reconcile.Request {
				return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: obj.(*platformv1.StackClaim).Spec.PoolName}}}
			})).
		Named("stackpool").
		Complete(r)
}
