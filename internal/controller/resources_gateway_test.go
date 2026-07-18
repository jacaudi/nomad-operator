package controller

import (
	"context"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	gwapiv1 "sigs.k8s.io/gateway-api/apis/v1"
	gwapiv1a2 "sigs.k8s.io/gateway-api/apis/v1alpha2"

	nomadv1alpha1 "github.com/jacaudi/nomad-operator/api/v1alpha1"
)

func TestManagedGatewayListeners(t *testing.T) {
	nc := minimalCluster("prod", "nomad-system")
	gw := buildManagedGateway(nc)
	if string(gw.Spec.GatewayClassName) != "cilium" {
		t.Errorf("gatewayClassName = %q", gw.Spec.GatewayClassName)
	}
	// 1 HTTP + 3 TCP listeners
	if len(gw.Spec.Listeners) != 4 {
		t.Fatalf("listeners = %d, want 4", len(gw.Spec.Listeners))
	}

	httpListener := gw.Spec.Listeners[0]
	if httpListener.TLS == nil || httpListener.TLS.Mode == nil {
		t.Fatalf("http listener TLS mode not set")
	}
	if *httpListener.TLS.Mode != gwapiv1.TLSModePassthrough {
		t.Errorf("http listener TLS mode = %q, want %q", *httpListener.TLS.Mode, gwapiv1.TLSModePassthrough)
	}

	rpcListeners := gw.Spec.Listeners[1:]
	seenPorts := make(map[gwapiv1.PortNumber]bool, len(rpcListeners))
	for ordinal, l := range rpcListeners {
		wantPort := gwapiv1.PortNumber(nc.Spec.ExternalAccess.Gateway.RPCPorts[ordinal])
		if l.Port != wantPort {
			t.Errorf("rpc listener[%d] port = %d, want %d", ordinal, l.Port, wantPort)
		}
		if seenPorts[l.Port] {
			t.Errorf("rpc listener[%d] port %d is a duplicate", ordinal, l.Port)
		}
		seenPorts[l.Port] = true
	}
}

func TestTCPRoutesOnePerServer(t *testing.T) {
	nc := minimalCluster("prod", "nomad-system")
	routes := buildTCPRoutes(nc)
	if len(routes) != 3 {
		t.Fatalf("tcp routes = %d, want 3", len(routes))
	}

	seenBackends := make(map[string]bool, len(routes))
	for ordinal, route := range routes {
		gotSection := route.Spec.ParentRefs[0].SectionName
		wantSection := gwapiv1.SectionName(listenerNameRPC(ordinal))
		if gotSection == nil || *gotSection != wantSection {
			t.Errorf("route[%d] parentRef sectionName = %v, want %q", ordinal, gotSection, wantSection)
		}

		be := route.Spec.Rules[0].BackendRefs[0]
		wantBackend := names(nc).PodSvc(ordinal)
		if string(be.Name) != wantBackend {
			t.Errorf("route[%d] backend name = %q, want %q", ordinal, be.Name, wantBackend)
		}
		if seenBackends[string(be.Name)] {
			t.Errorf("route[%d] backend name %q is a duplicate", ordinal, be.Name)
		}
		seenBackends[string(be.Name)] = true

		wantPort := gwapiv1.PortNumber(portRPC)
		if be.Port == nil || *be.Port != wantPort {
			t.Errorf("route[%d] backend port = %v, want %d", ordinal, be.Port, wantPort)
		}
	}
}

// TestSingleServerOneTCPRoute guards FR-1: a servers=1 control plane uses
// exactly one rpcPort and therefore exactly one per-server TCPRoute.
func TestSingleServerOneTCPRoute(t *testing.T) {
	nc := singleServerCluster("prod", "nomad-system")
	routes := buildTCPRoutes(nc)
	if len(routes) != 1 {
		t.Fatalf("tcp routes = %d, want 1", len(routes))
	}
}

