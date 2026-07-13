package controller

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	gwapiv1 "sigs.k8s.io/gateway-api/apis/v1"

	nomadv1alpha1 "github.com/jacaudi/nomad-operator/api/v1alpha1"
)

// existingModeCluster returns a minimal Existing-mode NomadCluster fixture
// referencing the given Gateway by name/namespace.
func existingModeCluster(name, ns, gwName, gwNS string) *nomadv1alpha1.NomadCluster {
	nc := minimalCluster(name, ns)
	nc.Spec.ExternalAccess.Gateway.Mode = nomadv1alpha1.GatewayModeExisting
	nc.Spec.ExternalAccess.Gateway.ClassName = ""
	nc.Spec.ExternalAccess.Gateway.Ref = &nomadv1alpha1.GatewayRef{Name: gwName, Namespace: gwNS}
	return nc
}

func TestGatewayToClusters(t *testing.T) {
	s := scheme.Scheme
	if err := nomadv1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}
	if err := gwapiv1.Install(s); err != nil {
		t.Fatalf("gwapiv1.Install: %v", err)
	}

	referencer := existingModeCluster("referencer", "default", "shared-gw", "default")
	otherGateway := existingModeCluster("other-gateway", "default", "different-gw", "default")
	managed := minimalCluster("managed", "default") // Managed mode, no ref
	lb := lbCluster("lb-edge", "default")           // LoadBalancer mode: ExternalAccess.Gateway == nil

	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(referencer, otherGateway, managed, lb).
		Build()
	r := &NomadClusterReconciler{Client: c, Scheme: s}

	g := &gwapiv1.Gateway{ObjectMeta: metav1.ObjectMeta{Name: "shared-gw", Namespace: "default"}}
	reqs := r.gatewayToClusters(t.Context(), g)
	if len(reqs) != 1 {
		t.Fatalf("gatewayToClusters(shared-gw) returned %d requests, want 1: %+v", len(reqs), reqs)
	}
	if reqs[0].Name != "referencer" || reqs[0].Namespace != "default" {
		t.Errorf("gatewayToClusters(shared-gw) = %+v, want {referencer default}", reqs[0])
	}

	unreferenced := &gwapiv1.Gateway{ObjectMeta: metav1.ObjectMeta{Name: "nobody-refs-this", Namespace: "default"}}
	if got := r.gatewayToClusters(t.Context(), unreferenced); len(got) != 0 {
		t.Errorf("gatewayToClusters(nobody-refs-this) returned %d requests, want 0: %+v", len(got), got)
	}
}
