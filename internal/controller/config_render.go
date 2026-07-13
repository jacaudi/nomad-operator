package controller

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"

	nomadv1alpha1 "github.com/jacaudi/nomad-operator/api/v1alpha1"
)

// renderConfig returns the shared Nomad server HCL (per-pod advertise addresses
// are filled by the init container at boot) and a SHA-256 hash over the inputs
// that must trigger a rollout when they change (gateway address, ports, region,
// datacenters, servers, image is on the pod template already).
func renderConfig(nc *nomadv1alpha1.NomadCluster, gatewayAddress string) (string, string) {
	n := names(nc)
	region := nc.Spec.Region

	// retry_join targets the headless service so servers gossip over the pod
	// network (advertise.serf = POD_IP, set by the init container).
	retryJoin := fmt.Sprintf(`"%s.%s.svc.cluster.local"`, n.HeadlessSvc, nc.Namespace)

	var b strings.Builder
	fmt.Fprintf(&b, "region     = %q\n", region)
	fmt.Fprintf(&b, "datacenter = %q\n", firstOr(nc.Spec.Datacenters, "dc1"))
	b.WriteString("data_dir   = \"/var/lib/nomad\"\n")
	b.WriteString("bind_addr  = \"0.0.0.0\"\n\n")
	fmt.Fprintf(&b, "server {\n  enabled          = true\n  bootstrap_expect = %d\n  server_join {\n    retry_join = [%s]\n  }\n}\n\n", nc.Spec.Servers, retryJoin)
	b.WriteString("acl {\n  enabled = true\n}\n\n")
	b.WriteString("tls {\n  http = true\n  rpc  = true\n  ca_file   = \"/nomad/tls/ca.crt\"\n  cert_file = \"/nomad/tls/tls.crt\"\n  key_file  = \"/nomad/tls/tls.key\"\n  verify_server_hostname = true\n  verify_https_client    = true\n}\n")

	body := b.String()
	sum := sha256.Sum256([]byte(body + "|gw=" + gatewayAddress + "|ports=" + fmt.Sprint(rpcAdvertisePorts(nc))))
	return body, hex.EncodeToString(sum[:])
}

// rpcAdvertisePorts returns the per-ordinal RPC advertise ports for the active
// external-access mode. Gateway mode uses the user's gateway.rpcPorts.
func rpcAdvertisePorts(nc *nomadv1alpha1.NomadCluster) []int32 {
	return nc.Spec.ExternalAccess.Gateway.RPCPorts
}

func firstOr(in []string, def string) string {
	if len(in) == 0 {
		return def
	}
	return in[0]
}

func itoa(i int) string                { return strconv.Itoa(i) }
func trimLeadingSpace(s string) string { return strings.TrimPrefix(s, " ") }
