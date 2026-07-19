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
	"maps"
	"time"

	"github.com/hashicorp/nomad/api"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	nomadv1alpha1 "github.com/jacaudi/nomad-operator/api/v1alpha1"
	"github.com/jacaudi/nomad-operator/internal/nomad"
)

const (
	poolResync         = 60 * time.Second
	nomadPoolFinalizer = "nomad.operator.io/nodepool-cleanup"
)

// NomadPoolReconciler manages Nomad node pools declared as NomadPool CRs. The CR
// is the source of truth: the operator upserts the pool onto Nomad and deletes
// it (finalizer-gated).
type NomadPoolReconciler struct {
	client.Client
	Scheme         *runtime.Scheme
	NewNomadClient NomadPoolClientFactory
	Recorder       events.EventRecorder
}

// +kubebuilder:rbac:groups=nomad.operator.io,resources=nomadpools,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=nomad.operator.io,resources=nomadpools/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=nomad.operator.io,resources=nomadpools/finalizers,verbs=update
// +kubebuilder:rbac:groups=nomad.operator.io,resources=nomadclusters,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

func (r *NomadPoolReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var np nomadv1alpha1.NomadPool
	if err := r.Get(ctx, req.NamespacedName, &np); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !np.DeletionTimestamp.IsZero() {
		return r.finalizePool(ctx, &np)
	}

	// Ensure finalizer before any external side-effect.
	if !controllerutil.ContainsFinalizer(&np, nomadPoolFinalizer) {
		controllerutil.AddFinalizer(&np, nomadPoolFinalizer)
		if err := r.Update(ctx, &np); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Resolve the cluster.
	var nc nomadv1alpha1.NomadCluster
	err := r.Get(ctx, types.NamespacedName{Name: np.Spec.ClusterRef.Name, Namespace: np.Namespace}, &nc)
	if apierrors.IsNotFound(err) {
		setPoolCondition(&np, nomadv1alpha1.NomadPoolCondReady, metav1.ConditionFalse, nomadv1alpha1.ReasonClusterNotFound, "referenced NomadCluster does not exist")
		np.Status.ObservedGeneration = np.Generation
		if err := r.Status().Update(ctx, &np); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: poolResync}, nil
	}
	if err != nil {
		return ctrl.Result{}, err
	}
	if nc.Status.Phase != nomadv1alpha1.PhaseReady {
		setPoolCondition(&np, nomadv1alpha1.NomadPoolCondReady, metav1.ConditionFalse, nomadv1alpha1.ReasonClusterNotReady, "referenced NomadCluster is not Ready")
		np.Status.ObservedGeneration = np.Generation
		if err := r.Status().Update(ctx, &np); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: poolResync}, nil
	}

	// Set the ownerReference for GC cascade, writing only when it actually changes
	// (avoids a spurious spec Update every 60s resync — M-1).
	orig := np.DeepCopy()
	if err := controllerutil.SetControllerReference(&nc, &np, r.Scheme); err != nil {
		return ctrl.Result{}, err
	}
	if !equality.Semantic.DeepEqual(orig.OwnerReferences, np.OwnerReferences) {
		if err := r.Update(ctx, &np); err != nil {
			return ctrl.Result{}, err
		}
	}

	cfg, err := clusterNomadConfig(ctx, r.Client, &nc)
	if err != nil {
		return ctrl.Result{}, err
	}
	ops, err := r.NewNomadClient(cfg)
	if err != nil {
		return ctrl.Result{}, err
	}
	return r.reconcilePool(ctx, &np, ops)
}

