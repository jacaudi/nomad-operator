package controller

import (
	"context"
	"net/http"
	"net/http/httptest"

	"github.com/hashicorp/nomad/api"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	nomadv1alpha1 "github.com/jacaudi/nomad-operator/api/v1alpha1"
	"github.com/jacaudi/nomad-operator/internal/nomad"
)

// notEmptyErr returns a genuine "namespace has non-terminal jobs" error by
// round-tripping a 400 through a throwaway nomad client — the proven pattern the
// job controller tests use to obtain a real api.UnexpectedResponseError. No
// production test-seam is added.
func notEmptyErr() error {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `namespace "team-a" has non-terminal jobs`, http.StatusBadRequest)
	}))
	DeferCleanup(srv.Close)
	c, err := nomad.New(nomad.Config{Address: srv.URL})
	Expect(err).NotTo(HaveOccurred())
	return c.DeleteNamespace(context.Background(), "team-a")
}

var _ = Describe("NomadNamespace reconciler: cluster resolution", func() {
	It("sets ClusterNotFound and adds the finalizer when the cluster is missing", func(ctx SpecContext) {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "nn-notfound-"}}
		Expect(k8s.Create(ctx, ns)).To(Succeed())

		nn := &nomadv1alpha1.NomadNamespace{
			ObjectMeta: metav1.ObjectMeta{Name: "team-a", Namespace: ns.Name},
			Spec: nomadv1alpha1.NomadNamespaceSpec{
				ClusterRef:    nomadv1alpha1.NamespaceClusterRef{Name: "missing"},
				NamespaceName: "team-a",
			},
		}
		Expect(k8s.Create(ctx, nn)).To(Succeed())

		f := newFakeNamespaceOps()
		r := &NomadNamespaceReconciler{Client: k8s, Scheme: k8s.Scheme(), NewNomadClient: f.factory(), Recorder: events.NewFakeRecorder(10)}
		_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "team-a", Namespace: ns.Name}})
		Expect(err).NotTo(HaveOccurred())

		var got nomadv1alpha1.NomadNamespace
		Expect(k8s.Get(ctx, types.NamespacedName{Name: "team-a", Namespace: ns.Name}, &got)).To(Succeed())
		cond := meta.FindStatusCondition(got.Status.Conditions, nomadv1alpha1.NomadNamespaceCondReady)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Reason).To(Equal(nomadv1alpha1.ReasonClusterNotFound))
		Expect(controllerutil.ContainsFinalizer(&got, nomadNamespaceFinalizer)).To(BeTrue(), "finalizer not added")
		Expect(got.Status.ObservedGeneration).To(Equal(got.Generation))
	})

	It("sets ClusterNotReady and registers nothing when the cluster is not Ready", func(ctx SpecContext) {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "nn-notready-"}}
		Expect(k8s.Create(ctx, ns)).To(Succeed())

		nc := readyCluster(ctx, ns.Name)
		nc.Status.Phase = nomadv1alpha1.PhaseDegraded
		Expect(k8s.Status().Update(ctx, nc)).To(Succeed())

		nn := &nomadv1alpha1.NomadNamespace{
			ObjectMeta: metav1.ObjectMeta{Name: "team-a", Namespace: ns.Name},
			Spec: nomadv1alpha1.NomadNamespaceSpec{
				ClusterRef:    nomadv1alpha1.NamespaceClusterRef{Name: nc.Name},
				NamespaceName: "team-a",
			},
		}
		Expect(k8s.Create(ctx, nn)).To(Succeed())

		f := newFakeNamespaceOps()
		r := &NomadNamespaceReconciler{Client: k8s, Scheme: k8s.Scheme(), NewNomadClient: f.factory(), Recorder: events.NewFakeRecorder(10)}
		_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "team-a", Namespace: ns.Name}})
		Expect(err).NotTo(HaveOccurred())

		var got nomadv1alpha1.NomadNamespace
		Expect(k8s.Get(ctx, types.NamespacedName{Name: "team-a", Namespace: ns.Name}, &got)).To(Succeed())
		cond := meta.FindStatusCondition(got.Status.Conditions, nomadv1alpha1.NomadNamespaceCondReady)
		Expect(cond.Reason).To(Equal(nomadv1alpha1.ReasonClusterNotReady))
		Expect(f.registered).To(BeEmpty(), "must not Register when cluster not Ready")
	})
})

