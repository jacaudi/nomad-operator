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

	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	nomadv1alpha1 "github.com/jacaudi/nomad-operator/api/v1alpha1"
	"github.com/jacaudi/nomad-operator/internal/nomad"
)

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
	logger := log.FromContext(ctx)

	var nc nomadv1alpha1.NomadCluster
	if err := r.Get(ctx, req.NamespacedName, &nc); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Phase machine is filled in by later tasks. For now, establish the
	// Reconciled condition and observedGeneration so the resource is live.
	if nc.Status.Phase == "" {
		nc.Status.Phase = nomadv1alpha1.PhasePending
	}
	nc.Status.ObservedGeneration = nc.Generation
	setCondition(&nc, nomadv1alpha1.CondReconciled, metav1ConditionTrue, "Accepted", "spec accepted")

	if err := r.Status().Update(ctx, &nc); err != nil {
		logger.Error(err, "status update failed")
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
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
