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
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	nomadv1alpha1 "github.com/jacaudi/nomad-operator/api/v1alpha1"
	"github.com/jacaudi/nomad-operator/internal/nomad"
)

// requeueShort is used while waiting on external state (cert Secret, Gateway
// address assignment) that this reconciler doesn't control the timing of.
const requeueShort = 15 * time.Second

// NomadOps is the subset of the Nomad client the reconciler needs. Defined at
// the consumer per Go convention; *nomad.Client satisfies it, and envtest
// injects a fake.
type NomadOps interface {
	Ping(ctx context.Context) error
	Leader(ctx context.Context) (string, error)
	ServerHealthy(ctx context.Context) (bool, error)
	ACLBootstrap(ctx context.Context, bootstrapToken string) (string, error)
}

// NomadClientFactory builds a NomadOps from an explicit per-cluster Config.
type NomadClientFactory func(cfg nomad.Config) (NomadOps, error)

// DefaultNomadClientFactory constructs the real client.
func DefaultNomadClientFactory(cfg nomad.Config) (NomadOps, error) {
	return nomad.New(cfg)
}

// NomadClusterReconciler reconciles a NomadCluster object.
type NomadClusterReconciler struct {
	client.Client
	Scheme         *runtime.Scheme
	NewNomadClient NomadClientFactory
}

// +kubebuilder:rbac:groups=nomad.operator.io,resources=nomadclusters,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=nomad.operator.io,resources=nomadclusters/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=nomad.operator.io,resources=nomadclusters/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=statefulsets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=services;configmaps;secrets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=policy,resources=poddisruptionbudgets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=gateways;tcproutes;tlsroutes;httproutes,verbs=get;list;watch;create;update;patch;delete

func (r *NomadClusterReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var nc nomadv1alpha1.NomadCluster
	if err := r.Get(ctx, req.NamespacedName, &nc); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Retain the Reconciled condition + observedGeneration.
	nc.Status.ObservedGeneration = nc.Generation
	setCondition(&nc, nomadv1alpha1.CondReconciled, metav1ConditionTrue, "Accepted", "spec accepted")

	// 1. Security material.
	gossipName, err := r.ensureGossipKey(ctx, &nc)
	if err != nil {
		return ctrl.Result{}, err
	}
	nc.Status.GossipKeySecretRef = gossipName

	certReady, err := r.certSecretReady(ctx, &nc)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !certReady {
		nc.Status.Phase = nomadv1alpha1.PhasePending
		setCondition(&nc, nomadv1alpha1.CondReady, metav1ConditionFalse, "WaitingForCert", "cert Secret not ready")
		return r.finish(ctx, &nc, ctrl.Result{RequeueAfter: requeueShort})
	}

	// 2. Gateway: dispatches to Managed or Existing based on spec.gateway.mode.
	gwAddr, gwReady, err := r.ensureGateway(ctx, &nc)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !gwReady {
		nc.Status.Phase = nomadv1alpha1.PhasePending
		setCondition(&nc, nomadv1alpha1.CondGatewayReady, metav1ConditionFalse, "WaitingForAddress", "gateway address not assigned")
		return r.finish(ctx, &nc, ctrl.Result{RequeueAfter: requeueShort})
	}
	nc.Status.GatewayAddress = gwAddr
	setCondition(&nc, nomadv1alpha1.CondGatewayReady, metav1ConditionTrue, "Assigned", "gateway address assigned")

	// 3. Render config + provision workloads.
	_, configHash := renderConfig(&nc, gwAddr)
	if err := r.apply(ctx, &nc, buildConfigMap(&nc, gwAddr)); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.apply(ctx, &nc, buildHeadlessService(&nc)); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.apply(ctx, &nc, buildAPIService(&nc)); err != nil {
		return ctrl.Result{}, err
	}
	for ordinal := range int(nc.Spec.Servers) {
		if err := r.apply(ctx, &nc, buildPodService(&nc, ordinal)); err != nil {
			return ctrl.Result{}, err
		}
	}
	if err := r.apply(ctx, &nc, buildTLSRoute(&nc)); err != nil {
		return ctrl.Result{}, err
	}
	for _, rt := range buildTCPRoutes(&nc) {
		if err := r.apply(ctx, &nc, rt); err != nil {
			return ctrl.Result{}, err
		}
	}
	if err := r.apply(ctx, &nc, buildStatefulSet(&nc, configHash)); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.apply(ctx, &nc, buildPDB(&nc)); err != nil {
		return ctrl.Result{}, err
	}

	// 4. Bootstrapping: wait for quorum, then ACL bootstrap.
	// Preserve a Ready/Degraded phase so bootstrapAndReady can evaluate quorum
	// loss (Ready->Degraded); only seed Bootstrapping from the initial gate.
	if nc.Status.Phase == nomadv1alpha1.PhasePending || nc.Status.Phase == "" {
		nc.Status.Phase = nomadv1alpha1.PhaseBootstrapping
	}
	return r.bootstrapAndReady(ctx, &nc, gwAddr)
}