// sharedGatewayFixture builds a user-owned Gateway named "shared-gw" with an
// HTTP listener (hostname "nomad.example.com", matching minimalCluster's
// default) plus one TCP listener per given port, admitting routes from all
// namespaces. Used to simulate a pre-existing Gateway the operator does not
// own (Existing mode).
func sharedGatewayFixture(ns string, rpcPorts []int32) *gwapiv1.Gateway {
	admitAll := &gwapiv1.AllowedRoutes{Namespaces: &gwapiv1.RouteNamespaces{From: new(gwapiv1.NamespacesFromAll)}}
	listeners := make([]gwapiv1.Listener, 0, 1+len(rpcPorts))
	listeners = append(listeners, gwapiv1.Listener{
		Name:          listenerNameHTTP,
		Port:          gwapiv1.PortNumber(portHTTP),
		Protocol:      gwapiv1.TLSProtocolType,
		Hostname:      ptrHostname("nomad.example.com"),
		TLS:           &gwapiv1.GatewayTLSConfig{Mode: new(gwapiv1.TLSModePassthrough)},
		AllowedRoutes: admitAll,
	})
	for ordinal, p := range rpcPorts {
		listeners = append(listeners, gwapiv1.Listener{
			Name:          gwapiv1.SectionName(listenerNameRPC(ordinal)),
			Port:          gwapiv1.PortNumber(p),
			Protocol:      gwapiv1.TCPProtocolType,
			AllowedRoutes: admitAll,
		})
	}
	return &gwapiv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "shared-gw", Namespace: ns},
		Spec:       gwapiv1.GatewaySpec{GatewayClassName: "cilium", Listeners: listeners},
	}
}

