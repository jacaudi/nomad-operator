package controller

import (
	"context"
	"time"

	"github.com/hashicorp/nomad/api"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	nomadv1alpha1 "github.com/jacaudi/nomad-operator/api/v1alpha1"
)

const nodeResync = 30 * time.Second

// NomadNodeReconciler reflects a NomadCluster's registered nodes into NomadNode
// CRs and drives eligibility/drain onto Nomad. Its primary object is the
// NomadCluster (a NomadNode-keyed reconciler could never mint the first CR),
// so each Reconcile handles one cluster's whole node set.
type NomadNodeReconciler struct {
	client.Client
	Scheme         *runtime.Scheme
	NewNomadClient NomadNodeClientFactory
}

// +kubebuilder:rbac:groups=nomad.operator.io,resources=nomadnodes,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=nomad.operator.io,resources=nomadnodes/status,verbs=get;update;patch

func (r *NomadNodeReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var nc nomadv1alpha1.NomadCluster
	if err := r.Get(ctx, req.NamespacedName, &nc); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if nc.Status.Phase != nomadv1alpha1.PhaseReady {
		// Not listable — flag the cluster's existing nodes, leave status stale.
		if err := r.markClusterNotReady(ctx, &nc); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: nodeResync}, nil
	}

	cfg, err := clusterNomadConfig(ctx, r.Client, &nc)
	if err != nil {
		return ctrl.Result{}, err
	}
	ops, err := r.NewNomadClient(cfg)
	if err != nil {
		return ctrl.Result{}, err
	}
	stubs, err := ops.ListNodes(ctx)
	if err != nil {
		// Transient (unreachable): prune nothing; flag the nodes ClusterNotReady.
		if merr := r.markClusterNotReady(ctx, &nc); merr != nil {
			return ctrl.Result{}, merr
		}
		return ctrl.Result{RequeueAfter: nodeResync}, nil
	}

	bound, dupes := bindNodes(stubs)
	for _, stub := range bound {
		if err := r.upsertNode(ctx, &nc, stub, ops); err != nil {
			return ctrl.Result{}, err
		}
	}
	if err := r.markDuplicates(ctx, &nc, dupes); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.pruneAbsent(ctx, &nc, bound, dupes); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: nodeResync}, nil
}

// bindNodes maps node Name -> the stub to manage. Task 5 uses a naive
// last-wins binding; Task 6 replaces the body with down-straggler
// disambiguation. dupes holds Names with genuine ambiguity.
func bindNodes(stubs []*api.NodeListStub) (map[string]*api.NodeListStub, map[string]bool) {
	bound := map[string]*api.NodeListStub{}
	for _, s := range stubs {
		bound[s.Name] = s
	}
	return bound, map[string]bool{}
}

// upsertNode creates-or-updates the NomadNode for one bound stub: sanitized
// metadata.name, ownerRef to the cluster, spec seeded ONCE at create, status
// mirrored every pass, then desired state driven onto Nomad.
func (r *NomadNodeReconciler) upsertNode(ctx context.Context, nc *nomadv1alpha1.NomadCluster, stub *api.NodeListStub, ops NomadNodeOps) error {
	objName := sanitizeNodeName(stub.Name)
	var nn nomadv1alpha1.NomadNode
	err := r.Get(ctx, types.NamespacedName{Name: objName, Namespace: nc.Namespace}, &nn)
	switch {
	case apierrors.IsNotFound(err):
		nn = nomadv1alpha1.NomadNode{
			ObjectMeta: metav1.ObjectMeta{Name: objName, Namespace: nc.Namespace, Labels: names(nc).Labels()},
			Spec: nomadv1alpha1.NomadNodeSpec{
				ClusterRef: nomadv1alpha1.NodeReference{Name: nc.Name},
				NodeName:   stub.Name,
				Eligible:   stub.SchedulingEligibility != api.NodeSchedulingIneligible, // seed from observed
			},
		}
		if stub.Drain { // seed drain presence; fetch detail only for a draining node
			nn.Spec.Drain = r.seedDrain(ctx, stub.ID, ops)
		}
		if err := controllerutil.SetControllerReference(nc, &nn, r.Scheme); err != nil {
			return err
		}
		if err := r.Create(ctx, &nn); err != nil {
			if !apierrors.IsAlreadyExists(err) {
				return err
			}
			// Lost a create race — re-Get so we hold a fresh object (with a
			// resourceVersion) for the status update below.
			if err := r.Get(ctx, types.NamespacedName{Name: objName, Namespace: nc.Namespace}, &nn); err != nil {
				return err
			}
		}
	case err != nil:
		return err
	}
	// Sanitization collision: a DIFFERENT node's Name maps to this object name.
	// Refuse to hijack the existing CR — surface DuplicateNodeName and skip
	// driving/mirroring (design §3.1/§3.2).
	if nn.Spec.NodeName != stub.Name {
		setNodeCondition(&nn, nomadv1alpha1.NomadNodeCondReconciled, metav1.ConditionFalse, nomadv1alpha1.ReasonDuplicateNodeName, "another node's Name sanitizes to this object name")
		return r.Status().Update(ctx, &nn)
	}
	// Drive desired state onto Nomad (Task 7 fills driveDesired), then mirror.
	if err := r.driveDesired(ctx, &nn, stub, ops); err != nil {
		return err
	}
	return r.mirrorStatus(ctx, &nn, stub)
}

