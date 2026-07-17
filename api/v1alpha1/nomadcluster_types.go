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
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!
// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

// GatewayMode describes how the Gateway API resources for a NomadCluster are provisioned.
type GatewayMode string

const (
	// GatewayModeManaged means the controller creates and owns the Gateway.
	GatewayModeManaged GatewayMode = "Managed"
	// GatewayModeExisting means the controller attaches routes to a pre-existing Gateway.
	GatewayModeExisting GatewayMode = "Existing"
)

// Phase values for NomadClusterStatus.Phase.
const (
	PhasePending       = "Pending"
	PhaseBootstrapping = "Bootstrapping"
	PhaseReady         = "Ready"
	PhaseDegraded      = "Degraded"
)

// Condition types.
const (
	CondReconciled          = "Reconciled"
	CondExternalAccessReady = "ExternalAccessReady"
	CondQuorumHealthy       = "QuorumHealthy"
	CondACLBootstrapped     = "ACLBootstrapped"
	CondReady               = "Ready"
)

// StorageSpec configures the persistent volume claims for Nomad server data.
type StorageSpec struct {
	// +kubebuilder:validation:Required
	Size string `json:"size"`
	// +optional
	StorageClassName string `json:"storageClassName,omitempty"`
}

// TLSSpec configures the TLS material used for Nomad's RPC/HTTP listeners.
type TLSSpec struct {
	// CertSecretRef names a cert-manager-issued Secret (tls.crt, tls.key, ca.crt).
	// SANs must include server.<region>.nomad, client.<region>.nomad,
	// spec.externalAccess.gateway.httpHostname (Gateway mode), localhost, and 127.0.0.1.
	// +kubebuilder:validation:Required
	CertSecretRef string `json:"certSecretRef"`
}

// GatewayRef identifies a pre-existing Gateway API Gateway to attach routes to.
type GatewayRef struct {
	// +kubebuilder:validation:Required
	Name string `json:"name"`
	// +kubebuilder:validation:Required
	Namespace string `json:"namespace"`
}

// GatewaySpec configures the Gateway API resources fronting a NomadCluster.
//
// +kubebuilder:validation:XValidation:rule="self.mode != 'Managed' || has(self.className)",message="className is required when mode is Managed"
// +kubebuilder:validation:XValidation:rule="self.mode != 'Existing' || has(self.ref)",message="ref is required when mode is Existing"
type GatewaySpec struct {
	// +kubebuilder:validation:Enum=Managed;Existing
	// +kubebuilder:default=Managed
	Mode GatewayMode `json:"mode,omitempty"`
	// +optional
	ClassName string `json:"className,omitempty"`
	// +optional
	Ref *GatewayRef `json:"ref,omitempty"`
	// RPCPorts is one L4 listener port per server; length must equal spec.servers.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="rpcPorts is immutable"
	RPCPorts []int32 `json:"rpcPorts"`
	// +kubebuilder:validation:Required
	HTTPHostname string `json:"httpHostname"`
}

// ExternalAccessMode selects how a NomadCluster's control plane is exposed to
// out-of-cluster edge agents.
type ExternalAccessMode string

const (
	// ExternalAccessGateway exposes the control plane via Gateway API objects
	// (one Gateway, per-server RPC listeners). Supports servers 1/3/5.
	ExternalAccessGateway ExternalAccessMode = "Gateway"
	// ExternalAccessLoadBalancer exposes a single-node control plane via one
	// type: LoadBalancer Service (north-south only; scoped to servers: 1).
	ExternalAccessLoadBalancer ExternalAccessMode = "LoadBalancer"
)

// LoadBalancerSpec configures the type: LoadBalancer Service used in
// LoadBalancer external-access mode. Kept intentionally lean (KISS): cloud LB
// behavior is driven by annotations, which already cover static-IP requests.
type LoadBalancerSpec struct {
	// +optional
	LoadBalancerClass string `json:"loadBalancerClass,omitempty"`
	// Annotations are applied verbatim to the LoadBalancer Service (cloud-LB
	// config). Untyped by design.
	// +optional
	Annotations map[string]string `json:"annotations,omitempty"`
}