var _ = Describe("NomadNamespace reconciler: apply", func() {
	It("registers on create and preserves unmanaged fields on update", func(ctx SpecContext) {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "nn-apply-"}}
		Expect(k8s.Create(ctx, ns)).To(Succeed())
		nc := readyCluster(ctx, ns.Name)

		nn := &nomadv1alpha1.NomadNamespace{
			ObjectMeta: metav1.ObjectMeta{Name: "team-a", Namespace: ns.Name},
			Spec: nomadv1alpha1.NomadNamespaceSpec{
				ClusterRef:    nomadv1alpha1.NamespaceClusterRef{Name: nc.Name},
				NamespaceName: "team-a",
				Description:   "Team A",
				Meta:          map[string]string{"owner": "team-a"},
			},
		}
		Expect(k8s.Create(ctx, nn)).To(Succeed())

		f := newFakeNamespaceOps()
		// Seed an existing namespace carrying an unmanaged Quota to prove preservation.
		f.namespaces["team-a"] = &api.Namespace{Name: "team-a", Quota: "q1", Description: "old"}
		f.jobCount = 3
		r := &NomadNamespaceReconciler{Client: k8s, Scheme: k8s.Scheme(), NewNomadClient: f.factory(), Recorder: events.NewFakeRecorder(10)}
		_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "team-a", Namespace: ns.Name}})
		Expect(err).NotTo(HaveOccurred())

		Expect(f.registered).To(HaveLen(1))
		Expect(f.registered[0].Description).To(Equal("Team A"))
		Expect(f.registered[0].Meta).To(Equal(map[string]string{"owner": "team-a"}))
		Expect(f.registered[0].Quota).To(Equal("q1"), "unmanaged Quota must be preserved")

		var got nomadv1alpha1.NomadNamespace
		Expect(k8s.Get(ctx, types.NamespacedName{Name: "team-a", Namespace: ns.Name}, &got)).To(Succeed())
		Expect(got.Status.JobCount).To(Equal(3))
		cond := meta.FindStatusCondition(got.Status.Conditions, nomadv1alpha1.NomadNamespaceCondReady)
		Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	})

	It("does not re-register when description and meta are unchanged", func(ctx SpecContext) {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "nn-nodrift-"}}
		Expect(k8s.Create(ctx, ns)).To(Succeed())
		nc := readyCluster(ctx, ns.Name)

		nn := &nomadv1alpha1.NomadNamespace{
			ObjectMeta: metav1.ObjectMeta{Name: "team-a", Namespace: ns.Name},
			Spec: nomadv1alpha1.NomadNamespaceSpec{
				ClusterRef: nomadv1alpha1.NamespaceClusterRef{Name: nc.Name}, NamespaceName: "team-a",
				Description: "same", Meta: map[string]string{"k": "v"},
			},
		}
		Expect(k8s.Create(ctx, nn)).To(Succeed())
		f := newFakeNamespaceOps()
		f.namespaces["team-a"] = &api.Namespace{Name: "team-a", Description: "same", Meta: map[string]string{"k": "v"}}
		r := &NomadNamespaceReconciler{Client: k8s, Scheme: k8s.Scheme(), NewNomadClient: f.factory(), Recorder: events.NewFakeRecorder(10)}
		_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "team-a", Namespace: ns.Name}})
		Expect(err).NotTo(HaveOccurred())
		Expect(f.registered).To(BeEmpty(), "no Register when nothing drifted")
	})
})

var _ = Describe("NomadNamespace reconciler: conflict", func() {
	It("sets NamespaceNameConflict and skips Register when a live sibling shares the name", func(ctx SpecContext) {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "nn-conflict-"}}
		Expect(k8s.Create(ctx, ns)).To(Succeed())
		nc := readyCluster(ctx, ns.Name)

		first := &nomadv1alpha1.NomadNamespace{
			ObjectMeta: metav1.ObjectMeta{Name: "team-a-1", Namespace: ns.Name},
			Spec:       nomadv1alpha1.NomadNamespaceSpec{ClusterRef: nomadv1alpha1.NamespaceClusterRef{Name: nc.Name}, NamespaceName: "team-a"},
		}
		Expect(k8s.Create(ctx, first)).To(Succeed())
		second := &nomadv1alpha1.NomadNamespace{
			ObjectMeta: metav1.ObjectMeta{Name: "team-a-2", Namespace: ns.Name},
			Spec:       nomadv1alpha1.NomadNamespaceSpec{ClusterRef: nomadv1alpha1.NamespaceClusterRef{Name: nc.Name}, NamespaceName: "team-a"},
		}
		Expect(k8s.Create(ctx, second)).To(Succeed())

		f := newFakeNamespaceOps()
		r := &NomadNamespaceReconciler{Client: k8s, Scheme: k8s.Scheme(), NewNomadClient: f.factory(), Recorder: events.NewFakeRecorder(10)}
		_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "team-a-2", Namespace: ns.Name}})
		Expect(err).NotTo(HaveOccurred())

		var got nomadv1alpha1.NomadNamespace
		Expect(k8s.Get(ctx, types.NamespacedName{Name: "team-a-2", Namespace: ns.Name}, &got)).To(Succeed())
		cond := meta.FindStatusCondition(got.Status.Conditions, nomadv1alpha1.NomadNamespaceCondReady)
		Expect(cond.Reason).To(Equal(nomadv1alpha1.ReasonNamespaceNameConflict))
		Expect(f.registered).To(BeEmpty())
	})
})

