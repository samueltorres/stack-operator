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
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
)

// Contract between the Stack provisioner and everything that consumes Stacks
// (StackPool, StackClaim, expiry). The provisioner reports progress through the
// Ready condition; consumers never look at provisioner-specific status.
const (
	// StackConditionReady is True once the Stack's infrastructure is usable.
	StackConditionReady = "Ready"

	// StackReasonProvisioning means the infrastructure is still being created.
	StackReasonProvisioning = "Provisioning"
	// StackReasonProvisioned means the infrastructure is up and usable.
	StackReasonProvisioned = "Provisioned"
	// StackReasonFailed means provisioning failed terminally and will not be retried.
	StackReasonFailed = "Failed"
)

// Well-known labels set on Stacks created by a StackPool.
const (
	// LabelPool holds the name of the StackPool that created the Stack.
	LabelPool = "platform.my.domain/pool"
	// LabelTemplateHash holds the hash of the StackPool template the Stack was created from.
	LabelTemplateHash = "platform.my.domain/template-hash"
)

// StackSpec defines the desired state of Stack
type StackSpec struct {
	// foo is an example field of Stack. Edit stack_types.go to remove/update
	// +optional
	Foo *string `json:"foo,omitempty"`

	// expiresAt is the time after which the Stack is deleted and its
	// infrastructure torn down. A Stack without expiresAt never expires.
	// For Stacks bound to a StackClaim this is managed by the claim.
	// +optional
	ExpiresAt *metav1.Time `json:"expiresAt,omitempty"`

	// claimRef binds the Stack to the StackClaim that holds it. It is set by the
	// StackPool controller and cannot be changed or removed once set.
	// +optional
	ClaimRef *ClaimReference `json:"claimRef,omitempty"`
}

// ClaimReference identifies the StackClaim a Stack is bound to.
// +structType=atomic
type ClaimReference struct {
	// name of the StackClaim.
	// +required
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// uid of the StackClaim, so that a recreated claim with the same name is
	// never mistaken for the original one.
	// +required
	// +kubebuilder:validation:Type=string
	// +kubebuilder:validation:MinLength=1
	UID types.UID `json:"uid"`
}

// StackStatus defines the observed state of Stack.
type StackStatus struct {
	// conditions represent the current state of the Stack resource.
	//
	// The Ready condition is the contract with consumers of Stacks:
	// - Ready=True: the infrastructure is usable.
	// - Ready=False, reason=Provisioning: the infrastructure is being created.
	// - Ready=False, reason=Failed: provisioning failed and will not be retried.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=".status.conditions[?(@.type=='Ready')].status"
// +kubebuilder:printcolumn:name="Reason",type=string,JSONPath=".status.conditions[?(@.type=='Ready')].reason"
// +kubebuilder:printcolumn:name="Pool",type=string,JSONPath=".metadata.labels.platform\\.my\\.domain/pool"
// +kubebuilder:printcolumn:name="Claim",type=string,JSONPath=".spec.claimRef.name"
// +kubebuilder:printcolumn:name="Expires",type=string,JSONPath=".spec.expiresAt"
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"

// Stack is the Schema for the stacks API
type Stack struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of Stack
	// +required
	// +kubebuilder:validation:XValidation:rule="!has(oldSelf.claimRef) || (has(self.claimRef) && self.claimRef == oldSelf.claimRef)",message="claimRef is immutable once set"
	Spec StackSpec `json:"spec"`

	// status defines the observed state of Stack
	// +optional
	Status StackStatus `json:"status,omitzero"`
}

// IsReady reports whether the Stack's infrastructure is usable.
func (s *Stack) IsReady() bool {
	return meta.IsStatusConditionTrue(s.Status.Conditions, StackConditionReady)
}

// HasFailed reports whether the Stack failed to provision.
func (s *Stack) HasFailed() bool {
	c := meta.FindStatusCondition(s.Status.Conditions, StackConditionReady)
	return c != nil && c.Status == metav1.ConditionFalse && c.Reason == StackReasonFailed
}

// IsClaimedBy reports whether the Stack is bound to the given StackClaim.
func (s *Stack) IsClaimedBy(claim *StackClaim) bool {
	return s.Spec.ClaimRef != nil && s.Spec.ClaimRef.UID == claim.UID
}

// +kubebuilder:object:root=true

// StackList contains a list of Stack
type StackList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []Stack `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &Stack{}, &StackList{})
		return nil
	})
}
