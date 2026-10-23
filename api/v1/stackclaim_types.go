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

package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// StackClaimPhase is a coarse summary of where a StackClaim is in its lifecycle.
// +kubebuilder:validation:Enum=Pending;Bound;Expired;Lost
type StackClaimPhase string

const (
	// StackClaimPending means the claim is waiting for a Stack.
	StackClaimPending StackClaimPhase = "Pending"
	// StackClaimBound means the claim holds a Stack.
	StackClaimBound StackClaimPhase = "Bound"
	// StackClaimExpired means the claim's ttl ran out and its Stack was released.
	StackClaimExpired StackClaimPhase = "Expired"
	// StackClaimLost means the claim's Stack failed or disappeared before the
	// ttl ran out. The claim is never re-bound; create a new one.
	StackClaimLost StackClaimPhase = "Lost"
)

// StackClaim condition types and reasons.
const (
	// StackClaimConditionBound is True while the claim holds a usable Stack.
	// Wait on it with `kubectl wait --for=condition=Bound stackclaim/<name>`.
	StackClaimConditionBound = "Bound"
	// StackClaimConditionTTLClamped is True when the requested ttl exceeded the
	// pool's maxTTL and was shortened.
	StackClaimConditionTTLClamped = "TTLClamped"

	StackClaimReasonWaitingForStack = "WaitingForStack"
	StackClaimReasonPoolNotFound    = "PoolNotFound"
	StackClaimReasonBound           = "Bound"
	StackClaimReasonExpired         = "Expired"
	StackClaimReasonStackLost       = "StackLost"
	StackClaimReasonStackFailed     = "StackFailed"
	StackClaimReasonClamped         = "ExceedsMaxTTL"
	StackClaimReasonWithinMaxTTL    = "WithinMaxTTL"
)

// StackClaimSpec defines the desired state of StackClaim
type StackClaimSpec struct {
	// poolName is the StackPool to take a Stack from.
	// +required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="poolName is immutable"
	PoolName string `json:"poolName"`

	// ttl is how long the claim holds its Stack, counted from the moment it is
	// bound. Defaults to the pool's defaultTTL and is capped by its maxTTL.
	// It may be raised while bound to extend the lease.
	// +optional
	// +kubebuilder:validation:XValidation:rule="duration(self) > duration('0s')",message="ttl must be positive"
	TTL *metav1.Duration `json:"ttl,omitempty"`
}

// StackClaimStatus defines the observed state of StackClaim.
type StackClaimStatus struct {
	// phase summarises the claim's lifecycle: Pending, Bound, Expired or Lost.
	// +optional
	Phase StackClaimPhase `json:"phase,omitempty"`

	// stackName is the Stack bound to this claim. Once set it never changes.
	// +optional
	StackName string `json:"stackName,omitempty"`

	// boundAt is when the claim was bound to its Stack.
	// +optional
	BoundAt *metav1.Time `json:"boundAt,omitempty"`

	// expiresAt is when the claim's Stack will be torn down.
	// +optional
	ExpiresAt *metav1.Time `json:"expiresAt,omitempty"`

	// conditions represent the current state of the StackClaim resource.
	//
	// - Bound: the claim holds a usable Stack.
	// - TTLClamped: the requested ttl was shortened to the pool's maxTTL.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:selectablefield:JSONPath=".spec.poolName"
// +kubebuilder:printcolumn:name="Pool",type=string,JSONPath=".spec.poolName"
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Stack",type=string,JSONPath=".status.stackName"
// +kubebuilder:printcolumn:name="Expires",type=string,JSONPath=".status.expiresAt"
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"

// StackClaim leases a Stack from a StackPool for a limited time. Deleting the
// claim releases the Stack, which is then torn down.
type StackClaim struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of StackClaim
	// +required
	Spec StackClaimSpec `json:"spec"`

	// status defines the observed state of StackClaim
	// +optional
	Status StackClaimStatus `json:"status,omitzero"`
}

// IsFinished reports whether the claim reached a terminal phase.
func (c *StackClaim) IsFinished() bool {
	return c.Status.Phase == StackClaimExpired || c.Status.Phase == StackClaimLost
}

// IsPending reports whether the claim is still waiting to be bound to a Stack.
func (c *StackClaim) IsPending() bool {
	return c.DeletionTimestamp.IsZero() && c.Status.StackName == "" && !c.IsFinished()
}

// +kubebuilder:object:root=true

// StackClaimList contains a list of StackClaim
type StackClaimList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []StackClaim `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &StackClaim{}, &StackClaimList{})
		return nil
	})
}