var _ = Describe("NomadNamespace reconciler: finalize", func() {
	newNS := func(ctx SpecContext, k8sNS, cluster string, del bool) *nomadv1alpha1.NomadNamespace {
		nn := &nomadv1alpha1.NomadNamespace{
			ObjectMeta: metav1.ObjectMeta{Name: "team-a", Namespace: k8sNS, Finalizers: []string{nomadNamespaceFinalizer}},
			Spec:       nomadv1alpha1.NomadNamespaceSpec{ClusterRef: nomadv1alpha1.NamespaceClusterRef{Name: cluster}, NamespaceName: "team-a"},
		}
		Expect(k8s.Create(ctx, nn)).To(Succeed())
		Expect(k8s.Delete(ctx, nn)).To(Succeed()) // sets DeletionTimestamp; finalizer holds it
		var got nomadv1alpha1.NomadNamespace
		Expect(k8s.Get(ctx, types.NamespacedName{Name: "team-a", Namespace: k8sNS}, &got)).To(Succeed())
		return &got
	}

	It("deletes the namespace and drops the finalizer when the cluster is Ready and it is empty", func(ctx SpecContext) {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "nn-fin-ok-"}}
		Expect(k8s.Create(ctx, ns)).To(Succeed())
		nc := readyCluster(ctx, ns.Name)
		nn := newNS(ctx, ns.Name, nc.Name, true)

		f := newFakeNamespaceOps()
		r := &NomadNamespaceReconciler{Client: k8s, Scheme: k8s.Scheme(), NewNomadClient: f.factory(), Recorder: events.NewFakeRecorder(10)}
		_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: nn.Name, Namespace: ns.Name}})
		Expect(err).NotTo(HaveOccurred())
		Expect(f.deleted).To(ConsistOf("team-a"))
		// CR should now be gone (finalizer dropped → GC).
		var got nomadv1alpha1.NomadNamespace
		Expect(apierrors.IsNotFound(k8s.Get(ctx, types.NamespacedName{Name: nn.Name, Namespace: ns.Name}, &got))).To(BeTrue())
	})

	It("holds with NamespaceNotEmpty when Delete is refused for non-terminal jobs", func(ctx SpecContext) {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "nn-fin-busy-"}}
		Expect(k8s.Create(ctx, ns)).To(Succeed())
		nc := readyCluster(ctx, ns.Name)
		nn := newNS(ctx, ns.Name, nc.Name, true)

		f := newFakeNamespaceOps()
		f.jobCount = 2
		f.deleteErr = notEmptyErr() // a genuine "not empty" api.UnexpectedResponseError the classifier matches
		r := &NomadNamespaceReconciler{Client: k8s, Scheme: k8s.Scheme(), NewNomadClient: f.factory(), Recorder: events.NewFakeRecorder(10)}
		_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: nn.Name, Namespace: ns.Name}})
		Expect(err).NotTo(HaveOccurred())
		var got nomadv1alpha1.NomadNamespace
		Expect(k8s.Get(ctx, types.NamespacedName{Name: nn.Name, Namespace: ns.Name}, &got)).To(Succeed())
		cond := meta.FindStatusCondition(got.Status.Conditions, nomadv1alpha1.NomadNamespaceCondDeleteBlocked)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Reason).To(Equal(nomadv1alpha1.ReasonNamespaceNotEmpty))
		Expect(got.Status.JobCount).To(Equal(2))
		Expect(controllerutil.ContainsFinalizer(&got, nomadNamespaceFinalizer)).To(BeTrue())
	})

	It("short-circuits (drops finalizer, no Delete) when the cluster is gone", func(ctx SpecContext) {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "nn-fin-gone-"}}
		Expect(k8s.Create(ctx, ns)).To(Succeed())
		nn := newNS(ctx, ns.Name, "missing-cluster", true)

		f := newFakeNamespaceOps()
		r := &NomadNamespaceReconciler{Client: k8s, Scheme: k8s.Scheme(), NewNomadClient: f.factory(), Recorder: events.NewFakeRecorder(10)}
		_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: nn.Name, Namespace: ns.Name}})
		Expect(err).NotTo(HaveOccurred())
		Expect(f.deleted).To(BeEmpty(), "must not call Delete when cluster is gone")
		var got nomadv1alpha1.NomadNamespace
		Expect(apierrors.IsNotFound(k8s.Get(ctx, types.NamespacedName{Name: nn.Name, Namespace: ns.Name}, &got))).To(BeTrue())
	})

	// Guards the Terminating-but-present half of clusterGoneOrGoing — the
	// NomadPool §3.4 foreground-cascade-deadlock fix and the reason this task
	// exists. SetControllerReference sets BlockOwnerDeletion, so a Terminating
	// cluster is present-with-DeletionTimestamp (NOT NotFound); the reconcile
	// must still short-circuit without a Delete. Reuses the shared
	// mustCreateTerminatingCluster (nomadpool_controller_test.go).
	It("short-circuits (drops finalizer, no Delete) when the cluster is present but Terminating", func(ctx SpecContext) {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "nn-fin-terminating-"}}
		Expect(k8s.Create(ctx, ns)).To(Succeed())
		nc := mustCreateTerminatingCluster(ctx, ns.Name)
		nn := newNS(ctx, ns.Name, nc.Name, true)

		f := newFakeNamespaceOps()
		r := &NomadNamespaceReconciler{Client: k8s, Scheme: k8s.Scheme(), NewNomadClient: f.factory(), Recorder: events.NewFakeRecorder(10)}
		_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: nn.Name, Namespace: ns.Name}})
		Expect(err).NotTo(HaveOccurred())
		Expect(f.deleted).To(BeEmpty(), "must not call Delete when cluster is Terminating")
		var got nomadv1alpha1.NomadNamespace
		Expect(apierrors.IsNotFound(k8s.Get(ctx, types.NamespacedName{Name: nn.Name, Namespace: ns.Name}, &got))).To(BeTrue())
	})
})