// reconcilePool applies the declared pool onto Nomad (read-modify-write,
// preserving unmanaged fields) and derives status. Collision detection (Task 6)
// and status counts (Task 7) are layered in.
func (r *NomadPoolReconciler) reconcilePool(ctx context.Context, np *nomadv1alpha1.NomadPool, ops NomadPoolOps) (ctrl.Result, error) {
	// Defense-in-depth guard: CEL already rejects built-ins at admission, but
	// never Register/Delete "default"/"all" even if one reaches here.
	if np.Spec.PoolName == api.NodePoolDefault || np.Spec.PoolName == api.NodePoolAll {
		setPoolCondition(np, nomadv1alpha1.NomadPoolCondReady, metav1.ConditionFalse, nomadv1alpha1.ReasonBuiltinPool, "built-in pool cannot be managed")
		np.Status.ObservedGeneration = np.Generation
		return ctrl.Result{}, r.Status().Update(ctx, np)
	}

	conflict, err := r.hasPoolNameConflict(ctx, np)
	if err != nil {
		return ctrl.Result{}, err
	}
	if conflict {
		setPoolCondition(np, nomadv1alpha1.NomadPoolCondReady, metav1.ConditionFalse, nomadv1alpha1.ReasonPoolNameConflict, "another NomadPool targets this poolName on this cluster; skipping Register")
		r.Recorder.Eventf(np, nil, "Warning", nomadv1alpha1.ReasonPoolNameConflict, "RegisterSkipped", "duplicate poolName on the same cluster; not registering to avoid churn")
		np.Status.ObservedGeneration = np.Generation
		if err := r.Status().Update(ctx, np); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: poolResync}, nil
	}

	existing, err := ops.GetNodePool(ctx, np.Spec.PoolName)
	if err != nil {
		return ctrl.Result{}, err
	}
	desired := desiredNodePool(existing, np)
	if existing == nil || existing.Description != desired.Description || !maps.Equal(existing.Meta, desired.Meta) {
		if err := ops.UpsertNodePool(ctx, desired); err != nil {
			return ctrl.Result{}, err
		}
	}
	count, err := ops.CountNodePoolNodes(ctx, np.Spec.PoolName)
	if err != nil {
		return ctrl.Result{}, err
	}
	np.Status.NodeCount = count

	setPoolCondition(np, nomadv1alpha1.NomadPoolCondReady, metav1.ConditionTrue, nomadv1alpha1.ReasonRegistered, "node pool registered onto Nomad")
	np.Status.ObservedGeneration = np.Generation
	if err := r.Status().Update(ctx, np); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: poolResync}, nil
}

// desiredNodePool builds the NodePool to Register: managed fields (Description,
// Meta) from spec, unmanaged fields (SchedulerConfiguration, NodeIdentityTTL)
// preserved from the existing pool. On create (existing==nil) it is fresh.
func desiredNodePool(existing *api.NodePool, np *nomadv1alpha1.NomadPool) *api.NodePool {
	var d api.NodePool
	if existing != nil {
		d = *existing // preserve SchedulerConfiguration, NodeIdentityTTL, indexes
	}
	d.Name = np.Spec.PoolName
	d.Description = np.Spec.Description
	d.Meta = np.Spec.Meta
	return &d
}

// finalizePool deletes the Nomad pool when the CR is deleted, gated so it never
// deadlocks a cascade: if the cluster is gone OR going (DeletionTimestamp set),
// there is nothing to clean up (the control plane is gone/going too), so drop
// the finalizer without calling Delete. This closes both background and
// foreground cascade (design §3.4).
func (r *NomadPoolReconciler) finalizePool(ctx context.Context, np *nomadv1alpha1.NomadPool) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(np, nomadPoolFinalizer) {
		return ctrl.Result{}, nil
	}

	var nc nomadv1alpha1.NomadCluster
	err := r.Get(ctx, types.NamespacedName{Name: np.Spec.ClusterRef.Name, Namespace: np.Namespace}, &nc)
	clusterGoneOrGoing := apierrors.IsNotFound(err) || (err == nil && !nc.DeletionTimestamp.IsZero())
	if clusterGoneOrGoing {
		return ctrl.Result{}, r.dropFinalizer(ctx, np)
	}
	if err != nil {
		return ctrl.Result{}, err
	}
	if nc.Status.Phase != nomadv1alpha1.PhaseReady {
		// Cluster present, not deleting, but unreachable — do NOT orphan on a blip.
		setPoolCondition(np, nomadv1alpha1.NomadPoolCondDeleteBlocked, metav1.ConditionTrue, nomadv1alpha1.ReasonClusterNotReady, "cluster not Ready; cannot confirm pool deletion")
		if uerr := r.Status().Update(ctx, np); uerr != nil {
			return ctrl.Result{}, uerr
		}
		return ctrl.Result{RequeueAfter: poolResync}, nil
	}

	cfg, err := clusterNomadConfig(ctx, r.Client, &nc)
	if err != nil {
		return ctrl.Result{}, err
	}
	ops, err := r.NewNomadClient(cfg)
	if err != nil {
		return ctrl.Result{}, err
	}

	if derr := ops.DeleteNodePool(ctx, np.Spec.PoolName); derr != nil && !nomad.IsNotFound(derr) {
		// Delete failed for a reason other than "already gone". Keep the
		// finalizer and requeue. Surface a friendly reason when the pool is
		// non-empty; fetch counts so the user sees what holds it.
		reason := nomadv1alpha1.ReasonDeleteFailed
		if nomad.IsNodePoolNotEmpty(derr) {
			reason = nomadv1alpha1.ReasonPoolNotEmpty
		}
		if nodes, e := ops.CountNodePoolNodes(ctx, np.Spec.PoolName); e == nil {
			np.Status.NodeCount = nodes
		}
		if jobs, e := ops.CountNodePoolJobs(ctx, np.Spec.PoolName); e == nil {
			np.Status.JobCount = jobs
		}
		setPoolCondition(np, nomadv1alpha1.NomadPoolCondDeleteBlocked, metav1.ConditionTrue, reason, derr.Error())
		if uerr := r.Status().Update(ctx, np); uerr != nil {
			return ctrl.Result{}, uerr
		}
		return ctrl.Result{RequeueAfter: poolResync}, nil
	}
	// Delete succeeded OR the pool is already gone (404) — either way there is
	// nothing left to clean up, so drop the finalizer (design §3.4). Without
	// this, a pool deleted out-of-band, or a crash between a successful Delete
	// and this Update landing, would re-issue Delete against a missing pool on
	// the next reconcile and get stuck Terminating forever.
	return ctrl.Result{}, r.dropFinalizer(ctx, np)
}

