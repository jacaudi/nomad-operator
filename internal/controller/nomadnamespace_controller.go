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
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	nomadv1alpha1 "github.com/jacaudi/nomad-operator/api/v1alpha1"
)

const (
	namespaceResync         = 60 * time.Second
	nomadNamespaceFinalizer = "nomad.operator.io/nomadnamespace-cleanup"
)

// NomadNamespaceReconciler manages Nomad namespaces declared as NomadNamespace
// CRs. The CR is the source of truth: the operator upserts the namespace onto
// Nomad and deletes it (finalizer-gated).
type NomadNamespaceReconciler struct {
	client.Client
	Scheme         *runtime.Scheme
	NewNomadClient NomadNamespaceClientFactory
	Recorder       record.EventRecorder
}

// +kubebuilder:rbac:groups=nomad.operator.io,resources=nomadnamespaces,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=nomad.operator.io,resources=nomadnamespaces/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=nomad.operator.io,resources=nomadnamespaces/finalizers,verbs=update
// +kubebuilder:rbac:groups=nomad.operator.io,resources=nomadclusters,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

func (r *NomadNamespaceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var nn nomadv1alpha1.NomadNamespace
	if err := r.Get(ctx, req.NamespacedName, &nn); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !nn.DeletionTimestamp.IsZero() {
		return r.finalizeNamespace(ctx, &nn)
	}

	if !controllerutil.ContainsFinalizer(&nn, nomadNamespaceFinalizer) {
		controllerutil.AddFinalizer(&nn, nomadNamespaceFinalizer)
		if err := r.Update(ctx, &nn); err != nil {
			return ctrl.Result{}, err
		}
	}

	var nc nomadv1alpha1.NomadCluster
	err := r.Get(ctx, types.NamespacedName{Name: nn.Spec.ClusterRef.Name, Namespace: nn.Namespace}, &nc)
	if apierrors.IsNotFound(err) {
		setNamespaceCondition(&nn, nomadv1alpha1.NomadNamespaceCondReady, metav1.ConditionFalse, nomadv1alpha1.ReasonClusterNotFound, "referenced NomadCluster does not exist")
		nn.Status.ObservedGeneration = nn.Generation
		if err := r.Status().Update(ctx, &nn); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: namespaceResync}, nil
	}
	if err != nil {
		return ctrl.Result{}, err
	}
	if nc.Status.Phase != nomadv1alpha1.PhaseReady {
		setNamespaceCondition(&nn, nomadv1alpha1.NomadNamespaceCondReady, metav1.ConditionFalse, nomadv1alpha1.ReasonClusterNotReady, "referenced NomadCluster is not Ready")
		nn.Status.ObservedGeneration = nn.Generation
		if err := r.Status().Update(ctx, &nn); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: namespaceResync}, nil
	}

	// Set the ownerReference for GC cascade, writing only when it changes.
	orig := nn.DeepCopy()
	if err := controllerutil.SetControllerReference(&nc, &nn, r.Scheme); err != nil {
		return ctrl.Result{}, err
	}
	if !equality.Semantic.DeepEqual(orig.OwnerReferences, nn.OwnerReferences) {
		if err := r.Update(ctx, &nn); err != nil {
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
	return r.reconcileNamespace(ctx, &nn, ops)
}

// reconcileNamespace applies the declared namespace onto Nomad (read-modify-write,
// preserving unmanaged fields) and derives status. Conflict detection (Task 6) is
// layered in.
func (r *NomadNamespaceReconciler) reconcileNamespace(ctx context.Context, nn *nomadv1alpha1.NomadNamespace, ops NomadNamespaceOps) (ctrl.Result, error) {
	// Defense-in-depth guard: CEL already rejects "default" at admission, but
	// never Register/Delete it even if one reaches here.
	if nn.Spec.NamespaceName == "default" {
		setNamespaceCondition(nn, nomadv1alpha1.NomadNamespaceCondReady, metav1.ConditionFalse, nomadv1alpha1.ReasonReservedNamespace, "the default namespace is built-in and cannot be managed")
		nn.Status.ObservedGeneration = nn.Generation
		return ctrl.Result{}, r.Status().Update(ctx, nn)
	}

	existing, err := ops.GetNamespace(ctx, nn.Spec.NamespaceName)
	if err != nil {
		return ctrl.Result{}, err
	}
	desired := desiredNamespace(existing, nn)
	if existing == nil || existing.Description != desired.Description || !maps.Equal(existing.Meta, desired.Meta) {
		if err := ops.UpsertNamespace(ctx, desired); err != nil {
			return ctrl.Result{}, err
		}
	}
	count, err := ops.CountNamespaceJobs(ctx, nn.Spec.NamespaceName)
	if err != nil {
		return ctrl.Result{}, err
	}
	nn.Status.JobCount = count

	setNamespaceCondition(nn, nomadv1alpha1.NomadNamespaceCondReady, metav1.ConditionTrue, nomadv1alpha1.ReasonRegistered, "namespace registered onto Nomad")
	nn.Status.ObservedGeneration = nn.Generation
	if err := r.Status().Update(ctx, nn); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: namespaceResync}, nil
}

