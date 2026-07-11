package controller

import "testing"

func TestStatefulSetBootstrapKnobs(t *testing.T) {
	nc := minimalCluster("prod", "nomad-system")
	ss := buildStatefulSet(nc, "abc123")
	if ss.Spec.PodManagementPolicy != "Parallel" {
		t.Errorf("podManagementPolicy = %q, want Parallel (bootstrap deadlock)", ss.Spec.PodManagementPolicy)
	}
	if *ss.Spec.Replicas != 3 {
		t.Errorf("replicas = %d", *ss.Spec.Replicas)
	}
	if ss.Spec.Template.Annotations["nomad.operator.io/config-hash"] != "abc123" {
		t.Error("config-hash annotation missing (ConfigMap changes must roll)")
	}
	if ss.Spec.Template.Spec.Affinity == nil || ss.Spec.Template.Spec.Affinity.PodAntiAffinity == nil {
		t.Error("required pod anti-affinity missing")
	}
	if len(ss.Spec.Template.Spec.InitContainers) == 0 {
		t.Error("init container (per-pod advertise) missing")
	}
	if len(ss.Spec.VolumeClaimTemplates) != 1 {
		t.Error("Raft PVC template missing")
	}
}

func TestServicesPublishNotReady(t *testing.T) {
	nc := minimalCluster("prod", "nomad-system")
	if !buildHeadlessService(nc).Spec.PublishNotReadyAddresses {
		t.Error("headless must publishNotReadyAddresses (Serf join pre-quorum)")
	}
	if !buildPodService(nc, 0).Spec.PublishNotReadyAddresses {
		t.Error("per-pod svc must publishNotReadyAddresses (TCPRoute backend pre-quorum)")
	}
	// per-pod service selects exactly one pod
	if buildPodService(nc, 1).Spec.Selector["statefulset.kubernetes.io/pod-name"] != "prod-server-1" {
		t.Error("per-pod selector wrong")
	}
}

func TestPDBMinAvailable(t *testing.T) {
	nc := minimalCluster("prod", "nomad-system")
	pdb := buildPDB(nc)
	if pdb.Spec.MinAvailable.IntValue() != 2 {
		t.Errorf("minAvailable = %d, want servers-1 = 2", pdb.Spec.MinAvailable.IntValue())
	}
}

func TestBuildConfigMapContents(t *testing.T) {
	nc := minimalCluster("prod", "nomad-system")
	cm := buildConfigMap(nc, "10.0.0.5")
	if cm.Name != "prod-server-config" {
		t.Errorf("ConfigMap name = %q, want prod-server-config", cm.Name)
	}
	if cm.Data["gateway_address"] != "10.0.0.5" {
		t.Errorf("gateway_address = %q", cm.Data["gateway_address"])
	}
	if cm.Data["rpc_ports"] != "14647 24647 34647" {
		t.Errorf("rpc_ports = %q, want space-separated ports with no leading space", cm.Data["rpc_ports"])
	}
	if cm.Data["server.hcl"] == "" {
		t.Error("server.hcl body missing")
	}
	if cm.Data["entrypoint.sh"] == "" {
		t.Error("entrypoint.sh (init container script) missing")
	}
}

func TestBuildAPIServiceSelectsServerPods(t *testing.T) {
	nc := minimalCluster("prod", "nomad-system")
	svc := buildAPIService(nc)
	if svc.Name != "prod-nomad" {
		t.Errorf("APIService name = %q, want prod-nomad", svc.Name)
	}
	if svc.Spec.Selector["app.kubernetes.io/instance"] != "prod" {
		t.Error("APIService selector must select this cluster's server pods")
	}
	if len(svc.Spec.Ports) != 1 || svc.Spec.Ports[0].Port != portHTTP {
		t.Errorf("APIService port = %+v, want single port %d", svc.Spec.Ports, portHTTP)
	}
}
