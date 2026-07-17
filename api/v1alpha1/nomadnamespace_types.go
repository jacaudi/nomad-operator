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

// NomadNamespace condition types and reasons. ReasonRegistered/ReasonClusterNotFound/
// ReasonDeleteFailed (nomadpool_types.go) and ReasonClusterNotReady (nomadnode_types.go)
// are declared elsewhere in this package and reused here.
const (
	NomadNamespaceCondReady         = "Ready"
	NomadNamespaceCondDeleteBlocked = "DeleteBlocked"

	ReasonNamespaceNameConflict = "NamespaceNameConflict"
	ReasonReservedNamespace     = "ReservedNamespace"
	ReasonNamespaceNotEmpty     = "NamespaceNotEmpty"
)

// NamespaceClusterRef names a NomadCluster in the same namespace.
type NamespaceClusterRef struct {
	// +kubebuilder:validation:Required
	Name string `json:"name"`
}

// NomadNamespaceSpec is the desired state of a Nomad namespace (a Nomad-internal
// tenancy partition, NOT a Kubernetes namespace). clusterRef + namespaceName are
// the immutable identity; description + meta are the managed body. The CR is the
// source of truth: the operator upserts it onto Nomad (read-modify-write,
// preserving unmanaged server fields) and deletes it.
//
// +kubebuilder:validation:XValidation:rule="self.namespaceName == oldSelf.namespaceName",message="namespaceName is immutable"
// +kubebuilder:validation:XValidation:rule="self.clusterRef.name == oldSelf.clusterRef.name",message="clusterRef.name is immutable"
type NomadNamespaceSpec struct {
	// ClusterRef names the NomadCluster (same namespace) this namespace lives on.
	// +kubebuilder:validation:Required
	ClusterRef NamespaceClusterRef `json:"clusterRef"`
	// NamespaceName is the exact Nomad namespace name. It is separate from
	// metadata.name because Nomad namespace names may contain characters illegal
	// in a Kubernetes object name. The built-in "default" namespace always exists
	// and cannot be deleted, so it is rejected.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^[a-zA-Z0-9-_]{1,128}$`
	// +kubebuilder:validation:XValidation:rule="self != 'default'",message="namespaceName 'default' is built-in and cannot be managed"
	NamespaceName string `json:"namespaceName"`
	// Description is a human-readable namespace description.
	// +optional
	Description string `json:"description,omitempty"`
	// Meta is a fully-managed key/value map on the namespace. spec.meta owns it
	// entirely; out-of-band Meta keys are overwritten.
	// +optional
	Meta map[string]string `json:"meta,omitempty"`
}

// NomadNamespaceStatus is the observed state, operator-owned.
type NomadNamespaceStatus struct {
	// JobCount is the total number of jobs in the namespace (informational;
	// populated on the delete-blocked path and steady-state resync). It is a raw
	// count from Jobs().List — the delete gate is the Delete refusal, not this.
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
// +kubebuilder:printcolumn:name="Namespace",type=string,JSONPath=`.spec.namespaceName`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// NomadNamespace is the Schema for the nomadnamespaces API.
type NomadNamespace struct {
	metav1.TypeMeta `json:",inline"`
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// +required
	Spec NomadNamespaceSpec `json:"spec"`
	// +optional
	Status NomadNamespaceStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// NomadNamespaceList contains a list of NomadNamespace.
type NomadNamespaceList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []NomadNamespace `json:"items"`
}

func init() {
	SchemeBuilder.Register(&NomadNamespace{}, &NomadNamespaceList{})
}
