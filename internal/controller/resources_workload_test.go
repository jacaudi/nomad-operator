package controller

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

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

// TestReadinessProbeIsExec guards the B1 fix: verify_https_client=true requires
// a client cert on every HTTPS request, which an httpGet probe cannot present.
// The readiness probe must be an Exec probe that shells out to `nomad operator
// api` (which reads NOMAD_CACERT/NOMAD_CLIENT_CERT/NOMAD_CLIENT_KEY) against
// /v1/agent/health, never an HTTPGet probe.
func TestReadinessProbeIsExec(t *testing.T) {
	nc := minimalCluster("prod", "nomad-system")
	ss := buildStatefulSet(nc, "abc123")
	containers := ss.Spec.Template.Spec.Containers
	var server *corev1.Container
	for i := range containers {
		if containers[i].Name == "nomad" {
			server = &containers[i]
		}
	}
	if server == nil {
		t.Fatal("server container \"nomad\" not found")
	}
	probe := server.ReadinessProbe
	if probe == nil {
		t.Fatal("readiness probe missing")
	}
	if probe.HTTPGet != nil {
		t.Error("readiness probe uses HTTPGet; verify_https_client=true breaks cert-less httpGet probes, must be Exec")
	}
	if probe.Exec == nil {
		t.Fatal("readiness probe must use an Exec handler")
	}
	cmd := strings.Join(probe.Exec.Command, " ")
	if !strings.Contains(cmd, "nomad operator api") {
		t.Errorf("readiness probe exec command = %q, want it to invoke \"nomad operator api\"", cmd)
	}
	if !strings.Contains(cmd, "/v1/agent/health") {
		t.Errorf("readiness probe exec command = %q, want it to target /v1/agent/health", cmd)
	}
}

// TestInitEntrypointInjectsGossipKey guards the B2 fix: gossip encryption must
// actually be enabled via the init container's overlay.hcl, reading the key
// mounted from the gossip Secret at /nomad/gossip/key.
func TestInitEntrypointInjectsGossipKey(t *testing.T) {
	if !strings.Contains(initEntrypoint, "/nomad/gossip/key") {
		t.Error("initEntrypoint does not read the gossip key from /nomad/gossip/key")
	}
	if !strings.Contains(initEntrypoint, "encrypt") {
		t.Error("initEntrypoint does not inject the gossip encrypt key into the overlay HCL")
	}
}