var _ = Describe("Existing gateway mode", func() {
	It("attaches routes to a pre-existing gateway and reads its address", func() {
		ctx := context.Background()
		ns := "exist"
		Expect(k8s.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		makeCertSecret(ctx, ns)
		// Pre-create a shared Gateway with the required listeners + an address.
		shared := sharedGatewayFixture(ns, []int32{14647, 24647, 34647})
		Expect(k8s.Create(ctx, shared)).To(Succeed())
		shared.Status.Addresses = []gwapiv1.GatewayStatusAddress{{Value: "10.0.0.9"}}
		Expect(k8s.Status().Update(ctx, shared)).To(Succeed())

		nc := minimalCluster("prod", ns)
		nc.Spec.ExternalAccess.Gateway.Mode = nomadv1alpha1.GatewayModeExisting
		nc.Spec.ExternalAccess.Gateway.ClassName = ""
		nc.Spec.ExternalAccess.Gateway.Ref = &nomadv1alpha1.GatewayRef{Name: "shared-gw", Namespace: ns}
		Expect(k8s.Create(ctx, nc)).To(Succeed())

		fake := &fakeNomad{leader: "10.0.0.9:14647", serverHealthy: true}
		r := &NomadClusterReconciler{Client: k8s, Scheme: k8s.Scheme(), NewNomadClient: newFakeFactory(fake)}
		reconcileOnce(r, "prod", ns)

		// operator must NOT create its own Gateway in Existing mode
		var own gwapiv1.Gateway
		err := k8s.Get(ctx, types.NamespacedName{Name: names(nc).Gateway, Namespace: ns}, &own)
		Expect(apierrors.IsNotFound(err)).To(BeTrue())
		// routes exist, parented to shared-gw
		var tcp gwapiv1a2.TCPRoute
		Expect(k8s.Get(ctx, types.NamespacedName{Name: "prod-rpc-0", Namespace: ns}, &tcp)).To(Succeed())
		Expect(string(tcp.Spec.ParentRefs[0].Name)).To(Equal("shared-gw"))
	})

	It("sets ExternalAccessReady=False when a required listener is missing", func() {
		ctx := context.Background()
		ns := "existbad"
		Expect(k8s.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		makeCertSecret(ctx, ns)
		shared := sharedGatewayFixture(ns, []int32{14647}) // missing 24647, 34647
		Expect(k8s.Create(ctx, shared)).To(Succeed())
		shared.Status.Addresses = []gwapiv1.GatewayStatusAddress{{Value: "10.0.0.9"}}
		Expect(k8s.Status().Update(ctx, shared)).To(Succeed())
		nc := minimalCluster("prod", ns)
		nc.Spec.ExternalAccess = nomadv1alpha1.ExternalAccessSpec{Mode: nomadv1alpha1.ExternalAccessGateway, Gateway: &nomadv1alpha1.GatewaySpec{Mode: nomadv1alpha1.GatewayModeExisting, Ref: &nomadv1alpha1.GatewayRef{Name: "shared-gw", Namespace: ns}, RPCPorts: []int32{14647, 24647, 34647}, HTTPHostname: "nomad.example.com"}}
		Expect(k8s.Create(ctx, nc)).To(Succeed())
		fake := &fakeNomad{}
		r := &NomadClusterReconciler{Client: k8s, Scheme: k8s.Scheme(), NewNomadClient: newFakeFactory(fake)}
		reconcileOnce(r, "prod", ns)
		var got nomadv1alpha1.NomadCluster
		Expect(k8s.Get(ctx, types.NamespacedName{Name: "prod", Namespace: ns}, &got)).To(Succeed())
		Expect(meta_IsStatusConditionTrue(got.Status.Conditions, nomadv1alpha1.CondExternalAccessReady)).To(BeFalse())
	})

	It("sets ExternalAccessReady=False when a listener has the right port but the wrong name", func() {
		ctx := context.Background()
		ns := "existwrongname"
		Expect(k8s.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		makeCertSecret(ctx, ns)
		// Right ports/protocols throughout, but the ordinal-0 RPC listener is
		// named "custom-rpc-0" instead of the operator's "rpc-0" convention
		// that buildTCPRoutes' parentRef.SectionName actually attaches to.
		shared := sharedGatewayFixture(ns, []int32{14647, 24647, 34647})
		shared.Spec.Listeners[1].Name = "custom-rpc-0"
		Expect(k8s.Create(ctx, shared)).To(Succeed())
		shared.Status.Addresses = []gwapiv1.GatewayStatusAddress{{Value: "10.0.0.9"}}
		Expect(k8s.Status().Update(ctx, shared)).To(Succeed())

		nc := minimalCluster("prod", ns)
		nc.Spec.ExternalAccess = nomadv1alpha1.ExternalAccessSpec{Mode: nomadv1alpha1.ExternalAccessGateway, Gateway: &nomadv1alpha1.GatewaySpec{Mode: nomadv1alpha1.GatewayModeExisting, Ref: &nomadv1alpha1.GatewayRef{Name: "shared-gw", Namespace: ns}, RPCPorts: []int32{14647, 24647, 34647}, HTTPHostname: "nomad.example.com"}}
		Expect(k8s.Create(ctx, nc)).To(Succeed())
		fake := &fakeNomad{}
		r := &NomadClusterReconciler{Client: k8s, Scheme: k8s.Scheme(), NewNomadClient: newFakeFactory(fake)}
		reconcileOnce(r, "prod", ns)
		var got nomadv1alpha1.NomadCluster
		Expect(k8s.Get(ctx, types.NamespacedName{Name: "prod", Namespace: ns}, &got)).To(Succeed())
		Expect(meta_IsStatusConditionTrue(got.Status.Conditions, nomadv1alpha1.CondExternalAccessReady)).To(BeFalse())
	})

	// Not part of the task-9 brief's Step 1 snippet, but required by the task's
	// design facts ("admits the CR's namespace via allowedRoutes") and its
	// report checklist. A Gateway that has every required listener but does
	// not admit routes from the CR's namespace must still be treated as not
	// ready — otherwise a shared Gateway silently accepts routes it never
	// opted into.
	It("sets ExternalAccessReady=False when the Gateway does not admit the CR's namespace", func() {
		ctx := context.Background()
		gwNs := "shared-gw-ns"
		crNs := "tenant-a"
		Expect(k8s.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: gwNs}})).To(Succeed())
		Expect(k8s.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: crNs}})).To(Succeed())
		makeCertSecret(ctx, crNs)

		// Same fixture shape as sharedGatewayFixture, but AllowedRoutes left at
		// the Core default (From: Same) instead of admitting all namespaces —
		// so a CR living in a different namespace than the Gateway is refused.
		shared := sharedGatewayFixture(gwNs, []int32{14647, 24647, 34647})
		sameOnly := &gwapiv1.RouteNamespaces{From: new(gwapiv1.NamespacesFromSame)}
		for i := range shared.Spec.Listeners {
			shared.Spec.Listeners[i].AllowedRoutes = &gwapiv1.AllowedRoutes{Namespaces: sameOnly}
		}
		Expect(k8s.Create(ctx, shared)).To(Succeed())
		shared.Status.Addresses = []gwapiv1.GatewayStatusAddress{{Value: "10.0.0.9"}}
		Expect(k8s.Status().Update(ctx, shared)).To(Succeed())

		nc := minimalCluster("prod", crNs)
		nc.Spec.ExternalAccess = nomadv1alpha1.ExternalAccessSpec{Mode: nomadv1alpha1.ExternalAccessGateway, Gateway: &nomadv1alpha1.GatewaySpec{Mode: nomadv1alpha1.GatewayModeExisting, Ref: &nomadv1alpha1.GatewayRef{Name: "shared-gw", Namespace: gwNs}, RPCPorts: []int32{14647, 24647, 34647}, HTTPHostname: "nomad.example.com"}}
		Expect(k8s.Create(ctx, nc)).To(Succeed())
		fake := &fakeNomad{}
		r := &NomadClusterReconciler{Client: k8s, Scheme: k8s.Scheme(), NewNomadClient: newFakeFactory(fake)}
		reconcileOnce(r, "prod", crNs)
		var got nomadv1alpha1.NomadCluster
		Expect(k8s.Get(ctx, types.NamespacedName{Name: "prod", Namespace: crNs}, &got)).To(Succeed())
		Expect(meta_IsStatusConditionTrue(got.Status.Conditions, nomadv1alpha1.CondExternalAccessReady)).To(BeFalse())
	})
})