// seedDrain fetches the active drain spec of a node draining at first mint.
func (r *NomadNodeReconciler) seedDrain(ctx context.Context, id string, ops NomadNodeOps) *nomadv1alpha1.NodeDrainSpec {
	node, err := ops.NodeInfo(ctx, id)
	if err != nil || node == nil || node.DrainStrategy == nil {
		return &nomadv1alpha1.NodeDrainSpec{} // presence only; deadline defaults in driveDesired
	}
	return &nomadv1alpha1.NodeDrainSpec{
		Deadline:         &metav1.Duration{Duration: node.DrainStrategy.Deadline},
		IgnoreSystemJobs: node.DrainStrategy.IgnoreSystemJobs,
	}
}

// driveDesired reconciles spec.eligible/drain onto Nomad. Task 7 implements it;
// Task 5 ships a no-op so mint/mirror are independently testable.
func (r *NomadNodeReconciler) driveDesired(_ context.Context, _ *nomadv1alpha1.NomadNode, _ *api.NodeListStub, _ NomadNodeOps) error {
	return nil
}

// mirrorStatus writes the observed node state into NomadNode.status.
func (r *NomadNodeReconciler) mirrorStatus(ctx context.Context, nn *nomadv1alpha1.NomadNode, stub *api.NodeListStub) error {
	nn.Status.NodeID = stub.ID
	nn.Status.Status = stub.Status
	nn.Status.SchedulingEligibility = stub.SchedulingEligibility
	nn.Status.Draining = stub.Drain
	nn.Status.NodeClass = stub.NodeClass
	nn.Status.NodePool = stub.NodePool
	nn.Status.Datacenter = stub.Datacenter
	nn.Status.ObservedGeneration = nn.Generation
	if stub.LastDrain != nil {
		nn.Status.LastDrain = &nomadv1alpha1.LastDrainStatus{
			Status:    string(stub.LastDrain.Status),
			StartedAt: &metav1.Time{Time: stub.LastDrain.StartedAt},
			UpdatedAt: &metav1.Time{Time: stub.LastDrain.UpdatedAt},
		}
	}
	setNodeCondition(nn, nomadv1alpha1.NomadNodeCondReconciled, metav1.ConditionTrue, nomadv1alpha1.ReasonReconciled, "reflected")
	return r.Status().Update(ctx, nn)
}

// markClusterNotReady flags every existing NomadNode of this cluster with
// Reconciled=False/ClusterNotReady, leaving mirrored status untouched
// (design §3.4): the cluster isn't listable, so the last-known status is kept.
func (r *NomadNodeReconciler) markClusterNotReady(ctx context.Context, nc *nomadv1alpha1.NomadCluster) error {
	var list nomadv1alpha1.NomadNodeList
	if err := r.List(ctx, &list, client.InNamespace(nc.Namespace), client.MatchingLabels(names(nc).Labels())); err != nil {
		return err
	}
	for i := range list.Items {
		nn := &list.Items[i]
		if nn.Spec.ClusterRef.Name != nc.Name {
			continue
		}
		setNodeCondition(nn, nomadv1alpha1.NomadNodeCondReconciled, metav1.ConditionFalse, nomadv1alpha1.ReasonClusterNotReady, "referenced NomadCluster is not Ready")
		if err := r.Status().Update(ctx, nn); err != nil {
			return err
		}
	}
	return nil
}

// markDuplicates / pruneAbsent are filled in Tasks 6 and 8; no-op stubs here.
func (r *NomadNodeReconciler) markDuplicates(_ context.Context, _ *nomadv1alpha1.NomadCluster, _ map[string]bool) error {
	return nil
}
func (r *NomadNodeReconciler) pruneAbsent(_ context.Context, _ *nomadv1alpha1.NomadCluster, _ map[string]*api.NodeListStub, _ map[string]bool) error {
	return nil
}

// clusterForNode maps a NomadNode event to its owning cluster's reconcile key.
func (r *NomadNodeReconciler) clusterForNode(_ context.Context, obj client.Object) []reconcile.Request {
	nn, ok := obj.(*nomadv1alpha1.NomadNode)
	if !ok {
		return nil
	}
	return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: nn.Spec.ClusterRef.Name, Namespace: nn.Namespace}}}
}

func (r *NomadNodeReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.NewNomadClient == nil {
		r.NewNomadClient = DefaultNomadNodeClientFactory
	}
	// The reflector's primary object is the cluster. A NomadNode event maps back
	// to its cluster via clusterForNode (spec.clusterRef), which is a strict
	// superset of what Owns(&NomadNode{}) would enqueue — so Owns is redundant
	// and omitted (KISS). GC cascade needs only the ownerReference set at mint,
	// not Owns.
	return ctrl.NewControllerManagedBy(mgr).
		For(&nomadv1alpha1.NomadCluster{}).
		Watches(&nomadv1alpha1.NomadNode{}, handler.EnqueueRequestsFromMapFunc(r.clusterForNode)).
		Named("nomadnode").
		Complete(r)
}

func setNodeCondition(nn *nomadv1alpha1.NomadNode, condType string, status metav1.ConditionStatus, reason, msg string) {
	meta := metav1.Condition{Type: condType, Status: status, Reason: reason, Message: msg, ObservedGeneration: nn.Generation}
	for i, c := range nn.Status.Conditions {
		if c.Type == condType {
			if c.Status != status {
				meta.LastTransitionTime = metav1.Now()
			} else {
				meta.LastTransitionTime = c.LastTransitionTime
			}
			nn.Status.Conditions[i] = meta
			return
		}
	}
	meta.LastTransitionTime = metav1.Now()
	nn.Status.Conditions = append(nn.Status.Conditions, meta)
}
