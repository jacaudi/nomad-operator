package controller

import (
	"context"
	"errors"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gwapiv1 "sigs.k8s.io/gateway-api/apis/v1"
	gwapiv1a2 "sigs.k8s.io/gateway-api/apis/v1alpha2"

	nomadv1alpha1 "github.com/jacaudi/nomad-operator/api/v1alpha1"
	"github.com/jacaudi/nomad-operator/internal/nomad"
)

func minimalCluster(name, ns string) *nomadv1alpha1.NomadCluster {
	return &nomadv1alpha1.NomadCluster{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: nomadv1alpha1.NomadClusterSpec{
			Image:   "hashicorp/nomad:2.0.4",
			Servers: 3,
			Storage: nomadv1alpha1.StorageSpec{Size: "1Gi"},
			TLS:     nomadv1alpha1.TLSSpec{CertSecretRef: "nomad-tls"},
			ExternalAccess: nomadv1alpha1.ExternalAccessSpec{
				Mode: nomadv1alpha1.ExternalAccessGateway,
				Gateway: &nomadv1alpha1.GatewaySpec{
					Mode: nomadv1alpha1.GatewayModeManaged, ClassName: "cilium",
					RPCPorts: []int32{14647, 24647, 34647}, HTTPHostname: "nomad.example.com",
				},
			},
		},
	}
}

// singleServerCluster is a minimal single-node (non-HA) control plane fixture
// for FR-1: servers=1 with exactly one rpcPort/TCPRoute.
func singleServerCluster(name, ns string) *nomadv1alpha1.NomadCluster {
	return &nomadv1alpha1.NomadCluster{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: nomadv1alpha1.NomadClusterSpec{
			Image:   "hashicorp/nomad:2.0.4",
			Servers: 1,
			Storage: nomadv1alpha1.StorageSpec{Size: "1Gi"},
			TLS:     nomadv1alpha1.TLSSpec{CertSecretRef: "nomad-tls"},
			ExternalAccess: nomadv1alpha1.ExternalAccessSpec{
				Mode: nomadv1alpha1.ExternalAccessGateway,
				Gateway: &nomadv1alpha1.GatewaySpec{
					Mode: nomadv1alpha1.GatewayModeManaged, ClassName: "cilium",
					RPCPorts: []int32{14647}, HTTPHostname: "nomad.example.com",
				},
			},
		},
	}
}

