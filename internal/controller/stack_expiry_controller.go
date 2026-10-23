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
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/events"
	"k8s.io/utils/clock"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	platformv1 "github.com/samueltorres/stack-operator/api/v1"
)

// StackExpiryReconciler deletes Stacks once their spec.expiresAt has passed.
// It is the single place where Stacks expire, whether they were created by
// hand or are held by a StackClaim.
type StackExpiryReconciler struct {
	client.Client
	Clock    clock.PassiveClock
	Recorder events.EventRecorder
}

// +kubebuilder:rbac:groups=platform.my.domain,resources=stacks,verbs=get;list;watch;delete
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch

// Reconcile deletes the Stack if it has expired, or schedules itself for the
// moment it will.
func (r *StackExpiryReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var stack platformv1.Stack
	if err := r.Get(ctx, req.NamespacedName, &stack); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if stack.Spec.ExpiresAt == nil || !stack.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	if remaining := stack.Spec.ExpiresAt.Sub(r.Clock.Now()); remaining > 0 {
		return ctrl.Result{RequeueAfter: remaining}, nil
	}

	// The UID precondition guards against deleting a newer Stack that reused the name.
	if err := r.Delete(ctx, &stack, client.Preconditions{UID: &stack.UID}); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	log.Info("Deleted expired Stack", "expiresAt", stack.Spec.ExpiresAt.Time)
	r.Recorder.Eventf(&stack, nil, corev1.EventTypeNormal, "Expired", "Delete",
		"Stack expired at %s", stack.Spec.ExpiresAt.UTC().Format(time.RFC3339))
	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *StackExpiryReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&platformv1.Stack{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Named("stack-expiry").
		Complete(r)
}
