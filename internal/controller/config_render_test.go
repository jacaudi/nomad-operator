package controller

import (
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	nomadv1alpha1 "github.com/jacaudi/nomad-operator/api/v1alpha1"
)

func TestRenderConfigDeterministicHash(t *testing.T) {
	nc := minimalCluster("prod", "nomad-system")
	hcl1, h1 := renderConfig(nc, "10.0.0.5")
	hcl2, h2 := renderConfig(nc, "10.0.0.5")
	if h1 != h2 {
		t.Fatal("hash is not deterministic")
	}
	if !strings.Contains(hcl1, "bootstrap_expect = 3") {
		t.Error("missing bootstrap_expect")
	}
	if !strings.Contains(hcl1, `verify_server_hostname = true`) {
		t.Error("missing TLS verify")
	}
	if !strings.Contains(hcl1, `acl {`) {
		t.Error("missing acl stanza")
	}
	// Address changes must change the hash (so the StatefulSet rolls).
	_, h3 := renderConfig(nc, "10.0.0.9")
	if h1 == h3 {
		t.Error("hash unchanged when gateway address changed")
	}
	_ = hcl2
}

func TestRpcAdvertisePortsLoadBalancerMode(t *testing.T) {
	nc := lbCluster("edge", "nomad-system")
	got := rpcAdvertisePorts(nc)
	if len(got) != 1 || got[0] != 4647 {
		t.Errorf("rpcAdvertisePorts(LB) = %v, want [4647]", got)
	}
	// renderConfig must not panic on a nil gateway block in LB mode, and its
	// hash must fold in the LB address (so an LB-IP change rolls the pods).
	_, h1 := renderConfig(nc, "203.0.113.7")
	_, h2 := renderConfig(nc, "203.0.113.9")
	if h1 == h2 {
		t.Error("hash unchanged when LB address changed")
	}
}

// TestRenderConfigNodeGCThreshold guards the optional server-stanza
// node_gc_threshold: it renders nothing when unset, and renders INSIDE the
// server{} block (not at top level, which Nomad rejects) when set. Setting it
// must also change the rollout hash so the StatefulSet rolls.
func TestRenderConfigNodeGCThreshold(t *testing.T) {
	base := &nomadv1alpha1.NomadCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "c", Namespace: "ns"},
		Spec: nomadv1alpha1.NomadClusterSpec{
			Servers: 1, Region: "global",
			ExternalAccess: nomadv1alpha1.ExternalAccessSpec{
				Mode:    nomadv1alpha1.ExternalAccessGateway,
				Gateway: &nomadv1alpha1.GatewaySpec{Mode: nomadv1alpha1.GatewayModeManaged, RPCPorts: []int32{14647}, HTTPHostname: "h"},
			},
		},
	}
	unsetBody, unsetHash := renderConfig(base, "1.2.3.4")
	if strings.Contains(unsetBody, "node_gc_threshold") {
		t.Error("unset: node_gc_threshold must not render")
	}

	set := base.DeepCopy()
	set.Spec.NodeGCThreshold = &metav1.Duration{Duration: 48 * time.Hour}
	setBody, setHash := renderConfig(set, "1.2.3.4")
	// node_gc_threshold is a SERVER-stanza option — it must render INSIDE the
	// server{} block, not at top level (Nomad rejects a top-level key).
	serverStart := strings.Index(setBody, "server {")
	serverEnd := strings.Index(setBody[serverStart:], "\n}\n") + serverStart
	if serverStart < 0 || !strings.Contains(setBody[serverStart:serverEnd], `node_gc_threshold = "48h0m0s"`) {
		t.Errorf("set: node_gc_threshold must render inside server{}, got:\n%s", setBody)
	}
	if setHash == unsetHash {
		t.Error("setting node_gc_threshold must change the rollout hash")
	}
}

// TestRenderConfigSingleServerBootstrapExpect guards FR-1: a servers=1
// control plane renders bootstrap_expect=1, so Raft bootstraps immediately
// without waiting to see peers.
func TestRenderConfigSingleServerBootstrapExpect(t *testing.T) {
	nc := singleServerCluster("prod", "nomad-system")
	hcl, _ := renderConfig(nc, "10.0.0.5")
	if !strings.Contains(hcl, "bootstrap_expect = 1") {
		t.Errorf("rendered config missing bootstrap_expect = 1:\n%s", hcl)
	}
}
