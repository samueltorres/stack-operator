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

// StackPool condition types and reasons.
const (
	// StackPoolConditionReady is True when the pool holds its desired number of warm Stacks.
	StackPoolConditionReady = "Ready"
	// StackPoolConditionDegraded is True when the pool stopped provisioning
	// because Stacks built from its template keep failing.
	StackPoolConditionDegraded = "Degraded"

	StackPoolReasonWarm                = "Warm"
	StackPoolReasonWarmingUp           = "WarmingUp"
	StackPoolReasonProvisioningHealthy = "ProvisioningHealthy"
	StackPoolReasonProvisioningFailing = "ProvisioningFailing"
)

// StackPoolSpec defines the desired state of StackPool
// +kubebuilder:validation:XValidation:rule="self.warm <= self.max",message="warm must not exceed max"
// +kubebuilder:validation:XValidation:rule="!has(self.maxTTL) || duration(self.defaultTTL) <= duration(self.maxTTL)",message="defaultTTL must not exceed maxTTL"
// +kubebuilder:validation:XValidation:rule="!has(self.warmSchedules) || self.warmSchedules.all(s, s.warm <= self.max)",message="warmSchedules[].warm must not exceed max"
type StackPoolSpec struct {
	// warm is the number of unclaimed Stacks, ready or provisioning, the pool
	// keeps around so claims can be served without waiting for provisioning.
	// Set it to 0 for a purely on-demand pool. warmSchedules can override it
	// at set times.
	// +required
	// +kubebuilder:validation:Minimum=0
	Warm int32 `json:"warm"`

	// warmSchedules override warm during recurring windows, for example to
	// keep fewer Stacks around at weekends. When several windows are active
	// at once, the lowest warm wins. Outside every window, warm applies.
	// Pending claims are still served while a window is active; they wait for
	// provisioning if the pool holds nothing warm.
	// +optional
	// +listType=map
	// +listMapKey=name
	// +kubebuilder:validation:MaxItems=16
	WarmSchedules []WarmSchedule `json:"warmSchedules,omitempty"`

	// timeZone is the IANA time zone warmSchedules are evaluated in, such as
	// "Europe/Lisbon". Defaults to UTC.
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=64
	// +kubebuilder:validation:XValidation:rule="self != 'Local' && timestamp('2000-01-01T00:00:00Z').getHours(self) >= 0",message="timeZone must be a valid IANA time zone"
	TimeZone *string `json:"timeZone,omitempty"`

	// max caps the number of Stacks the pool runs at once: warm, provisioning
	// and claimed. Stacks being torn down do not count.
	// +required
	// +kubebuilder:validation:Minimum=1
	Max int32 `json:"max"`

	// defaultTTL is how long a claim holds its Stack when the claim does not
	// set a ttl. The clock starts when the claim is bound.
	// +optional
	// +kubebuilder:default="2h"
	// +kubebuilder:validation:XValidation:rule="duration(self) > duration('0s')",message="defaultTTL must be positive"
	DefaultTTL metav1.Duration `json:"defaultTTL,omitzero"`

	// maxTTL caps the ttl a claim may hold its Stack for. Longer requests are
	// clamped. When unset, claims may ask for any ttl.
	// +optional
	// +kubebuilder:validation:XValidation:rule="duration(self) > duration('0s')",message="maxTTL must be positive"
	MaxTTL *metav1.Duration `json:"maxTTL,omitempty"`

	// template describes the Stacks the pool creates. Every Stack in a pool is
	// built from the same template. Changing it replaces unclaimed Stacks;
	// claimed Stacks are left alone.
	// +required
	Template StackTemplateSpec `json:"template"`
}

// WarmSchedule sets the pool's warm target during a window that opens on the
// given days at start, in the pool's timeZone, and stays open for duration.
// A window may run past midnight into the following days.
type WarmSchedule struct {
	// name identifies the schedule. status.activeWarmSchedule reports it.
	// +required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	Name string `json:"name"`

	// days the window starts on.
	// +required
	// +listType=set
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=7
	Days []Weekday `json:"days"`

	// start is the time of day the window opens, as HH:MM.
	// +optional
	// +kubebuilder:default="00:00"
	// +kubebuilder:validation:Pattern=`^([01][0-9]|2[0-3]):[0-5][0-9]$`
	Start string `json:"start,omitempty"`

	// duration is how long the window stays open, up to a week. It is measured
	// on the wall clock, so a window from 20:00 lasting 12h closes at 08:00 even
	// when daylight saving time changes overnight.
	// +optional
	// +kubebuilder:default="24h"
	// +kubebuilder:validation:XValidation:rule="duration(self) > duration('0s')",message="duration must be positive"
	// +kubebuilder:validation:XValidation:rule="duration(self) <= duration('168h')",message="duration must not exceed a week"
	Duration metav1.Duration `json:"duration,omitzero"`

	// warm is the number of warm Stacks the pool keeps while the window is open.
	// +required
	// +kubebuilder:validation:Minimum=0
	Warm int32 `json:"warm"`
}