var _ = Describe("NomadCluster reconcile skeleton", func() {
	It("sets Pending phase and Reconciled condition", func() {
		ctx := context.Background()
		nc := minimalCluster("skel", "default")
		Expect(k8s.Create(ctx, nc)).To(Succeed())

		r := &NomadClusterReconciler{Client: k8s, Scheme: k8s.Scheme(), NewNomadClient: newFakeFactory(&fakeNomad{})}
		_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "skel", Namespace: "default"}})
		Expect(err).NotTo(HaveOccurred())

		var got nomadv1alpha1.NomadCluster
		Expect(k8s.Get(ctx, types.NamespacedName{Name: "skel", Namespace: "default"}, &got)).To(Succeed())
		Expect(got.Status.Phase).To(Equal(nomadv1alpha1.PhasePending))
		Expect(meta_IsStatusConditionTrue(got.Status.Conditions, nomadv1alpha1.CondReconciled)).To(BeTrue())
	})

	It("rejects mutation of the immutable servers field", func() {
		ctx := context.Background()
		nc := minimalCluster("immut", "default")
		Expect(k8s.Create(ctx, nc)).To(Succeed())
		nc.Spec.Servers = 5
		Expect(k8s.Update(ctx, nc)).NotTo(Succeed()) // CEL immutability
	})

	// FR-1: a single-node (non-HA) control plane is a user-approved tradeoff --
	// on Kubernetes a failed control-plane pod is rescheduled, so the downtime
	// from servers=1 is minimal and full Raft HA is not always required.
	It("accepts a single-node (servers: 1) control plane with exactly one rpcPort", func() {
		ctx := context.Background()
		nc := singleServerCluster("single", "default")
		Expect(k8s.Create(ctx, nc)).To(Succeed())
	})

	It("still rejects an even servers count (split-brain safety)", func() {
		ctx := context.Background()
		nc := minimalCluster("even", "default")
		nc.Spec.Servers = 2
		nc.Spec.ExternalAccess.Gateway.RPCPorts = []int32{14647, 24647}
		Expect(k8s.Create(ctx, nc)).NotTo(Succeed())
	})

	It("rejects LoadBalancer mode with servers: 3 (LB requires servers: 1)", func() {
		ctx := context.Background()
		nc := minimalCluster("lb-multi", "default")
		nc.Spec.Servers = 3
		nc.Spec.ExternalAccess = nomadv1alpha1.ExternalAccessSpec{
			Mode:         nomadv1alpha1.ExternalAccessLoadBalancer,
			LoadBalancer: &nomadv1alpha1.LoadBalancerSpec{},
		}
		Expect(k8s.Create(ctx, nc)).NotTo(Succeed())
	})

	It("accepts LoadBalancer mode with servers: 1 and no gateway block", func() {
		ctx := context.Background()
		nc := singleServerCluster("lb-single", "default")
		nc.Spec.ExternalAccess = nomadv1alpha1.ExternalAccessSpec{
			Mode:         nomadv1alpha1.ExternalAccessLoadBalancer,
			LoadBalancer: &nomadv1alpha1.LoadBalancerSpec{},
		}
		Expect(k8s.Create(ctx, nc)).To(Succeed())
	})

	It("rejects a gateway block set under LoadBalancer mode (union exclusivity)", func() {
		ctx := context.Background()
		nc := singleServerCluster("lb-badunion", "default")
		nc.Spec.ExternalAccess = nomadv1alpha1.ExternalAccessSpec{
			Mode:    nomadv1alpha1.ExternalAccessLoadBalancer,
			Gateway: &nomadv1alpha1.GatewaySpec{Mode: nomadv1alpha1.GatewayModeManaged, ClassName: "cilium", RPCPorts: []int32{14647}, HTTPHostname: "nomad.example.com"},
		}
		Expect(k8s.Create(ctx, nc)).NotTo(Succeed())
	})

	It("rejects mutation of the immutable externalAccess.mode field", func() {
		ctx := context.Background()
		nc := singleServerCluster("mode-immut", "default")
		Expect(k8s.Create(ctx, nc)).To(Succeed())
		nc.Spec.ExternalAccess.Mode = nomadv1alpha1.ExternalAccessLoadBalancer
		Expect(k8s.Update(ctx, nc)).NotTo(Succeed())
	})
})

func meta_IsStatusConditionTrue(conds []metav1.Condition, t string) bool {
	for _, c := range conds {
		if c.Type == t {
			return c.Status == metav1.ConditionTrue
		}
	}
	return false
}

func meta_IsStatusConditionFalse(conds []metav1.Condition, t string) bool {
	for _, c := range conds {
		if c.Type == t {
			return c.Status == metav1.ConditionFalse
		}
	}
	return false
}

