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

// NomadPool condition types and reasons. ReasonClusterNotReady is declared in
// nomadnode_types.go (same package) and reused here.
const (
	NomadPoolCondReady         = "Ready"
	NomadPoolCondDeleteBlocked = "DeleteBlocked"

	ReasonRegistered       = "Registered"
	ReasonClusterNotFound  = "ClusterNotFound"
	ReasonPoolNameConflict = "PoolNameConflict"
	ReasonPoolNotEmpty     = "PoolNotEmpty"
	ReasonDeleteFailed     = "DeleteFailed"
)

// PoolClusterRef names a NomadCluster in the same namespace.
type PoolClusterRef struct {
	// +kubebuilder:validation:Required
	Name string `json:"name"`
}

// NomadPoolSpec is the desired state of a Nomad node pool. clusterRef + poolName
// are the immutable identity; description + meta are the managed body. The CR is
// the source of truth: the operator upserts it onto Nomad and deletes it.
//
// +kubebuilder:validation:XValidation:rule="self.poolName == oldSelf.poolName",message="poolName is immutable"
// +kubebuilder:validation:XValidation:rule="self.clusterRef.name == oldSelf.clusterRef.name",message="clusterRef.name is immutable"
type NomadPoolSpec struct {
	// ClusterRef names the NomadCluster (same namespace) this pool lives on.
	// +kubebuilder:validation:Required
	ClusterRef PoolClusterRef `json:"clusterRef"`
	// PoolName is the exact Nomad node-pool name. It is separate from
	// metadata.name because Nomad pool names allow characters illegal in a
	// Kubernetes object name (underscores, uppercase). The built-in pools
	// "default" and "all" cannot be managed and are rejected.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^[a-zA-Z0-9-_]{1,128}$`
	// +kubebuilder:validation:XValidation:rule="self != 'default' && self != 'all'",message="poolName 'default' and 'all' are built-in and cannot be managed"
	PoolName string `json:"poolName"`
	// Description is a human-readable pool description (Nomad Community Edition).
	// +optional
	Description string `json:"description,omitempty"`
	// Meta is a fully-managed key/value map on the pool (Nomad Community
	// Edition). spec.meta owns it entirely; out-of-band Meta keys are overwritten.
	// +optional
	Meta map[string]string `json:"meta,omitempty"`
}

// NomadPoolStatus is the observed state, operator-owned.
type NomadPoolStatus struct {
	// NodeCount is how many nodes are registered in the pool (refreshed each
	// steady-state resync).
	// +optional
	NodeCount int `json:"nodeCount,omitempty"`
	// JobCount is how many jobs target the pool. Populated on the delete-blocked
	// path where it is surfaced (design §3.2/§3.4), not on every resync.
	// +optional
	JobCount int `json:"jobCount,omitempty"`
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
// +kubebuilder:printcolumn:name="Pool",type=string,JSONPath=`.spec.poolName`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Nodes",type=integer,JSONPath=`.status.nodeCount`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// NomadPool is the Schema for the nomadpools API.
type NomadPool struct {
	metav1.TypeMeta `json:",inline"`
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// +required
	Spec NomadPoolSpec `json:"spec"`
	// +optional
	Status NomadPoolStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// NomadPoolList contains a list of NomadPool.
type NomadPoolList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []NomadPool `json:"items"`
}

func init() {
	SchemeBuilder.Register(&NomadPool{}, &NomadPoolList{})
}