// ExternalAccessSpec is a discriminated union selecting the external-access
// mechanism. gateway is set iff mode==Gateway; loadBalancer is optional and may
// be set only when mode==LoadBalancer.
//
// +kubebuilder:validation:XValidation:rule="self.mode == oldSelf.mode",message="externalAccess.mode is immutable"
// +kubebuilder:validation:XValidation:rule="self.mode != 'Gateway' || has(self.gateway)",message="externalAccess.gateway is required when mode is Gateway"
// +kubebuilder:validation:XValidation:rule="self.mode != 'LoadBalancer' || !has(self.gateway)",message="externalAccess.gateway must be absent when mode is LoadBalancer"
// +kubebuilder:validation:XValidation:rule="self.mode != 'Gateway' || !has(self.loadBalancer)",message="externalAccess.loadBalancer must be absent when mode is Gateway"
type ExternalAccessSpec struct {
	// +kubebuilder:validation:Enum=Gateway;LoadBalancer
	// +kubebuilder:default=Gateway
	Mode ExternalAccessMode `json:"mode,omitempty"`
	// +optional
	Gateway *GatewaySpec `json:"gateway,omitempty"`
	// +optional
	LoadBalancer *LoadBalancerSpec `json:"loadBalancer,omitempty"`
}

// NomadClusterSpec defines the desired state of NomadCluster
//
// +kubebuilder:validation:XValidation:rule="self.externalAccess.mode != 'Gateway' || size(self.externalAccess.gateway.rpcPorts) == self.servers",message="externalAccess.gateway.rpcPorts length must equal servers"
// +kubebuilder:validation:XValidation:rule="self.externalAccess.mode != 'LoadBalancer' || self.servers == 1",message="LoadBalancer external-access mode requires servers: 1"
type NomadClusterSpec struct {
	// +kubebuilder:validation:Required
	Image string `json:"image"`
	// Servers is the number of Raft control-plane servers. 3 or 5 give full
	// Raft HA (survives 1 or 2 node failures respectively) and are recommended
	// for production. 1 is a non-HA, single-node control plane for edge/dev
	// deployments: on Kubernetes a failed control-plane pod is rescheduled by
	// the StatefulSet controller, so the outage is bounded by the reschedule
	// time rather than open-ended, but there is no Raft quorum to fail over to
	// while the pod is down. Even counts are always rejected (split-brain
	// safety).
	// +kubebuilder:validation:Enum=1;3;5
	// +kubebuilder:default=3
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="servers is immutable"
	Servers int32 `json:"servers,omitempty"`
	// +kubebuilder:default=global
	Region string `json:"region,omitempty"`
	// +kubebuilder:default={"dc1"}
	Datacenters []string `json:"datacenters,omitempty"`
	// NodeGCThreshold sets the servers' node_gc_threshold — how long a node
	// must stay in a terminal (down) state before Nomad garbage-collects it.
	// Optional with NO default: when unset, the operator emits nothing and
	// Nomad uses its built-in default (24h). The NomadNode reflector's
	// down-node retention window tracks whatever this resolves to.
	// +optional
	NodeGCThreshold *metav1.Duration `json:"nodeGCThreshold,omitempty"`
	// +kubebuilder:validation:Required
	Storage StorageSpec `json:"storage"`
	// +kubebuilder:validation:Required
	TLS TLSSpec `json:"tls"`
	// +kubebuilder:validation:Required
	ExternalAccess ExternalAccessSpec `json:"externalAccess"`
	// +optional
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`
}

// MemberStatus reports the observed state of a single Nomad server member.
type MemberStatus struct {
	Name   string `json:"name"`
	Addr   string `json:"addr"`
	Status string `json:"status"`
	Leader bool   `json:"leader"`
	// Voter reports whether this server is a raft voter.
	// +optional
	Voter bool `json:"voter,omitempty"`
}

// NomadClusterStatus defines the observed state of NomadCluster.
type NomadClusterStatus struct {
	// +optional
	Phase string `json:"phase,omitempty"`
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// +optional
	ExternalAddress string `json:"externalAddress,omitempty"`
	// +optional
	Members []MemberStatus `json:"members,omitempty"`
	// +optional
	Leader string `json:"leader,omitempty"`
	// +optional
	Quorum string `json:"quorum,omitempty"`
	// +optional
	Endpoint string `json:"endpoint,omitempty"`
	// +optional
	BootstrapTokenSecretRef string `json:"bootstrapTokenSecretRef,omitempty"`
	// +optional
	GossipKeySecretRef string `json:"gossipKeySecretRef,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Quorum",type=string,JSONPath=`.status.quorum`
// +kubebuilder:printcolumn:name="Leader",type=string,JSONPath=`.status.leader`

// NomadCluster is the Schema for the nomadclusters API
type NomadCluster struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of NomadCluster
	// +required
	Spec NomadClusterSpec `json:"spec"`

	// status defines the observed state of NomadCluster
	// +optional
	Status NomadClusterStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// NomadClusterList contains a list of NomadCluster
type NomadClusterList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []NomadCluster `json:"items"`
}

func init() {
	SchemeBuilder.Register(&NomadCluster{}, &NomadClusterList{})
}
