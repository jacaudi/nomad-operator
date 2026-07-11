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

	"k8s.io/apimachinery/pkg/runtime"
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

	// 2. Gateway (Managed only in this task; Existing added in Task 9).
	gwAddr, gwReady, err := r.ensureManagedGateway(ctx, &nc)
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

	// 4. Bootstrapping: wait for quorum, then ACL bootstrap (Task 8).
	nc.Status.Phase = nomadv1alpha1.PhaseBootstrapping
	return r.bootstrapAndReady(ctx, &nc, gwAddr)
}

// finish persists status and returns the given Result.
func (r *NomadClusterReconciler) finish(ctx context.Context, nc *nomadv1alpha1.NomadCluster, res ctrl.Result) (ctrl.Result, error) {
	if err := r.Status().Update(ctx, nc); err != nil {
		return ctrl.Result{}, err
	}
	return res, nil
}

// bootstrapAndReady is a stub: it persists the Bootstrapping phase reached by
// Reconcile and requeues. Quorum polling and ACL bootstrap land in Task 8,
// which is why gwAddr is unused here (it will drive the ACL bootstrap client).
func (r *NomadClusterReconciler) bootstrapAndReady(ctx context.Context, nc *nomadv1alpha1.NomadCluster, _ string) (ctrl.Result, error) {
	return r.finish(ctx, nc, ctrl.Result{RequeueAfter: requeueShort})
}

func (r *NomadClusterReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.NewNomadClient == nil {
		r.NewNomadClient = DefaultNomadClientFactory
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&nomadv1alpha1.NomadCluster{}).
		Named("nomadcluster").
		Complete(r)
}
