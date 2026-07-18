package controller

import (
	"context"
	"errors"
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
	"sigs.k8s.io/controller-runtime/pkg/log"
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
	owners := resolveCollisionOwners(bound)
	// One node's failure must not stall the whole cluster's reflection: keep the
	// failed stub in bound (so pruneAbsent won't delete a healthy CR), accumulate
	// its error, and still run markDuplicates + pruneAbsent. A non-nil joined
	// error is returned so genuinely-transient per-node failures still retry.
	var errs []error
	for _, stub := range bound {
		if err := r.upsertNode(ctx, &nc, stub, bound, owners, ops); err != nil {
			errs = append(errs, err)
		}
	}
	if err := r.markDuplicates(ctx, &nc, dupes); err != nil {
		errs = append(errs, err)
	}
	if err := r.pruneAbsent(ctx, &nc, bound, dupes); err != nil {
		errs = append(errs, err)
	}
	if err := errors.Join(errs...); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: nodeResync}, nil
}

// bindNodes groups stubs by Name and, per Name, binds the single non-down stub
// (tie-breaking on highest CreateIndex if several non-down share a Name).
// A down straggler from a re-registered box is ignored, not treated as a
// duplicate. dupes[name]=true marks Names with two-or-more NON-down stubs
// (genuine ambiguity) — those are surfaced, not bound.
func bindNodes(stubs []*api.NodeListStub) (map[string]*api.NodeListStub, map[string]bool) {
	byName := map[string][]*api.NodeListStub{}
	for _, s := range stubs {
		byName[s.Name] = append(byName[s.Name], s)
	}
	bound := map[string]*api.NodeListStub{}
	dupes := map[string]bool{}
	for name, group := range byName {
		var live []*api.NodeListStub
		for _, s := range group {
			if s.Status != api.NodeStatusDown {
				live = append(live, s)
			}
		}
		switch len(live) {
		case 0:
			// all down — bind the freshest so the box stays visible until GC
			best := group[0]
			for _, s := range group {
				if s.CreateIndex > best.CreateIndex {
					best = s
				}
			}
			bound[name] = best
		case 1:
			bound[name] = live[0]
		default:
			dupes[name] = true // genuine ambiguity: refuse to guess
		}
	}
	return bound, dupes
}

// resolveCollisionOwners maps each sanitized object name to the single Nomad
// node Name that owns its CR when multiple distinct Names sanitize to the same
// object name. The owner is chosen deterministically (lowest CreateIndex, then
// Name), so ownership never flaps with map-iteration order (M-1).
func resolveCollisionOwners(bound map[string]*api.NodeListStub) map[string]string {
	best := map[string]*api.NodeListStub{}
	for _, stub := range bound {
		obj := sanitizeNodeName(stub.Name)
		cur, ok := best[obj]
		if !ok || stub.CreateIndex < cur.CreateIndex ||
			(stub.CreateIndex == cur.CreateIndex && stub.Name < cur.Name) {
			best[obj] = stub
		}
	}
	owners := make(map[string]string, len(best))
	for obj, stub := range best {
		owners[obj] = stub.Name
	}
	return owners
}

