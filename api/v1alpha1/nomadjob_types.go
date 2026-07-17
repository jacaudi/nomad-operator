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
	"k8s.io/apimachinery/pkg/runtime"
)

// NomadJob condition types and reasons. ReasonRegistered/ReasonClusterNotFound
// (nomadpool_types.go) and ReasonClusterNotReady (nomadnode_types.go) are
// declared elsewhere in this package and reused here.
const (
	NomadJobCondReady         = "Ready"
	NomadJobCondDeleteBlocked = "DeleteBlocked"

	ReasonInvalidJobSpec    = "InvalidJobSpec"
	ReasonJobIDMismatch     = "JobIDMismatch"
	ReasonNamespaceMismatch = "NamespaceMismatch"
	ReasonDeregisterFailed  = "DeregisterFailed"
)

// JobClusterRef names a NomadCluster in the same namespace.
type JobClusterRef struct {
	// +kubebuilder:validation:Required
	Name string `json:"name"`
}

// NomadJobSpec is the desired state of a Nomad job. clusterRef + jobID are the
// immutable identity; job is the full Nomad jobspec. The CR is the source of
// truth: the operator strict-decodes spec.job into api.Job, injects the
// authoritative ID/Region, and Registers it (drift-gated by Plan).
//
// +kubebuilder:validation:XValidation:rule="self.jobID == oldSelf.jobID",message="jobID is immutable"
// +kubebuilder:validation:XValidation:rule="self.clusterRef.name == oldSelf.clusterRef.name",message="clusterRef.name is immutable"
// +kubebuilder:validation:XValidation:rule="self.nomadNamespace == oldSelf.nomadNamespace",message="nomadNamespace is immutable"
type NomadJobSpec struct {
	// ClusterRef names the NomadCluster (same namespace) this job runs on.
	// +kubebuilder:validation:Required
	ClusterRef JobClusterRef `json:"clusterRef"`
	// JobID is the exact Nomad job ID. It is a separate top-level field (not read
	// from spec.job) because CEL cannot validate into a schemaless RawExtension,
	// so identity and immutability must live here. The operator injects it as the
	// authoritative job.ID; a differing job.id inside spec.job is rejected.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^[a-zA-Z0-9-_.]{1,128}$`
	JobID string `json:"jobID"`
	// NomadNamespace is the Nomad namespace (a Nomad-internal tenancy partition,
	// NOT the Kubernetes namespace) this job is placed into. Immutable because
	// Nomad job identity is (namespace, jobID). The named namespace must already
	// exist (via a NomadNamespace CR, out-of-band, or the always-present
	// "default"); the operator injects it as the authoritative job.Namespace, and
	// a differing namespace inside spec.job is rejected.
	// +optional
	// +kubebuilder:default="default"
	// +kubebuilder:validation:Pattern=`^[a-zA-Z0-9-_]{1,128}$`
	NomadNamespace string `json:"nomadNamespace,omitempty"`
	// Job is the Nomad jobspec expressed as the api.Job structure (camelCase or
	// PascalCase; keys match api.Job case-insensitively). It is schemaless: the
	// CRD does not model its fields, so there is no per-field validation — the
	// operator strict-decodes it (unknown/typo'd keys are rejected). NOTE:
	// time.Duration fields (e.g. update.minHealthyTime) must be integer
	// nanoseconds, not "10s" strings.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Type=object
	// +kubebuilder:pruning:PreserveUnknownFields
	// +kubebuilder:validation:Schemaless
	Job runtime.RawExtension `json:"job"`
}

// NomadJobGroupStatus is the observed vs desired allocation count for one task
// group.
type NomadJobGroupStatus struct {
	// +optional
	Running int `json:"running,omitempty"`
	// +optional
	Desired int `json:"desired,omitempty"`
}

// NomadJobStatus is the observed state, operator-owned.
type NomadJobStatus struct {
	// JobStatus is Nomad's job status (running/pending/dead) from Info.
	// +optional
	JobStatus string `json:"jobStatus,omitempty"`
	// JobVersion is the server-observed job version from Info (distinct from
	// observedGeneration, which tracks the CR).
	// +optional
	JobVersion int64 `json:"jobVersion,omitempty"`
	// Groups maps each task-group name to its running/desired allocation counts.
	// +optional
	Groups map[string]NomadJobGroupStatus `json:"groups,omitempty"`
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
// +kubebuilder:printcolumn:name="Job",type=string,JSONPath=`.spec.jobID`
// +kubebuilder:printcolumn:name="Status",type=string,JSONPath=`.status.jobStatus`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// NomadJob is the Schema for the nomadjobs API.
type NomadJob struct {
	metav1.TypeMeta `json:",inline"`
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// +required
	Spec NomadJobSpec `json:"spec"`
	// +optional
	Status NomadJobStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// NomadJobList contains a list of NomadJob.
type NomadJobList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []NomadJob `json:"items"`
}

func init() {
	SchemeBuilder.Register(&NomadJob{}, &NomadJobList{})
}
