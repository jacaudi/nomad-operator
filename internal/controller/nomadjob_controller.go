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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	jobResync         = 60 * time.Second
	nomadJobFinalizer = "nomad.operator.io/nomadjob-cleanup"
)

// errJobIDMismatch is returned by decodeJob when spec.job carries an explicit
// id that disagrees with spec.jobID (spec.jobID is authoritative).
var errJobIDMismatch = errors.New("job.id does not match spec.jobID")

// decodeJob strict-decodes spec.job (JSON in RawExtension.Raw) into an api.Job,
// then injects the authoritative identity and region. DisallowUnknownFields
// turns a typo'd/unknown key or a wrong-scalar-type (incl. an HCL-style duration
// string, which time.Duration rejects — it wants integer nanoseconds) into an
// error the reconciler surfaces as InvalidJobSpec. A blob id that disagrees with
// spec.jobID is rejected (JobIDMismatch); otherwise spec.jobID wins.
func decodeJob(spec nomadv1alpha1.NomadJobSpec, region string) (*api.Job, error) {
	dec := json.NewDecoder(bytes.NewReader(spec.Job.Raw))
	dec.DisallowUnknownFields()
	var job api.Job
	if err := dec.Decode(&job); err != nil {
		return nil, fmt.Errorf("decode spec.job: %w", err)
	}
	if job.ID != nil && *job.ID != "" && *job.ID != spec.JobID {
		return nil, fmt.Errorf("%w: job.id=%q spec.jobID=%q", errJobIDMismatch, *job.ID, spec.JobID)
	}
	job.ID = &spec.JobID
	job.Region = &region
	return &job, nil
}

// NomadJobReconciler manages Nomad jobs declared as NomadJob CRs. The CR is the
// source of truth: the operator strict-decodes spec.job, Registers it onto Nomad
// (drift-gated by Plan), and Deregisters it (finalizer-gated).
type NomadJobReconciler struct {
	client.Client
	Scheme         *runtime.Scheme
	NewNomadClient NomadJobClientFactory
	Recorder       record.EventRecorder
}

// +kubebuilder:rbac:groups=nomad.operator.io,resources=nomadjobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=nomad.operator.io,resources=nomadjobs/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=nomad.operator.io,resources=nomadjobs/finalizers,verbs=update
// +kubebuilder:rbac:groups=nomad.operator.io,resources=nomadclusters,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

func (r *NomadJobReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var nj nomadv1alpha1.NomadJob
	if err := r.Get(ctx, req.NamespacedName, &nj); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !nj.DeletionTimestamp.IsZero() {
		return r.finalizeJob(ctx, &nj)
	}

	// Ensure finalizer before any external side-effect.
	if !controllerutil.ContainsFinalizer(&nj, nomadJobFinalizer) {
		controllerutil.AddFinalizer(&nj, nomadJobFinalizer)
		if err := r.Update(ctx, &nj); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Resolve the cluster.
	var nc nomadv1alpha1.NomadCluster
	err := r.Get(ctx, types.NamespacedName{Name: nj.Spec.ClusterRef.Name, Namespace: nj.Namespace}, &nc)
	if apierrors.IsNotFound(err) {
		setJobCondition(&nj, nomadv1alpha1.NomadJobCondReady, metav1.ConditionFalse, nomadv1alpha1.ReasonClusterNotFound, "referenced NomadCluster does not exist")
		nj.Status.ObservedGeneration = nj.Generation
		if err := r.Status().Update(ctx, &nj); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: jobResync}, nil
	}
	if err != nil {
		return ctrl.Result{}, err
	}
	if nc.Status.Phase != nomadv1alpha1.PhaseReady {
		setJobCondition(&nj, nomadv1alpha1.NomadJobCondReady, metav1.ConditionFalse, nomadv1alpha1.ReasonClusterNotReady, "referenced NomadCluster is not Ready")
		nj.Status.ObservedGeneration = nj.Generation
		if err := r.Status().Update(ctx, &nj); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: jobResync}, nil
	}

	// Set the ownerReference for GC cascade, writing only when it actually changes
	// (avoids a spurious spec Update every 60s resync — mirrors NomadPool M-1).
	orig := nj.DeepCopy()
	if err := controllerutil.SetControllerReference(&nc, &nj, r.Scheme); err != nil {
		return ctrl.Result{}, err
	}
	if !equality.Semantic.DeepEqual(orig.OwnerReferences, nj.OwnerReferences) {
		if err := r.Update(ctx, &nj); err != nil {
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
	return r.reconcileJob(ctx, &nj, ops, nc.Spec.Region)
}

// reconcileJob decodes spec.job, drift-gates the register via Plan, and derives
// status. Status derivation (Task 7) is layered in before the Ready write.
func (r *NomadJobReconciler) reconcileJob(ctx context.Context, nj *nomadv1alpha1.NomadJob, ops NomadJobOps, region string) (ctrl.Result, error) {
	desired, err := decodeJob(nj.Spec, region)
	if err != nil {
		reason := nomadv1alpha1.ReasonInvalidJobSpec
		if errors.Is(err, errJobIDMismatch) {
			reason = nomadv1alpha1.ReasonJobIDMismatch
		}
		setJobCondition(nj, nomadv1alpha1.NomadJobCondReady, metav1.ConditionFalse, reason, err.Error())
		nj.Status.ObservedGeneration = nj.Generation
		if uerr := r.Status().Update(ctx, nj); uerr != nil {
			return ctrl.Result{}, uerr
		}
		return ctrl.Result{RequeueAfter: jobResync}, nil
	}

	// Drift-gate the register on Plan's Diff.Type (never on FailedTGAllocs — an
	// infeasible-but-changed job must still be registered so the user can fix it).
	changed, err := ops.PlanJob(ctx, desired)
	if err != nil {
		return ctrl.Result{}, err
	}
	if changed {
		warnings, err := ops.RegisterJob(ctx, desired)
		if err != nil {
			return ctrl.Result{}, err
		}
		if warnings != "" {
			r.Recorder.Event(nj, "Normal", "RegisterWarnings", warnings)
		}
	}

	// Derive bounded runtime status (managed, not a deep mirror).
	info, err := ops.GetJob(ctx, nj.Spec.JobID)
	if err != nil {
		return ctrl.Result{}, err
	}
	if info != nil {
		if info.Status != nil {
			nj.Status.JobStatus = *info.Status
		}
		if info.Version != nil {
			nj.Status.JobVersion = int64(*info.Version)
		}
	}
	summary, err := ops.JobGroupSummary(ctx, nj.Spec.JobID)
	if err != nil {
		return ctrl.Result{}, err
	}
	groups := make(map[string]nomadv1alpha1.NomadJobGroupStatus, len(desired.TaskGroups))
	for _, g := range desired.TaskGroups {
		name := ""
		if g.Name != nil {
			name = *g.Name
		}
		gs := nomadv1alpha1.NomadJobGroupStatus{}
		if g.Count != nil {
			gs.Desired = *g.Count
		}
		if s, ok := summary[name]; ok {
			gs.Running = s.Running
		}
		groups[name] = gs
	}
	nj.Status.Groups = groups

	setJobCondition(nj, nomadv1alpha1.NomadJobCondReady, metav1.ConditionTrue, nomadv1alpha1.ReasonRegistered, "job registered onto Nomad")
	nj.Status.ObservedGeneration = nj.Generation
	if err := r.Status().Update(ctx, nj); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: jobResync}, nil
}