var _ = Describe("Managed provisioning", func() {
	It("creates workloads and routes and reaches Ready when gateway+cert are ready and the fake reports a leader", func() {
		ctx := context.Background()
		ns := "mgd"
		Expect(k8s.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		makeCertSecret(ctx, ns)
		nc := minimalCluster("prod", ns)
		Expect(k8s.Create(ctx, nc)).To(Succeed())

		// Pre-create the Gateway with an assigned address (envtest runs no Gateway controller).
		fake := &fakeNomad{leader: "10.0.0.5:14647", serverHealthy: true}
		r := &NomadClusterReconciler{Client: k8s, Scheme: k8s.Scheme(), NewNomadClient: newFakeFactory(fake)}

		// First reconcile creates the Gateway (no address yet) → Pending.
		reconcileOnce(r, "prod", ns)
		gwName := names(nc).Gateway
		var gw gwapiv1.Gateway
		Expect(k8s.Get(ctx, types.NamespacedName{Name: gwName, Namespace: ns}, &gw)).To(Succeed())

		var afterFirst nomadv1alpha1.NomadCluster
		Expect(k8s.Get(ctx, types.NamespacedName{Name: "prod", Namespace: ns}, &afterFirst)).To(Succeed())
		Expect(afterFirst.Status.Phase).To(Equal(nomadv1alpha1.PhasePending))

		gw.Status.Addresses = []gwapiv1.GatewayStatusAddress{{Value: "10.0.0.5"}}
		Expect(k8s.Status().Update(ctx, &gw)).To(Succeed())

		// Second reconcile provisions workloads → Bootstrapping.
		reconcileOnce(r, "prod", ns)

		var ss appsv1.StatefulSet
		Expect(k8s.Get(ctx, types.NamespacedName{Name: names(nc).StatefulSet, Namespace: ns}, &ss)).To(Succeed())
		Expect(ss.Spec.PodManagementPolicy).To(Equal(appsv1.ParallelPodManagement))
		var tcp gwapiv1a2.TCPRoute
		Expect(k8s.Get(ctx, types.NamespacedName{Name: "prod-rpc-0", Namespace: ns}, &tcp)).To(Succeed())

		var afterSecond nomadv1alpha1.NomadCluster
		Expect(k8s.Get(ctx, types.NamespacedName{Name: "prod", Namespace: ns}, &afterSecond)).To(Succeed())
		// Task 8: the fake reports a leader and bootstraps ACLs successfully, so
		// the same reconcile that provisions workloads also completes bootstrap
		// and reaches Ready (not just Bootstrapping).
		Expect(afterSecond.Status.Phase).To(Equal(nomadv1alpha1.PhaseReady))
	})

	It("persists Bootstrapping (not Ready) and CondQuorumHealthy=False when the fake reports no leader", func() {
		ctx := context.Background()
		ns := "mgd-noleader"
		Expect(k8s.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		makeCertSecret(ctx, ns)
		nc := minimalCluster("noleader", ns)
		Expect(k8s.Create(ctx, nc)).To(Succeed())

		// No leader reported: fakeNomad.Leader() returns an error when leader=="".
		fake := &fakeNomad{}
		r := &NomadClusterReconciler{Client: k8s, Scheme: k8s.Scheme(), NewNomadClient: newFakeFactory(fake)}

		// First reconcile creates the Gateway (no address yet) → Pending.
		reconcileOnce(r, "noleader", ns)
		gwName := names(nc).Gateway
		var gw gwapiv1.Gateway
		Expect(k8s.Get(ctx, types.NamespacedName{Name: gwName, Namespace: ns}, &gw)).To(Succeed())
		gw.Status.Addresses = []gwapiv1.GatewayStatusAddress{{Value: "10.0.0.5"}}
		Expect(k8s.Status().Update(ctx, &gw)).To(Succeed())

		// Second reconcile provisions workloads and reaches bootstrapAndReady,
		// where the no-leader branch must leave Phase at Bootstrapping and
		// mark CondQuorumHealthy False (nomadcluster_controller.go ~197-211).
		reconcileOnce(r, "noleader", ns)

		var got nomadv1alpha1.NomadCluster
		Expect(k8s.Get(ctx, types.NamespacedName{Name: "noleader", Namespace: ns}, &got)).To(Succeed())
		Expect(got.Status.Phase).To(Equal(nomadv1alpha1.PhaseBootstrapping))
		Expect(meta_IsStatusConditionFalse(got.Status.Conditions, nomadv1alpha1.CondQuorumHealthy)).To(BeTrue())
	})

	It("transitions Ready to Degraded (QuorumLost) when the fake later reports no leader", func() {
		ctx := context.Background()
		ns := "mgd-quorumlost"
		Expect(k8s.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		makeCertSecret(ctx, ns)
		nc := minimalCluster("quorumlost", ns)
		Expect(k8s.Create(ctx, nc)).To(Succeed())

		// Same mutable fake+reconciler reused across all three reconciles below,
		// matching the pattern in the two specs above.
		fake := &fakeNomad{leader: "10.0.0.5:14647", serverHealthy: true}
		r := &NomadClusterReconciler{Client: k8s, Scheme: k8s.Scheme(), NewNomadClient: newFakeFactory(fake)}

		// First reconcile creates the Gateway (no address yet) → Pending.
		reconcileOnce(r, "quorumlost", ns)
		gwName := names(nc).Gateway
		var gw gwapiv1.Gateway
		Expect(k8s.Get(ctx, types.NamespacedName{Name: gwName, Namespace: ns}, &gw)).To(Succeed())
		gw.Status.Addresses = []gwapiv1.GatewayStatusAddress{{Value: "10.0.0.5"}}
		Expect(k8s.Status().Update(ctx, &gw)).To(Succeed())

		// Second reconcile provisions workloads and, with a leader reported,
		// reaches Ready.
		reconcileOnce(r, "quorumlost", ns)
		var afterReady nomadv1alpha1.NomadCluster
		Expect(k8s.Get(ctx, types.NamespacedName{Name: "quorumlost", Namespace: ns}, &afterReady)).To(Succeed())
		Expect(afterReady.Status.Phase).To(Equal(nomadv1alpha1.PhaseReady))

		// Mutate the same fake to report no leader, then reconcile again. This
		// exercises bootstrapAndReady's Ready->Degraded guard
		// (nomadcluster_controller.go ~202-206), which requires Reconcile to
		// preserve the Ready phase instead of clobbering it to Bootstrapping
		// before calling bootstrapAndReady (nomadcluster_controller.go ~143).
		fake.leader = ""
		fake.serverHealthy = false
		reconcileOnce(r, "quorumlost", ns)

		var afterLoss nomadv1alpha1.NomadCluster
		Expect(k8s.Get(ctx, types.NamespacedName{Name: "quorumlost", Namespace: ns}, &afterLoss)).To(Succeed())
		Expect(afterLoss.Status.Phase).To(Equal(nomadv1alpha1.PhaseDegraded))
		Expect(meta_IsStatusConditionFalse(afterLoss.Status.Conditions, nomadv1alpha1.CondReady)).To(BeTrue())
		reason := ""
		for _, c := range afterLoss.Status.Conditions {
			if c.Type == nomadv1alpha1.CondReady {
				reason = c.Reason
			}
		}
		Expect(reason).To(Equal("QuorumLost"))
	})

	It("re-attempts ACLBootstrap on the next reconcile after a transient first-attempt failure, and does not report Ready/ACLBootstrapped or annotate the token Secret until confirmed", func() {
		ctx := context.Background()
		ns := "mgd-bootstrap-retry"
		Expect(k8s.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		makeCertSecret(ctx, ns)
		nc := minimalCluster("retry", ns)
		Expect(k8s.Create(ctx, nc)).To(Succeed())

		// Leader is present (quorum healthy) but the FIRST ACLBootstrap call
		// fails transiently (e.g. a leader flap right after election).
		fake := &fakeNomad{leader: "10.0.0.5:14647", serverHealthy: true, bootstrapErr: errors.New("transient: leader flap")}
		r := &NomadClusterReconciler{Client: k8s, Scheme: k8s.Scheme(), NewNomadClient: newFakeFactory(fake)}

		// First reconcile creates the Gateway (no address yet) → Pending.
		reconcileOnce(r, "retry", ns)
		gwName := names(nc).Gateway
		var gw gwapiv1.Gateway
		Expect(k8s.Get(ctx, types.NamespacedName{Name: gwName, Namespace: ns}, &gw)).To(Succeed())
		gw.Status.Addresses = []gwapiv1.GatewayStatusAddress{{Value: "10.0.0.5"}}
		Expect(k8s.Status().Update(ctx, &gw)).To(Succeed())

		// Second reconcile provisions workloads, reaches bootstrapAndReady, and
		// hits the transient ACLBootstrap failure. This is the CRITICAL
		// assertion: the reconciler must NOT report Ready/ACLBootstrapped, and
		// the token Secret it wrote must NOT carry the durable
		// "acl-bootstrapped" marker — otherwise a later reconcile would wrongly
		// treat the un-bootstrapped cluster as confirmed (the security bug this
		// spec guards against).
		reconcileOnce(r, "retry", ns)
		Expect(fake.bootstrapCalls).To(Equal(1))

		var afterFailure nomadv1alpha1.NomadCluster
		Expect(k8s.Get(ctx, types.NamespacedName{Name: "retry", Namespace: ns}, &afterFailure)).To(Succeed())
		Expect(afterFailure.Status.Phase).NotTo(Equal(nomadv1alpha1.PhaseReady))
		Expect(meta_IsStatusConditionFalse(afterFailure.Status.Conditions, nomadv1alpha1.CondACLBootstrapped)).To(BeTrue())

		var tokenSec corev1.Secret
		Expect(k8s.Get(ctx, types.NamespacedName{Name: names(nc).TokenSecret, Namespace: ns}, &tokenSec)).To(Succeed())
		Expect(tokenSec.Annotations["nomad.operator.io/acl-bootstrapped"]).To(BeEmpty())
		tokenBeforeRetry := tokenSec.Data["token"]

		// Clear the transient error and reconcile again: the reconciler must
		// RE-ATTEMPT ACLBootstrap (not skip it because the Secret exists), using
		// the SAME token it already wrote (crash-and-retry re-submits the same
		// token, design §3.3).
		fake.bootstrapErr = nil
		reconcileOnce(r, "retry", ns)
		Expect(fake.bootstrapCalls).To(Equal(2)) // re-called, not skipped

		var afterRetry nomadv1alpha1.NomadCluster
		Expect(k8s.Get(ctx, types.NamespacedName{Name: "retry", Namespace: ns}, &afterRetry)).To(Succeed())
		Expect(afterRetry.Status.Phase).To(Equal(nomadv1alpha1.PhaseReady))
		Expect(meta_IsStatusConditionTrue(afterRetry.Status.Conditions, nomadv1alpha1.CondACLBootstrapped)).To(BeTrue())

		Expect(k8s.Get(ctx, types.NamespacedName{Name: names(nc).TokenSecret, Namespace: ns}, &tokenSec)).To(Succeed())
		Expect(tokenSec.Annotations["nomad.operator.io/acl-bootstrapped"]).To(Equal("true"))
		Expect(tokenSec.Data["token"]).To(Equal(tokenBeforeRetry)) // same token re-submitted

		// Steady state: once confirmed, a further reconcile must NOT re-bootstrap
		// (the existing Secret-gated idempotency guarantee still holds).
		reconcileOnce(r, "retry", ns)
		Expect(fake.bootstrapCalls).To(Equal(2))
	})

	It("populates status.members and a real status.quorum from ServerHealth once Ready", func() {
		ctx := context.Background()
		ns := "mgd-members"
		Expect(k8s.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		makeCertSecret(ctx, ns)
		nc := minimalCluster("members", ns)
		Expect(k8s.Create(ctx, nc)).To(Succeed())

		fake := &fakeNomad{
			leader:        "10.0.0.5:14647",
			serverHealthy: true,
			members: []nomad.NomadMember{
				{Name: "members-server-0", Addr: "10.0.0.5:14647", Status: "alive", Leader: true, Voter: true},
				{Name: "members-server-1", Addr: "10.0.0.6:24647", Status: "alive", Leader: false, Voter: true},
				{Name: "members-server-2", Addr: "10.0.0.7:34647", Status: "failed", Leader: false, Voter: false},
			},
		}
		r := &NomadClusterReconciler{Client: k8s, Scheme: k8s.Scheme(), NewNomadClient: newFakeFactory(fake)}

		reconcileOnce(r, "members", ns)
		var gw gwapiv1.Gateway
		Expect(k8s.Get(ctx, types.NamespacedName{Name: names(nc).Gateway, Namespace: ns}, &gw)).To(Succeed())
		gw.Status.Addresses = []gwapiv1.GatewayStatusAddress{{Value: "10.0.0.5"}}
		Expect(k8s.Status().Update(ctx, &gw)).To(Succeed())
		reconcileOnce(r, "members", ns)

		var got nomadv1alpha1.NomadCluster
		Expect(k8s.Get(ctx, types.NamespacedName{Name: "members", Namespace: ns}, &got)).To(Succeed())
		Expect(got.Status.Phase).To(Equal(nomadv1alpha1.PhaseReady))
		Expect(got.Status.Quorum).To(Equal("2/3")) // 2 voters of 3 members
		Expect(got.Status.Members).To(HaveLen(3))
		Expect(got.Status.Members[0]).To(Equal(nomadv1alpha1.MemberStatus{
			Name: "members-server-0", Addr: "10.0.0.5:14647", Status: "alive", Leader: true, Voter: true,
		}))
		Expect(got.Status.Members[2].Status).To(Equal("failed"))
		Expect(got.Status.Members[2].Voter).To(BeFalse())
	})

	It("does not flip Ready->Degraded when ServerHealth errors", func() {
		ctx := context.Background()
		ns := "mgd-members-err"
		Expect(k8s.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		makeCertSecret(ctx, ns)
		nc := minimalCluster("memberr", ns)
		Expect(k8s.Create(ctx, nc)).To(Succeed())

		fake := &fakeNomad{leader: "10.0.0.5:14647", serverHealthy: true, memberErr: errors.New("transient: health read")}
		r := &NomadClusterReconciler{Client: k8s, Scheme: k8s.Scheme(), NewNomadClient: newFakeFactory(fake)}

		reconcileOnce(r, "memberr", ns)
		var gw gwapiv1.Gateway
		Expect(k8s.Get(ctx, types.NamespacedName{Name: names(nc).Gateway, Namespace: ns}, &gw)).To(Succeed())
		gw.Status.Addresses = []gwapiv1.GatewayStatusAddress{{Value: "10.0.0.5"}}
		Expect(k8s.Status().Update(ctx, &gw)).To(Succeed())
		reconcileOnce(r, "memberr", ns)

		var got nomadv1alpha1.NomadCluster
		Expect(k8s.Get(ctx, types.NamespacedName{Name: "memberr", Namespace: ns}, &got)).To(Succeed())
		Expect(got.Status.Phase).To(Equal(nomadv1alpha1.PhaseReady)) // health-read error must NOT degrade
	})
})

var _ = Describe("advertise.rpc drift guard", func() {
	It("does not fire on a stable address (no drift)", func() {
		ctx := context.Background()
		r, rec := driveToReady(ctx, "stable", "drift-stable", "10.0.0.5", 3, []int32{14647, 24647, 34647})
		got := driftTo(ctx, "stable", "drift-stable", "10.0.0.5", r) // same address
		Expect(meta_IsStatusConditionFalse(got.Status.Conditions, nomadv1alpha1.CondRaftAddressDrift)).To(BeTrue())
		Consistently(rec.Events).ShouldNot(Receive())
	})

	It("raises a Warning + True condition on servers:1 drift while Ready", func() {
		ctx := context.Background()
		r, rec := driveToReady(ctx, "single", "drift-single", "10.0.0.5", 1, []int32{14647})
		got := driftTo(ctx, "single", "drift-single", "10.0.0.9", r)
		Expect(meta_IsStatusConditionTrue(got.Status.Conditions, nomadv1alpha1.CondRaftAddressDrift)).To(BeTrue())
		var ev string
		Eventually(rec.Events).Should(Receive(&ev))
		Expect(ev).To(ContainSubstring("Warning"))
		Expect(ev).To(ContainSubstring("wedge"))
	})

	It("raises a Normal (self-heal) event + True condition on HA drift", func() {
		ctx := context.Background()
		r, rec := driveToReady(ctx, "hadrift", "drift-ha", "10.0.0.5", 3, []int32{14647, 24647, 34647})
		got := driftTo(ctx, "hadrift", "drift-ha", "10.0.0.9", r)
		Expect(meta_IsStatusConditionTrue(got.Status.Conditions, nomadv1alpha1.CondRaftAddressDrift)).To(BeTrue())
		var ev string
		Eventually(rec.Events).Should(Receive(&ev))
		Expect(ev).To(ContainSubstring("Normal"))
		Expect(ev).To(ContainSubstring("self-heal"))
	})
})

var _ = Describe("drift Warning does not re-fire during an apply-error window (6b Minor 2)", func() {
	It("emits the servers:1 drift Warning only once across a persistent apply error", func() {
		ctx := context.Background()
		r, rec := driveToReady(ctx, "efire", "e-fire", "10.0.0.5", 1, []int32{14647})

		// Wrap the reconciler's client so every ConfigMap apply fails — simulating
		// a persistent apply-error window concurrent with a drift.
		r.Client = &configMapApplyFails{Client: r.Client}

		drift := func() {
			var gw gwapiv1.Gateway
			Expect(k8s.Get(ctx, types.NamespacedName{Name: "efire-gateway", Namespace: "e-fire"}, &gw)).To(Succeed())
			gw.Status.Addresses = []gwapiv1.GatewayStatusAddress{{Value: "10.0.0.9"}}
			Expect(k8s.Status().Update(ctx, &gw)).To(Succeed())
			_, _ = r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "efire", Namespace: "e-fire"}})
		}
		drift() // reconcile 1: address 10.0.0.5 -> 10.0.0.9, apply fails after persist
		drift() // reconcile 2: address already 10.0.0.9 persisted -> no new drift

		// Exactly one Warning across both reconciles.
		warnings := 0
		for {
			select {
			case ev := <-rec.Events:
				if strings.Contains(ev, "Warning") {
					warnings++
				}
				continue
			default:
			}
			break
		}
		Expect(warnings).To(Equal(1), "drift Warning must fire once, not per-reconcile, during an apply-error window")
	})
})

// configMapApplyFails fails every write to a ConfigMap; all other calls pass through.
type configMapApplyFails struct {
	client.Client
}

func (c *configMapApplyFails) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
	if _, ok := obj.(*corev1.ConfigMap); ok {
		return errors.New("simulated ConfigMap apply failure")
	}
	return c.Client.Patch(ctx, obj, patch, opts...)
}

var _ = Describe("transient-read flap guard (#5)", func() {
	It("keeps a Ready cluster Ready when the gateway address momentarily disappears", func() {
		ctx := context.Background()
		r, _ := driveToReady(ctx, "flapgw", "flap-gw", "10.0.0.5", 3, []int32{14647, 24647, 34647})

		// Transient blip: gateway loses its address.
		var gw gwapiv1.Gateway
		Expect(k8s.Get(ctx, types.NamespacedName{Name: "flapgw-gateway", Namespace: "flap-gw"}, &gw)).To(Succeed())
		gw.Status.Addresses = nil
		Expect(k8s.Status().Update(ctx, &gw)).To(Succeed())
		reconcileOnce(r, "flapgw", "flap-gw")

		var got nomadv1alpha1.NomadCluster
		Expect(k8s.Get(ctx, types.NamespacedName{Name: "flapgw", Namespace: "flap-gw"}, &got)).To(Succeed())
		Expect(got.Status.Phase).To(Equal(nomadv1alpha1.PhaseReady), "must not demote to Pending on a transient gateway blip")
		Expect(meta_IsStatusConditionTrue(got.Status.Conditions, nomadv1alpha1.CondReady)).To(BeTrue())
	})

	It("keeps a Ready cluster Ready when the cert Secret momentarily becomes incomplete", func() {
		ctx := context.Background()
		r, _ := driveToReady(ctx, "flapcert", "flap-cert", "10.0.0.5", 3, []int32{14647, 24647, 34647})

		// Transient blip: cert Secret loses ca.crt -> certSecretReady == false.
		// ca.crt (not tls.crt) is used here: the apiserver enforces built-in
		// validation on kubernetes.io/tls Secrets requiring non-empty tls.crt/
		// tls.key, so deleting tls.crt is rejected with a 422 before this test
		// ever reaches the reconciler under test. ca.crt has no such apiserver
		// constraint and still trips certSecretReady's identical missing-key
		// FALSE path (security.go:72-76).
		var s corev1.Secret
		Expect(k8s.Get(ctx, types.NamespacedName{Name: "nomad-tls", Namespace: "flap-cert"}, &s)).To(Succeed())
		delete(s.Data, "ca.crt")
		Expect(k8s.Update(ctx, &s)).To(Succeed())
		reconcileOnce(r, "flapcert", "flap-cert")

		var got nomadv1alpha1.NomadCluster
		Expect(k8s.Get(ctx, types.NamespacedName{Name: "flapcert", Namespace: "flap-cert"}, &got)).To(Succeed())
		Expect(got.Status.Phase).To(Equal(nomadv1alpha1.PhaseReady), "must not demote to Pending on a transient cert blip")
	})

	It("still gates an unprovisioned cluster to Pending on a missing address", func() {
		ctx := context.Background()
		ns := "flap-new"
		Expect(k8s.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		makeCertSecret(ctx, ns)
		nc := minimalCluster("fresh", ns)
		Expect(k8s.Create(ctx, nc)).To(Succeed())
		fake := &fakeNomad{leader: "10.0.0.5:14647", serverHealthy: true}
		r := &NomadClusterReconciler{Client: k8s, Scheme: k8s.Scheme(), NewNomadClient: newFakeFactory(fake)}
		reconcileOnce(r, "fresh", ns) // gateway has no address yet
		var got nomadv1alpha1.NomadCluster
		Expect(k8s.Get(ctx, types.NamespacedName{Name: "fresh", Namespace: ns}, &got)).To(Succeed())
		Expect(got.Status.Phase).To(Equal(nomadv1alpha1.PhasePending))
	})
})

func reconcileOnce(r *NomadClusterReconciler, name, ns string) {
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: ns}})
	Expect(err).NotTo(HaveOccurred())
}