// dropFinalizer removes the cleanup finalizer, allowing Kubernetes to garbage
// collect the CR.
func (r *NomadPoolReconciler) dropFinalizer(ctx context.Context, np *nomadv1alpha1.NomadPool) error {
	controllerutil.RemoveFinalizer(np, nomadPoolFinalizer)
	return r.Update(ctx, np)
}

// poolClusterKey is the composite index/collision key for a pool CR.
func poolClusterKey(np *nomadv1alpha1.NomadPool) string {
	return np.Spec.ClusterRef.Name + "/" + np.Spec.PoolName
}

// hasPoolNameConflict reports whether another live NomadPool in this namespace
// targets the same poolName on the same cluster (design §3.5). A Terminating
// sibling (non-zero DeletionTimestamp) does not count: it is being replaced or
// GC'd and must not block a same-name successor from Registering. A plain
// namespaced List + in-Go filter — no field indexer, so it works identically
// on a cached (production) and a bare (envtest) client; at namespaced-pool
// scale the list cost is negligible and is not what §3.5 avoids (the skipped
// Register is).
func (r *NomadPoolReconciler) hasPoolNameConflict(ctx context.Context, np *nomadv1alpha1.NomadPool) (bool, error) {
	var list nomadv1alpha1.NomadPoolList
	if err := r.List(ctx, &list, client.InNamespace(np.Namespace)); err != nil {
		return false, err
	}
	key := poolClusterKey(np)
	for i := range list.Items {
		if list.Items[i].Name != np.Name && list.Items[i].DeletionTimestamp.IsZero() && poolClusterKey(&list.Items[i]) == key {
			return true, nil
		}
	}
	return false, nil
}

func (r *NomadPoolReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.NewNomadClient == nil {
		r.NewNomadClient = DefaultNomadPoolClientFactory
	}
	if r.Recorder == nil {
		r.Recorder = mgr.GetEventRecorder("nomadpool")
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&nomadv1alpha1.NomadPool{}).
		Watches(&nomadv1alpha1.NomadCluster{}, handler.EnqueueRequestsFromMapFunc(r.poolsForCluster)).
		Named("nomadpool").
		Complete(r)
}

// poolsForCluster maps a NomadCluster event to the reconcile keys of every
// NomadPool that targets it (so a cluster going Ready reconciles pending pools).
func (r *NomadPoolReconciler) poolsForCluster(ctx context.Context, obj client.Object) []reconcile.Request {
	nc, ok := obj.(*nomadv1alpha1.NomadCluster)
	if !ok {
		return nil
	}
	var list nomadv1alpha1.NomadPoolList
	if err := r.List(ctx, &list, client.InNamespace(nc.Namespace)); err != nil {
		return nil
	}
	var reqs []reconcile.Request
	for i := range list.Items {
		if list.Items[i].Spec.ClusterRef.Name == nc.Name {
			reqs = append(reqs, reconcile.Request{NamespacedName: types.NamespacedName{Name: list.Items[i].Name, Namespace: list.Items[i].Namespace}})
		}
	}
	return reqs
}

// setPoolCondition upserts a status condition, preserving LastTransitionTime
// when the status is unchanged (mirrors setNodeCondition).
func setPoolCondition(np *nomadv1alpha1.NomadPool, condType string, status metav1.ConditionStatus, reason, msg string) {
	c := metav1.Condition{Type: condType, Status: status, Reason: reason, Message: msg, ObservedGeneration: np.Generation}
	for i, existing := range np.Status.Conditions {
		if existing.Type == condType {
			if existing.Status != status {
				c.LastTransitionTime = metav1.Now()
			} else {
				c.LastTransitionTime = existing.LastTransitionTime
			}
			np.Status.Conditions[i] = c
			return
		}
	}
	c.LastTransitionTime = metav1.Now()
	np.Status.Conditions = append(np.Status.Conditions, c)
}