// Weekday is a day of the week.
// +kubebuilder:validation:Enum=Monday;Tuesday;Wednesday;Thursday;Friday;Saturday;Sunday
type Weekday string

const (
	Monday    Weekday = "Monday"
	Tuesday   Weekday = "Tuesday"
	Wednesday Weekday = "Wednesday"
	Thursday  Weekday = "Thursday"
	Friday    Weekday = "Friday"
	Saturday  Weekday = "Saturday"
	Sunday    Weekday = "Sunday"
)

// StackTemplateSpec describes the Stacks a StackPool creates.
type StackTemplateSpec struct {
	// metadata holds labels and annotations copied onto every Stack.
	// +optional
	Metadata StackTemplateMetadata `json:"metadata,omitzero"`

	// spec is copied onto every Stack.
	// +optional
	// +kubebuilder:validation:XValidation:rule="!has(self.expiresAt) && !has(self.claimRef)",message="expiresAt and claimRef are managed by the pool and cannot be templated"
	Spec StackSpec `json:"spec,omitzero"`
}

// StackTemplateMetadata is the subset of object metadata a template may set.
type StackTemplateMetadata struct {
	// labels copied onto every Stack.
	// +optional
	Labels map[string]string `json:"labels,omitempty"`

	// annotations copied onto every Stack.
	// +optional
	Annotations map[string]string `json:"annotations,omitempty"`
}

// StackPoolStatus defines the observed state of StackPool.
type StackPoolStatus struct {
	// observedGeneration is the generation of the spec this status reflects.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// templateHash identifies the current template. Stacks carrying a
	// different hash are replaced once they are unclaimed.
	// +optional
	TemplateHash string `json:"templateHash,omitempty"`

	// desiredWarm is the number of warm Stacks the pool currently aims for:
	// spec.warm, or the warm of the active warm schedule.
	// +optional
	DesiredWarm int32 `json:"desiredWarm"`

	// activeWarmSchedule names the warm schedule that sets desiredWarm. It is
	// empty when spec.warm applies.
	// +optional
	ActiveWarmSchedule string `json:"activeWarmSchedule,omitempty"`

	// provisioning is the number of unclaimed Stacks still being provisioned.
	// +optional
	Provisioning int32 `json:"provisioning"`

	// warm is the number of unclaimed, ready Stacks waiting for a claim.
	// +optional
	Warm int32 `json:"warm"`

	// claimed is the number of Stacks bound to a StackClaim.
	// +optional
	Claimed int32 `json:"claimed"`

	// pendingClaims is the number of StackClaims waiting for a Stack.
	// +optional
	PendingClaims int32 `json:"pendingClaims"`

	// consecutiveFailures counts Stacks built from the current template that
	// failed to provision since the last one that succeeded.
	// +optional
	ConsecutiveFailures int32 `json:"consecutiveFailures,omitempty"`

	// lastFailureTime is when the most recent provisioning failure was observed.
	// Provisioning backs off exponentially from this point.
	// +optional
	LastFailureTime *metav1.Time `json:"lastFailureTime,omitempty"`

	// conditions represent the current state of the StackPool resource.
	//
	// - Ready: the pool holds its desired number of warm Stacks.
	// - Degraded: provisioning stopped after repeated failures. Change the
	//   template (for example by bumping an annotation) to try again.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:printcolumn:name="Desired",type=integer,JSONPath=".status.desiredWarm"
// +kubebuilder:printcolumn:name="Warm",type=integer,JSONPath=".status.warm"
// +kubebuilder:printcolumn:name="Provisioning",type=integer,JSONPath=".status.provisioning"
// +kubebuilder:printcolumn:name="Claimed",type=integer,JSONPath=".status.claimed"
// +kubebuilder:printcolumn:name="Pending",type=integer,JSONPath=".status.pendingClaims"
// +kubebuilder:printcolumn:name="Max",type=integer,JSONPath=".spec.max"
// +kubebuilder:printcolumn:name="Degraded",type=string,JSONPath=".status.conditions[?(@.type=='Degraded')].status"
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"

// StackPool keeps a set of identical Stacks provisioned ahead of time so that
// StackClaims can be served without waiting for provisioning.
type StackPool struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of StackPool
	// +required
	Spec StackPoolSpec `json:"spec"`

	// status defines the observed state of StackPool
	// +optional
	Status StackPoolStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// StackPoolList contains a list of StackPool
type StackPoolList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []StackPool `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &StackPool{}, &StackPoolList{})
		return nil
	})
}
