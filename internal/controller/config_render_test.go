package controller

import (
	"strings"
	"testing"
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
