package controller

import "testing"

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
}

func TestTCPRoutesOnePerServer(t *testing.T) {
	nc := minimalCluster("prod", "nomad-system")
	routes := buildTCPRoutes(nc)
	if len(routes) != 3 {
		t.Fatalf("tcp routes = %d, want 3", len(routes))
	}
	// route 1 backends the per-pod service for server-1
	be := routes[1].Spec.Rules[0].BackendRefs[0]
	if string(be.Name) != "prod-server-1-rpc" {
		t.Errorf("route[1] backend = %q", be.Name)
	}
}
