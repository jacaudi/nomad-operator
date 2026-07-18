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

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// NomadNode condition types and reasons.
const (
	NomadNodeCondReconciled = "Reconciled"
	// NomadNodeCondDrainSpecPendingRestart is True when the desired drain spec
	// was edited while the node was already draining: the in-flight drain keeps
	// its original parameters (re-issuing would restart the deadline), so the
	// edit takes effect only on the next re-issued drain. It is False once the
	// desired spec again matches the in-flight drain (e.g. the edit is reverted).
	NomadNodeCondDrainSpecPendingRestart = "DrainSpecPendingRestart"

	ReasonClusterNotReady   = "ClusterNotReady"
	ReasonDuplicateNodeName = "DuplicateNodeName"
	ReasonNodeNotFound      = "NodeNotFound"
	ReasonReconciled        = "Reconciled"
	ReasonDrainSpecEdited   = "DrainSpecEdited"
	ReasonDrainSpecInSync   = "DrainSpecInSync"
)

// NodeReference names a NomadCluster in the same namespace.
type NodeReference struct {
	// +kubebuilder:validation:Required
	Name string `json:"name"`
}

// NodeDrainSpec mirrors github.com/hashicorp/nomad/api.DrainSpec. Its presence
// on NomadNodeSpec means "drain this node"; its absence means "do not drain".
type NodeDrainSpec struct {
	// Deadline is how long remaining allocations may take to migrate before
	// they are force-stopped. It is a POINTER so unset is distinguishable from
	// an explicit zero: nil → the operator substitutes 1h; an explicit value is
	// used verbatim, where 0 means "no deadline" (drain gracefully forever).
	// +optional
	Deadline *metav1.Duration `json:"deadline,omitempty"`
	// +optional
	IgnoreSystemJobs bool `json:"ignoreSystemJobs,omitzero"`
}

// NomadNodeSpec is the desired state of a NomadNode. clusterRef + nodeName are
// the immutable identity; eligible + drain are the user's control surface.
//
// +kubebuilder:validation:XValidation:rule="self.nodeName == oldSelf.nodeName",message="nodeName is immutable"
// +kubebuilder:validation:XValidation:rule="self.clusterRef.name == oldSelf.clusterRef.name",message="clusterRef.name is immutable"
type NomadNodeSpec struct {
	// +kubebuilder:validation:Required
	ClusterRef NodeReference `json:"clusterRef"`
	// NodeName is the exact Nomad node Name this CR represents (the match key).
	// +kubebuilder:validation:Required
	NodeName string `json:"nodeName"`
	// Eligible is the scheduling-eligibility target when the node is not
	// actively draining. Defaults true for a hand-omitted field; it is NOT
	// omitempty, so the reflector's seeded value — including a deliberate false
	// for an observed-ineligible node — is always sent and never clobbered by
	// the default.
	// +kubebuilder:default=true
	// +optional
	Eligible bool `json:"eligible"`
	// Drain, when set, requests a drain; remove it to cancel.
	// +optional
	Drain *NodeDrainSpec `json:"drain,omitempty"`
}

// LastDrainStatus summarizes Nomad's Node.LastDrain (DrainMetadata).
type LastDrainStatus struct {
	Status    string       `json:"status,omitempty"`
	StartedAt *metav1.Time `json:"startedAt,omitempty"`
	UpdatedAt *metav1.Time `json:"updatedAt,omitempty"`
}

// NomadNodeStatus is the observed state, all operator-owned (mirror/resolved).
type NomadNodeStatus struct {
	// +optional
	NodeID string `json:"nodeID,omitempty"`
	// +optional
	Status string `json:"status,omitempty"`
	// +optional
	SchedulingEligibility string `json:"schedulingEligibility,omitempty"`
	// +optional
	Draining bool `json:"draining,omitempty"`
	// DrainObservedGeneration records the spec generation at which the current
	// drain was issued, so a completed drain converges (design §3.3).
	// +optional
	DrainObservedGeneration int64 `json:"drainObservedGeneration,omitempty"`
	// +optional
	LastDrain *LastDrainStatus `json:"lastDrain,omitempty"`
	// +optional
	NodeClass string `json:"nodeClass,omitempty"`
	// +optional
	NodePool string `json:"nodePool,omitempty"`
	// +optional
	Datacenter string `json:"datacenter,omitempty"`
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Cluster",type=string,JSONPath=`.spec.clusterRef.name`
// +kubebuilder:printcolumn:name="Status",type=string,JSONPath=`.status.status`
// +kubebuilder:printcolumn:name="Eligible",type=string,JSONPath=`.status.schedulingEligibility`
// +kubebuilder:printcolumn:name="Draining",type=boolean,JSONPath=`.status.draining`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// NomadNode is the Schema for the nomadnodes API.
type NomadNode struct {
	metav1.TypeMeta `json:",inline"`
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// +required
	Spec NomadNodeSpec `json:"spec"`
	// +optional
	Status NomadNodeStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// NomadNodeList contains a list of NomadNode.
type NomadNodeList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []NomadNode `json:"items"`
}

func init() {
	SchemeBuilder.Register(&NomadNode{}, &NomadNodeList{})
}