var _ = Describe("Existing-mode gateway reason (#6)", func() {
	// helper: an Existing-mode cluster referencing a Gateway named "shared" in ns.
	existingCluster := func(name, ns string) *nomadv1alpha1.NomadCluster {
		nc := minimalCluster(name, ns)
		nc.Spec.ExternalAccess.Gateway.Mode = nomadv1alpha1.GatewayModeExisting
		nc.Spec.ExternalAccess.Gateway.Ref = &nomadv1alpha1.GatewayRef{Name: "shared", Namespace: ns}
		return nc
	}
	reasonFor := func(name, ns string) string {
		var got nomadv1alpha1.NomadCluster
		Expect(k8s.Get(context.Background(), types.NamespacedName{Name: name, Namespace: ns}, &got)).To(Succeed())
		for _, c := range got.Status.Conditions {
			if c.Type == nomadv1alpha1.CondExternalAccessReady {
				return c.Reason
			}
		}
		return ""
	}

	It("reports GatewayNotFound when the referenced Gateway is absent", func() {
		ctx := context.Background()
		ns := "ex-notfound"
		Expect(k8s.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		makeCertSecret(ctx, ns)
		Expect(k8s.Create(ctx, existingCluster("c", ns))).To(Succeed())
		fake := &fakeNomad{leader: "10.0.0.5:14647", serverHealthy: true}
		r := &NomadClusterReconciler{Client: k8s, Scheme: k8s.Scheme(), NewNomadClient: newFakeFactory(fake)}
		reconcileOnce(r, "c", ns)
		Expect(reasonFor("c", ns)).To(Equal("GatewayNotFound"))
	})

	It("reports GatewayNoAddress when the referenced Gateway is valid but has no address", func() {
		ctx := context.Background()
		ns := "ex-noaddr"
		Expect(k8s.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		makeCertSecret(ctx, ns)
		// A fully-valid referenced Gateway (HTTP TLS listener + one RPC TCP
		// listener per RPCPort), no address. existingCluster is built on
		// minimalCluster, which sets 3 RPC ports (14647, 24647, 34647) — the
		// brief's own snippet defines only the rpc-0 listener, which would
		// trip RPCListenerInvalid on the missing rpc-1/rpc-2 listeners before
		// ever reaching the no-address check. Adapted here to include all
		// three so the test genuinely exercises the GatewayNoAddress path.
		gw := &gwapiv1.Gateway{
			ObjectMeta: metav1.ObjectMeta{Name: "shared", Namespace: ns},
			Spec: gwapiv1.GatewaySpec{
				GatewayClassName: "cilium",
				Listeners: []gwapiv1.Listener{
					{Name: listenerNameHTTP, Port: gwapiv1.PortNumber(portHTTP), Protocol: gwapiv1.TLSProtocolType,
						Hostname: ptrHostname("nomad.example.com"), TLS: &gwapiv1.GatewayTLSConfig{Mode: new(gwapiv1.TLSModePassthrough)}},
					{Name: gwapiv1.SectionName(listenerNameRPC(0)), Port: 14647, Protocol: gwapiv1.TCPProtocolType},
					{Name: gwapiv1.SectionName(listenerNameRPC(1)), Port: 24647, Protocol: gwapiv1.TCPProtocolType},
					{Name: gwapiv1.SectionName(listenerNameRPC(2)), Port: 34647, Protocol: gwapiv1.TCPProtocolType},
				},
			},
		}
		Expect(k8s.Create(ctx, gw)).To(Succeed())
		Expect(k8s.Create(ctx, existingCluster("c", ns))).To(Succeed())
		fake := &fakeNomad{leader: "10.0.0.5:14647", serverHealthy: true}
		r := &NomadClusterReconciler{Client: k8s, Scheme: k8s.Scheme(), NewNomadClient: newFakeFactory(fake)}
		reconcileOnce(r, "c", ns)
		Expect(reasonFor("c", ns)).To(Equal("GatewayNoAddress"))
	})

	It("reports HTTPListenerInvalid when the HTTP listener has the wrong hostname", func() {
		ctx := context.Background()
		ns := "ex-httpinvalid"
		Expect(k8s.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		makeCertSecret(ctx, ns)
		// HTTP listener present but hostname doesn't match
		// spec.externalAccess.gateway.httpHostname ("nomad.example.com", set by
		// minimalCluster). RPC listeners are otherwise valid so the failure is
		// attributable to the HTTP listener alone.
		gw := &gwapiv1.Gateway{
			ObjectMeta: metav1.ObjectMeta{Name: "shared", Namespace: ns},
			Spec: gwapiv1.GatewaySpec{
				GatewayClassName: "cilium",
				Listeners: []gwapiv1.Listener{
					{Name: listenerNameHTTP, Port: gwapiv1.PortNumber(portHTTP), Protocol: gwapiv1.TLSProtocolType,
						Hostname: ptrHostname("wrong.example.com"), TLS: &gwapiv1.GatewayTLSConfig{Mode: new(gwapiv1.TLSModePassthrough)}},
					{Name: gwapiv1.SectionName(listenerNameRPC(0)), Port: 14647, Protocol: gwapiv1.TCPProtocolType},
					{Name: gwapiv1.SectionName(listenerNameRPC(1)), Port: 24647, Protocol: gwapiv1.TCPProtocolType},
					{Name: gwapiv1.SectionName(listenerNameRPC(2)), Port: 34647, Protocol: gwapiv1.TCPProtocolType},
				},
			},
		}
		Expect(k8s.Create(ctx, gw)).To(Succeed())
		Expect(k8s.Create(ctx, existingCluster("c", ns))).To(Succeed())
		fake := &fakeNomad{leader: "10.0.0.5:14647", serverHealthy: true}
		r := &NomadClusterReconciler{Client: k8s, Scheme: k8s.Scheme(), NewNomadClient: newFakeFactory(fake)}
		reconcileOnce(r, "c", ns)
		Expect(reasonFor("c", ns)).To(Equal("HTTPListenerInvalid"))
	})

	It("reports RPCListenerInvalid when an RPC listener has the wrong protocol", func() {
		ctx := context.Background()
		ns := "ex-rpcinvalid"
		Expect(k8s.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		makeCertSecret(ctx, ns)
		// HTTP listener valid; the ordinal-0 RPC listener is HTTP protocol instead of TCP.
		gw := &gwapiv1.Gateway{
			ObjectMeta: metav1.ObjectMeta{Name: "shared", Namespace: ns},
			Spec: gwapiv1.GatewaySpec{
				GatewayClassName: "cilium",
				Listeners: []gwapiv1.Listener{
					{Name: listenerNameHTTP, Port: gwapiv1.PortNumber(portHTTP), Protocol: gwapiv1.TLSProtocolType,
						Hostname: ptrHostname("nomad.example.com"), TLS: &gwapiv1.GatewayTLSConfig{Mode: new(gwapiv1.TLSModePassthrough)}},
					{Name: gwapiv1.SectionName(listenerNameRPC(0)), Port: 14647, Protocol: gwapiv1.HTTPProtocolType},
					{Name: gwapiv1.SectionName(listenerNameRPC(1)), Port: 24647, Protocol: gwapiv1.TCPProtocolType},
					{Name: gwapiv1.SectionName(listenerNameRPC(2)), Port: 34647, Protocol: gwapiv1.TCPProtocolType},
				},
			},
		}
		Expect(k8s.Create(ctx, gw)).To(Succeed())
		Expect(k8s.Create(ctx, existingCluster("c", ns))).To(Succeed())
		fake := &fakeNomad{leader: "10.0.0.5:14647", serverHealthy: true}
		r := &NomadClusterReconciler{Client: k8s, Scheme: k8s.Scheme(), NewNomadClient: newFakeFactory(fake)}
		reconcileOnce(r, "c", ns)
		Expect(reasonFor("c", ns)).To(Equal("RPCListenerInvalid"))
	})

	It("reports NamespaceNotAdmitted when the HTTP listener's allowedRoutes excludes the CR's namespace", func() {
		ctx := context.Background()
		ns := "ex-nsselect"
		Expect(k8s.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		makeCertSecret(ctx, ns)
		// Listeners are otherwise fully valid, but the HTTP listener restricts
		// admission to a namespace Selector — the Core support level this
		// operator treats as fail-closed (not admitted), regardless of the CR's
		// actual namespace or labels.
		excludeSelector := &gwapiv1.AllowedRoutes{Namespaces: &gwapiv1.RouteNamespaces{
			From:     new(gwapiv1.NamespacesFromSelector),
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"gateway-tenant": "none-match-this-namespace"}},
		}}
		gw := &gwapiv1.Gateway{
			ObjectMeta: metav1.ObjectMeta{Name: "shared", Namespace: ns},
			Spec: gwapiv1.GatewaySpec{
				GatewayClassName: "cilium",
				Listeners: []gwapiv1.Listener{
					{Name: listenerNameHTTP, Port: gwapiv1.PortNumber(portHTTP), Protocol: gwapiv1.TLSProtocolType,
						Hostname: ptrHostname("nomad.example.com"), TLS: &gwapiv1.GatewayTLSConfig{Mode: new(gwapiv1.TLSModePassthrough)},
						AllowedRoutes: excludeSelector},
					{Name: gwapiv1.SectionName(listenerNameRPC(0)), Port: 14647, Protocol: gwapiv1.TCPProtocolType},
					{Name: gwapiv1.SectionName(listenerNameRPC(1)), Port: 24647, Protocol: gwapiv1.TCPProtocolType},
					{Name: gwapiv1.SectionName(listenerNameRPC(2)), Port: 34647, Protocol: gwapiv1.TCPProtocolType},
				},
			},
		}
		Expect(k8s.Create(ctx, gw)).To(Succeed())
		Expect(k8s.Create(ctx, existingCluster("c", ns))).To(Succeed())
		fake := &fakeNomad{leader: "10.0.0.5:14647", serverHealthy: true}
		r := &NomadClusterReconciler{Client: k8s, Scheme: k8s.Scheme(), NewNomadClient: newFakeFactory(fake)}
		reconcileOnce(r, "c", ns)
		Expect(reasonFor("c", ns)).To(Equal("NamespaceNotAdmitted"))
	})

	It("still reports the generic WaitingForAddress reason for the Managed mode path", func() {
		ctx := context.Background()
		ns := "ex-generic-managed"
		Expect(k8s.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		makeCertSecret(ctx, ns)
		nc := minimalCluster("c", ns) // Managed mode, no Gateway controller in envtest -> no address
		Expect(k8s.Create(ctx, nc)).To(Succeed())
		fake := &fakeNomad{leader: "10.0.0.5:14647", serverHealthy: true}
		r := &NomadClusterReconciler{Client: k8s, Scheme: k8s.Scheme(), NewNomadClient: newFakeFactory(fake)}
		reconcileOnce(r, "c", ns)
		Expect(reasonFor("c", ns)).To(Equal("WaitingForAddress"))
	})
})