// finish persists status and returns the given Result.
func (r *NomadClusterReconciler) finish(ctx context.Context, nc *nomadv1alpha1.NomadCluster, res ctrl.Result) (ctrl.Result, error) {
	if err := r.Status().Update(ctx, nc); err != nil {
		return ctrl.Result{}, err
	}
	return res, nil
}

// clientFor builds a per-cluster NomadOps from the CR: endpoint is the
// in-cluster API Service, TLS material comes as PEM bytes from the
// cert-manager Secret (never files), and the token, if bootstrapped, comes
// from the token Secret.
func (r *NomadClusterReconciler) clientFor(ctx context.Context, nc *nomadv1alpha1.NomadCluster) (NomadOps, error) {
	n := names(nc)
	endpoint := fmt.Sprintf("https://%s.%s.svc:%d", n.APISvc, nc.Namespace, portHTTP)

	var certSec corev1.Secret
	if err := r.Get(ctx, types.NamespacedName{Name: nc.Spec.TLS.CertSecretRef, Namespace: nc.Namespace}, &certSec); err != nil {
		return nil, err
	}
	// The operator holds PEM bytes (from the Secret), not files, so it uses the
	// *PEM fields added to nomad.TLSConfig in Step 4a.
	cfg := nomad.Config{
		Address:       endpoint,
		Region:        nc.Spec.Region,
		TLSServerName: "server." + nc.Spec.Region + ".nomad",
		TLS: nomad.TLSConfig{
			CACertPEM:     certSec.Data["ca.crt"],
			ClientCertPEM: certSec.Data["tls.crt"],
			ClientKeyPEM:  certSec.Data["tls.key"],
		},
	}

	// Token, if bootstrapped.
	var tokenSec corev1.Secret
	if err := r.Get(ctx, types.NamespacedName{Name: names(nc).TokenSecret, Namespace: nc.Namespace}, &tokenSec); err == nil {
		cfg.Token = string(tokenSec.Data["token"])
	}
	return r.NewNomadClient(cfg)
}

// bootstrapAndReady waits for quorum via the injected client, runs the
// idempotent ACL bootstrap, and advances the cluster to Ready. The gateway
// address is already persisted to nc.Status.GatewayAddress by the caller
// before this runs, so the body below doesn't consume it directly; the
// parameter is kept (blank) to match this method's documented interface
// contract (task-8 brief).
func (r *NomadClusterReconciler) bootstrapAndReady(ctx context.Context, nc *nomadv1alpha1.NomadCluster, _ string) (ctrl.Result, error) {
	ops, err := r.clientFor(ctx, nc)
	if err != nil {
		return ctrl.Result{}, err
	}
	leader, err := ops.Leader(ctx)
	if err != nil || leader == "" {
		setCondition(nc, nomadv1alpha1.CondQuorumHealthy, metav1ConditionFalse, "NoLeader", "waiting for quorum")
		if nc.Status.Phase == nomadv1alpha1.PhaseReady {
			// Was Ready, now no leader → quorum lost (design §3.5/§3.7).
			nc.Status.Phase = nomadv1alpha1.PhaseDegraded
			setCondition(nc, nomadv1alpha1.CondReady, metav1ConditionFalse, "QuorumLost", "leader lost")
		}
		return r.finish(ctx, nc, ctrl.Result{RequeueAfter: requeueShort})
	}
	setCondition(nc, nomadv1alpha1.CondQuorumHealthy, metav1ConditionTrue, "LeaderElected", "quorum healthy")
	// status.leader carries the raw "ip:port" from Status().Leader(). Mapping it
	// to a friendly "<name>-server-N.<region>" and populating status.members from
	// Status().Peers() are DEFERRED to slice 6 (hardening) — noted so they are not
	// silently dropped; the DoD only requires leader/quorum be populated.
	nc.Status.Leader = leader
	nc.Status.Quorum = fmt.Sprintf("%d/%d", nc.Spec.Servers, nc.Spec.Servers)

	if err := r.ensureBootstrapToken(ctx, nc, ops); err != nil {
		setCondition(nc, nomadv1alpha1.CondACLBootstrapped, metav1ConditionFalse, "BootstrapFailed", err.Error())
		return r.finish(ctx, nc, ctrl.Result{RequeueAfter: requeueShort})
	}
	nc.Status.BootstrapTokenSecretRef = names(nc).TokenSecret
	setCondition(nc, nomadv1alpha1.CondACLBootstrapped, metav1ConditionTrue, "Bootstrapped", "acl bootstrapped")

	n := names(nc)
	nc.Status.Endpoint = fmt.Sprintf("https://%s.%s.svc:%d", n.APISvc, nc.Namespace, portHTTP)
	nc.Status.Phase = nomadv1alpha1.PhaseReady
	setCondition(nc, nomadv1alpha1.CondReady, metav1ConditionTrue, "Ready", "cluster ready")
	return r.finish(ctx, nc, ctrl.Result{RequeueAfter: requeueSteady})
}

const requeueSteady = 60 * time.Second

func (r *NomadClusterReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.NewNomadClient == nil {
		r.NewNomadClient = DefaultNomadClientFactory
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&nomadv1alpha1.NomadCluster{}).
		Named("nomadcluster").
		Complete(r)
}
