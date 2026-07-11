package controller

import "testing"

func TestNamesDeterministic(t *testing.T) {
	nc := minimalCluster("prod", "nomad-system") // defined in the shared _test.go; same package
	n := names(nc)
	if n.StatefulSet != "prod-server" {
		t.Errorf("StatefulSet = %q", n.StatefulSet)
	}
	if n.HeadlessSvc != "prod-server-headless" {
		t.Errorf("HeadlessSvc = %q", n.HeadlessSvc)
	}
	if n.PodSvc(1) != "prod-server-1-rpc" {
		t.Errorf("PodSvc(1) = %q", n.PodSvc(1))
	}
	if n.PodName(2) != "prod-server-2" {
		t.Errorf("PodName(2) = %q", n.PodName(2))
	}
	if n.TokenSecret != "prod-nomad-bootstrap-token" {
		t.Errorf("TokenSecret = %q", n.TokenSecret)
	}
	if n.Labels()["app.kubernetes.io/instance"] != "prod" {
		t.Errorf("labels missing instance")
	}
	if n.TLSRoute != "prod-nomad-tls" {
		t.Errorf("TLSRoute = %q, want %q", n.TLSRoute, "prod-nomad-tls")
	}
	if n.TCPRoute(0) != "prod-rpc-0" {
		t.Errorf("TCPRoute(0) = %q, want %q", n.TCPRoute(0), "prod-rpc-0")
	}
	if n.TCPRoute(2) != "prod-rpc-2" {
		t.Errorf("TCPRoute(2) = %q, want %q", n.TCPRoute(2), "prod-rpc-2")
	}
}