var _ = Describe("NomadNamespace CEL", func() {
	It("rejects namespaceName 'default'", func(ctx SpecContext) {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "nn-cel-default-"}}
		Expect(k8s.Create(ctx, ns)).To(Succeed())
		nn := &nomadv1alpha1.NomadNamespace{
			ObjectMeta: metav1.ObjectMeta{Name: "d", Namespace: ns.Name},
			Spec:       nomadv1alpha1.NomadNamespaceSpec{ClusterRef: nomadv1alpha1.NamespaceClusterRef{Name: "c"}, NamespaceName: "default"},
		}
		Expect(k8s.Create(ctx, nn)).NotTo(Succeed())
	})

	It("rejects a namespaceName with illegal characters", func(ctx SpecContext) {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "nn-cel-pattern-"}}
		Expect(k8s.Create(ctx, ns)).To(Succeed())
		nn := &nomadv1alpha1.NomadNamespace{
			ObjectMeta: metav1.ObjectMeta{Name: "bad", Namespace: ns.Name},
			Spec:       nomadv1alpha1.NomadNamespaceSpec{ClusterRef: nomadv1alpha1.NamespaceClusterRef{Name: "c"}, NamespaceName: "has spaces!"},
		}
		Expect(k8s.Create(ctx, nn)).NotTo(Succeed())
	})

	It("rejects mutating namespaceName", func(ctx SpecContext) {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "nn-cel-immutable-"}}
		Expect(k8s.Create(ctx, ns)).To(Succeed())
		nn := &nomadv1alpha1.NomadNamespace{
			ObjectMeta: metav1.ObjectMeta{Name: "team-a", Namespace: ns.Name},
			Spec:       nomadv1alpha1.NomadNamespaceSpec{ClusterRef: nomadv1alpha1.NamespaceClusterRef{Name: "c"}, NamespaceName: "team-a"},
		}
		Expect(k8s.Create(ctx, nn)).To(Succeed())
		nn.Spec.NamespaceName = "team-b"
		Expect(k8s.Update(ctx, nn)).NotTo(Succeed())
	})
})