// finalizeJob handles CR deletion. Filled in Task 8.
func (r *NomadJobReconciler) finalizeJob(ctx context.Context, nj *nomadv1alpha1.NomadJob) (ctrl.Result, error) {
	controllerutil.RemoveFinalizer(nj, nomadJobFinalizer)
	return ctrl.Result{}, r.Update(ctx, nj)
}

// dropJobFinalizer removes the cleanup finalizer, allowing Kubernetes to garbage
// collect the CR.
func (r *NomadJobReconciler) dropJobFinalizer(ctx context.Context, nj *nomadv1alpha1.NomadJob) error {
	controllerutil.RemoveFinalizer(nj, nomadJobFinalizer)
	return r.Update(ctx, nj)
}

func (r *NomadJobReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.NewNomadClient == nil {
		r.NewNomadClient = DefaultNomadJobClientFactory
	}
	if r.Recorder == nil {
		r.Recorder = mgr.GetEventRecorderFor("nomadjob")
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&nomadv1alpha1.NomadJob{}).
		Watches(&nomadv1alpha1.NomadCluster{}, handler.EnqueueRequestsFromMapFunc(r.jobsForCluster)).
		Named("nomadjob").
		Complete(r)
}

// jobsForCluster maps a NomadCluster event to the reconcile keys of every
// NomadJob that targets it (so a cluster going Ready reconciles pending jobs).
func (r *NomadJobReconciler) jobsForCluster(ctx context.Context, obj client.Object) []reconcile.Request {
	nc, ok := obj.(*nomadv1alpha1.NomadCluster)
	if !ok {
		return nil
	}
	var list nomadv1alpha1.NomadJobList
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

// setJobCondition upserts a status condition, preserving LastTransitionTime when
// the status is unchanged (mirrors setPoolCondition).
func setJobCondition(nj *nomadv1alpha1.NomadJob, condType string, status metav1.ConditionStatus, reason, msg string) {
	c := metav1.Condition{Type: condType, Status: status, Reason: reason, Message: msg, ObservedGeneration: nj.Generation}
	for i, existing := range nj.Status.Conditions {
		if existing.Type == condType {
			if existing.Status != status {
				c.LastTransitionTime = metav1.Now()
			} else {
				c.LastTransitionTime = existing.LastTransitionTime
			}
			nj.Status.Conditions[i] = c
			return
		}
	}
	c.LastTransitionTime = metav1.Now()
	nj.Status.Conditions = append(nj.Status.Conditions, c)
}