// driveToReady runs the two-reconcile Managed path to Ready at address A,
// returning the reconciler (with a fake recorder) for a follow-up drift.
func driveToReady(ctx context.Context, name, ns, addrA string, servers int32, rpcPorts []int32) (*NomadClusterReconciler, *record.FakeRecorder) {
	Expect(k8s.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
	makeCertSecret(ctx, ns)
	nc := minimalCluster(name, ns)
	nc.Spec.Servers = servers
	nc.Spec.ExternalAccess.Gateway.RPCPorts = rpcPorts
	Expect(k8s.Create(ctx, nc)).To(Succeed())

	rec := record.NewFakeRecorder(10)
	fake := &fakeNomad{leader: "10.0.0.5:14647", serverHealthy: true}
	r := &NomadClusterReconciler{Client: k8s, Scheme: k8s.Scheme(), NewNomadClient: newFakeFactory(fake), Recorder: rec}

	reconcileOnce(r, name, ns)
	var gw gwapiv1.Gateway
	Expect(k8s.Get(ctx, types.NamespacedName{Name: names(nc).Gateway, Namespace: ns}, &gw)).To(Succeed())
	gw.Status.Addresses = []gwapiv1.GatewayStatusAddress{{Value: addrA}}
	Expect(k8s.Status().Update(ctx, &gw)).To(Succeed())
	reconcileOnce(r, name, ns)

	var got nomadv1alpha1.NomadCluster
	Expect(k8s.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, &got)).To(Succeed())
	Expect(got.Status.Phase).To(Equal(nomadv1alpha1.PhaseReady))
	return r, rec
}

func driftTo(ctx context.Context, name, ns, addrB string, r *NomadClusterReconciler) nomadv1alpha1.NomadCluster {
	var gw gwapiv1.Gateway
	// names(nc).Gateway == nc.Name + "-gateway" (internal/controller/names.go:37).
	Expect(k8s.Get(ctx, types.NamespacedName{Name: name + "-gateway", Namespace: ns}, &gw)).To(Succeed())
	gw.Status.Addresses = []gwapiv1.GatewayStatusAddress{{Value: addrB}}
	Expect(k8s.Status().Update(ctx, &gw)).To(Succeed())
	reconcileOnce(r, name, ns)
	var got nomadv1alpha1.NomadCluster
	Expect(k8s.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, &got)).To(Succeed())
	return got
}