// upsertNode creates-or-updates the NomadNode for one bound stub: sanitized
// metadata.name, ownerRef to the cluster, spec seeded ONCE at create, status
// mirrored every pass, then desired state driven onto Nomad.
func (r *NomadNodeReconciler) upsertNode(ctx context.Context, nc *nomadv1alpha1.NomadCluster, stub *api.NodeListStub, bound map[string]*api.NodeListStub, owners map[string]string, ops NomadNodeOps) error {
	objName := sanitizeNodeName(stub.Name)
	var nn nomadv1alpha1.NomadNode
	err := r.Get(ctx, types.NamespacedName{Name: objName, Namespace: nc.Namespace}, &nn)
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	mint := apierrors.IsNotFound(err)

	// An existing CR is owned by its Spec.NodeName; a different colliding node
	// must not hijack it or clobber its status — skip deterministically (M-1).
	// Exception (M-2): re-adopt the CR when its recorded owner has disappeared
	// this pass AND this stub is now the deterministic owner. Guarding on both
	// keeps a genuine live collision (recorded owner still present) skipping the
	// loser, and — when the owner is gone but two colliders survive — lets only
	// the deterministic owner re-own, so ownership never flaps.
	if !mint && nn.Spec.NodeName != stub.Name {
		_, ownerLive := bound[nn.Spec.NodeName]
		if ownerLive || owners[objName] != stub.Name {
			log.FromContext(ctx).Info("skipping node whose sanitized name collides with an existing owner",
				"node", stub.Name, "object", objName, "owner", nn.Spec.NodeName)
			return nil
		}
		// Recorded owner is gone; re-adopt the object to this live owner. NodeName
		// is immutable (CRD CEL rule), so the identity cannot be rewritten in place:
		// delete the stale CR and re-mint it under the live owner (same object
		// name). NomadNode carries no finalizer, so the delete is synchronous.
		log.FromContext(ctx).Info("re-adopting node whose recorded owner has disappeared",
			"node", stub.Name, "object", objName, "formerOwner", nn.Spec.NodeName)
		if err := r.Delete(ctx, &nn); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
		nn = nomadv1alpha1.NomadNode{}
		mint = true
	}

	if mint {
		// Only the deterministic owner mints the CR; a colliding loser skips so
		// ownership never flaps with map-iteration order (M-1).
		if owners[objName] != stub.Name {
			log.FromContext(ctx).Info("skipping node whose sanitized name collides with the owner",
				"node", stub.Name, "object", objName, "owner", owners[objName])
			return nil
		}
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

const defaultDrainDeadline = time.Hour

// drainSpecDiverged reports whether the desired drain spec differs from the
// drain currently in flight on Nomad (live). The desired deadline uses the same
// 1h default as driveDesired so a nil-vs-default never false-fires. An
// unreadable live strategy (nil node / nil DrainStrategy) returns false: a
// divergence we cannot substantiate is not warned.
func drainSpecDiverged(nn *nomadv1alpha1.NomadNode, live *api.Node) bool {
	if live == nil || live.DrainStrategy == nil {
		return false
	}
	desiredDeadline := defaultDrainDeadline
	if nn.Spec.Drain.Deadline != nil {
		desiredDeadline = nn.Spec.Drain.Deadline.Duration
	}
	return desiredDeadline != live.DrainStrategy.Deadline ||
		nn.Spec.Drain.IgnoreSystemJobs != live.DrainStrategy.IgnoreSystemJobs
}

// driveDesired reconciles spec.eligible/drain onto Nomad. Drain, when present,
// transiently dominates eligibility (Nomad forces a draining node ineligible),
// so eligibility is only driven when no drain is desired.
func (r *NomadNodeReconciler) driveDesired(ctx context.Context, nn *nomadv1alpha1.NomadNode, stub *api.NodeListStub, ops NomadNodeOps) error {
	if nn.Spec.Drain != nil {
		if drainHandledThisGeneration(nn, stub) {
			return nil // in progress (converging) or complete (converged)
		}
		// Adoption: the node is ALREADY draining (e.g. drained out-of-band, seeded
		// at first mint). Don't re-issue — it would restart the deadline. Mark this
		// generation handled and persist it (L-3, using the L-1 immediate-persist).
		if stub.Drain {
			// A generation bump on an already-draining node can mean the user
			// EDITED the drain spec; that edit is silently dropped (re-issuing
			// would restart the deadline — L-3). Surface it: if the desired spec
			// diverges from the in-flight drain, flag DrainSpecPendingRestart so
			// the ignored edit is observable. It clears (False) once the spec
			// again matches the in-flight drain (e.g. the edit is reverted).
			live, err := ops.NodeInfo(ctx, stub.ID)
			if err != nil {
				live = nil // unreadable live strategy: don't warn on data we lack
			}
			if drainSpecDiverged(nn, live) {
				setNodeCondition(nn, nomadv1alpha1.NomadNodeCondDrainSpecPendingRestart, metav1.ConditionTrue,
					nomadv1alpha1.ReasonDrainSpecEdited, "drain spec edited mid-drain; the change takes effect on the next re-issued drain, not the in-flight one")
			} else {
				setNodeCondition(nn, nomadv1alpha1.NomadNodeCondDrainSpecPendingRestart, metav1.ConditionFalse,
					nomadv1alpha1.ReasonDrainSpecInSync, "desired drain spec matches the in-flight drain")
			}
			nn.Status.DrainObservedGeneration = nn.Generation
			return r.Status().Update(ctx, nn)
		}
		deadline := defaultDrainDeadline
		if nn.Spec.Drain.Deadline != nil {
			deadline = nn.Spec.Drain.Deadline.Duration // explicit value, incl. 0 = no deadline
		}
		spec := &api.DrainSpec{Deadline: deadline, IgnoreSystemJobs: nn.Spec.Drain.IgnoreSystemJobs}
		if err := ops.UpdateDrain(ctx, stub.ID, spec, false); err != nil {
			return err
		}
		nn.Status.DrainObservedGeneration = nn.Generation
		// Persist the generation NOW, decoupled from mirrorStatus: a later
		// status-write failure must not lose it and cause a re-issue (L-1).
		return r.Status().Update(ctx, nn)
	}

	// No drain desired. If the node is still draining, cancel it, marking it
	// eligible per spec.eligible.
	if stub.Drain {
		return ops.UpdateDrain(ctx, stub.ID, nil, nn.Spec.Eligible)
	}
	// Otherwise reconcile eligibility directly, compare-before-write.
	want := api.NodeSchedulingEligible
	if !nn.Spec.Eligible {
		want = api.NodeSchedulingIneligible
	}
	if stub.SchedulingEligibility != want {
		return ops.SetEligibility(ctx, stub.ID, nn.Spec.Eligible)
	}
	return nil
}

// drainHandledThisGeneration reports whether the drain requested at the current
// spec generation has already been issued and is either IN PROGRESS or COMPLETE
// — in both cases UpdateDrain must NOT be re-issued. Re-issuing a running drain
// restarts its relative deadline, so it would slide forever (design §3.3). A
// drain cancelled out-of-band (Node.Drain==false with LastDrain != complete)
// matches neither, so it is re-issued — spec wins.
func drainHandledThisGeneration(nn *nomadv1alpha1.NomadNode, stub *api.NodeListStub) bool {
	if nn.Status.DrainObservedGeneration != nn.Generation {
		return false
	}
	inProgress := stub.Drain
	complete := !stub.Drain &&
		stub.SchedulingEligibility == api.NodeSchedulingIneligible &&
		stub.LastDrain != nil && stub.LastDrain.Status == api.DrainStatusComplete
	return inProgress || complete
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

// markDuplicates sets DuplicateNodeName on the CR for each ambiguous Name
// (creating a minimal CR if none exists yet) without binding it to any node.
func (r *NomadNodeReconciler) markDuplicates(ctx context.Context, nc *nomadv1alpha1.NomadCluster, dupes map[string]bool) error {
	for name := range dupes {
		objName := sanitizeNodeName(name)
		var nn nomadv1alpha1.NomadNode
		err := r.Get(ctx, types.NamespacedName{Name: objName, Namespace: nc.Namespace}, &nn)
		if apierrors.IsNotFound(err) {
			nn = nomadv1alpha1.NomadNode{
				ObjectMeta: metav1.ObjectMeta{Name: objName, Namespace: nc.Namespace, Labels: names(nc).Labels()},
				Spec:       nomadv1alpha1.NomadNodeSpec{ClusterRef: nomadv1alpha1.NodeReference{Name: nc.Name}, NodeName: name},
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
		} else if err != nil {
			return err
		}
		setNodeCondition(&nn, nomadv1alpha1.NomadNodeCondReconciled, metav1.ConditionFalse, nomadv1alpha1.ReasonDuplicateNodeName, "two or more non-down nodes share this Name")
		if err := r.Status().Update(ctx, &nn); err != nil {
			return err
		}
	}
	return nil
}

// pruneAbsent deletes this cluster's NomadNode CRs whose node Name is absent
// from the successful list (Nomad GC'd them). It is only reached after a
// successful ListNodes (the reconcile returns earlier on list error), so a
// transient outage never prunes.
func (r *NomadNodeReconciler) pruneAbsent(ctx context.Context, nc *nomadv1alpha1.NomadCluster, bound map[string]*api.NodeListStub, dupes map[string]bool) error {
	present := map[string]bool{}
	for name := range bound {
		present[sanitizeNodeName(name)] = true
	}
	for name := range dupes {
		// markDuplicates created these CRs this pass; their nodes ARE present
		// (just ambiguous), so they must not be pruned.
		present[sanitizeNodeName(name)] = true
	}
	var list nomadv1alpha1.NomadNodeList
	if err := r.List(ctx, &list, client.InNamespace(nc.Namespace), client.MatchingLabels(names(nc).Labels())); err != nil {
		return err
	}
	// L-2: a successful-but-empty node list would prune EVERY CR. Treat a
	// sudden full-empty as suspect (transient API glitch) rather than a genuine
	// scale-to-zero: skip the mass-delete and warn. Accepted consequence: a
	// cluster that legitimately runs zero clients retains its (stale) CRs until
	// a node reappears (non-empty list -> normal per-node prune) or the cluster
	// is deleted (ownerRef GC). This is preferred over a spurious mass-delete.
	if len(present) == 0 && len(list.Items) > 0 {
		log.FromContext(ctx).Info("skipping prune: node list is unexpectedly empty while CRs exist (L-2)",
			"cluster", nc.Name, "existingCRs", len(list.Items))
		return nil
	}
	for i := range list.Items {
		nn := &list.Items[i]
		if nn.Spec.ClusterRef.Name != nc.Name {
			continue
		}
		if !present[nn.Name] {
			if err := r.Delete(ctx, nn); err != nil && !apierrors.IsNotFound(err) {
				return err
			}
		}
	}
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
