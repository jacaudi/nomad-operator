package controller

import (
	"testing"

	gwapiv1 "sigs.k8s.io/gateway-api/apis/v1"
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
		wantPort := gwapiv1.PortNumber(nc.Spec.Gateway.RPCPorts[ordinal])
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