// desiredNamespace builds the Namespace to Register: managed fields (Description,
// Meta) from spec, unmanaged fields (Quota, Capabilities, NodePoolConfiguration,
// Vault/Consul config, extra-claims) preserved from the existing namespace.
func desiredNamespace(existing *api.Namespace, nn *nomadv1alpha1.NomadNamespace) *api.Namespace {
	var d api.Namespace
	if existing != nil {
		d = *existing // preserve Quota, Capabilities, config blocks, indexes
	}
	d.Name = nn.Spec.NamespaceName
	d.Description = nn.Spec.Description
	d.Meta = nn.Spec.Meta
	return &d
}

// finalizeNamespace is filled in Task 7; here it only drops the finalizer so the
// skeleton compiles and a delete does not hang.
func (r *NomadNamespaceReconciler) finalizeNamespace(ctx context.Context, nn *nomadv1alpha1.NomadNamespace) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(nn, nomadNamespaceFinalizer) {
		return ctrl.Result{}, nil
	}
	return ctrl.Result{}, r.dropNamespaceFinalizer(ctx, nn)
}

func (r *NomadNamespaceReconciler) dropNamespaceFinalizer(ctx context.Context, nn *nomadv1alpha1.NomadNamespace) error {
	controllerutil.RemoveFinalizer(nn, nomadNamespaceFinalizer)
	return r.Update(ctx, nn)
}

func (r *NomadNamespaceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.NewNomadClient == nil {
		r.NewNomadClient = DefaultNomadNamespaceClientFactory
	}
	if r.Recorder == nil {
		r.Recorder = mgr.GetEventRecorderFor("nomadnamespace")
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&nomadv1alpha1.NomadNamespace{}).
		Watches(&nomadv1alpha1.NomadCluster{}, handler.EnqueueRequestsFromMapFunc(r.namespacesForCluster)).
		Named("nomadnamespace").
		Complete(r)
}

// namespacesForCluster maps a NomadCluster event to the reconcile keys of every
// NomadNamespace that targets it (so a cluster going Ready reconciles pending ones).
func (r *NomadNamespaceReconciler) namespacesForCluster(ctx context.Context, obj client.Object) []reconcile.Request {
	nc, ok := obj.(*nomadv1alpha1.NomadCluster)
	if !ok {
		return nil
	}
	var list nomadv1alpha1.NomadNamespaceList
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

// setNamespaceCondition upserts a status condition, preserving LastTransitionTime
// when the status is unchanged (mirrors setPoolCondition).
func setNamespaceCondition(nn *nomadv1alpha1.NomadNamespace, condType string, status metav1.ConditionStatus, reason, msg string) {
	c := metav1.Condition{Type: condType, Status: status, Reason: reason, Message: msg, ObservedGeneration: nn.Generation}
	for i, existing := range nn.Status.Conditions {
		if existing.Type == condType {
			if existing.Status != status {
				c.LastTransitionTime = metav1.Now()
			} else {
				c.LastTransitionTime = existing.LastTransitionTime
			}
			nn.Status.Conditions[i] = c
			return
		}
	}
	c.LastTransitionTime = metav1.Now()
	nn.Status.Conditions = append(nn.Status.Conditions, c)
}
