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

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/clock"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	platformv1 "github.com/samueltorres/stack-operator/api/v1"
)

// AnnotationSimulateFailure makes the simulator fail a Stack's provisioning
// instead of completing it.
const AnnotationSimulateFailure = "platform.my.domain/simulate-failure"

// StackReconciler simulates a Stack provisioner. It does not create any
// infrastructure: it walks each Stack through the Ready condition contract
// (Provisioning, then Provisioned or Failed) after ProvisioningDelay, so that
// pools, claims and expiry can be exercised end to end.
//
// The real provisioner replaces this reconciler.
type StackReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	Clock  clock.PassiveClock

	// ProvisioningDelay is how long a simulated Stack takes to provision.
	ProvisioningDelay time.Duration
}

// +kubebuilder:rbac:groups=platform.my.domain,resources=stacks,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=platform.my.domain,resources=stacks/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=platform.my.domain,resources=stacks/finalizers,verbs=update

// Reconcile advances a Stack's simulated provisioning.
func (r *StackReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var stack platformv1.Stack
	if err := r.Get(ctx, req.NamespacedName, &stack); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !stack.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	ready := meta.FindStatusCondition(stack.Status.Conditions, platformv1.StackConditionReady)
	if ready != nil && ready.Reason != platformv1.StackReasonProvisioning {
		return ctrl.Result{}, nil // Provisioned or Failed: nothing left to simulate.
	}

	base := stack.DeepCopy()
	if ready == nil {
		meta.SetStatusCondition(&stack.Status.Conditions, metav1.Condition{
			Type:               platformv1.StackConditionReady,
			Status:             metav1.ConditionFalse,
			Reason:             platformv1.StackReasonProvisioning,
			Message:            "Simulating infrastructure provisioning",
			ObservedGeneration: stack.Generation,
			LastTransitionTime: now(r.Clock),
		})
		if err := r.Status().Patch(ctx, &stack, client.MergeFrom(base)); err != nil {
			return ctrl.Result{}, err
		}
		log.V(1).Info("Started provisioning Stack")
		return ctrl.Result{RequeueAfter: r.ProvisioningDelay}, nil
	}

	if remaining := r.ProvisioningDelay - r.Clock.Since(ready.LastTransitionTime.Time); remaining > 0 {
		return ctrl.Result{RequeueAfter: remaining}, nil
	}

	done := metav1.Condition{
		Type:               platformv1.StackConditionReady,
		Status:             metav1.ConditionTrue,
		Reason:             platformv1.StackReasonProvisioned,
		Message:            "Simulated infrastructure is ready",
		ObservedGeneration: stack.Generation,
		LastTransitionTime: now(r.Clock),
	}
	if stack.Annotations[AnnotationSimulateFailure] == "true" {
		done.Status = metav1.ConditionFalse
		done.Reason = platformv1.StackReasonFailed
		done.Message = "Simulated provisioning failure"
	}
	meta.SetStatusCondition(&stack.Status.Conditions, done)
	if err := r.Status().Patch(ctx, &stack, client.MergeFrom(base)); err != nil {
		return ctrl.Result{}, err
	}
	log.Info("Finished provisioning Stack", "reason", done.Reason)
	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *StackReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&platformv1.Stack{}).
		Named("stack").
		Complete(r)
}
