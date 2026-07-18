# Edge agents — attaching Nomad clients

nomad-operator runs the Nomad **servers** in Kubernetes. Your **clients** (the
workers that actually run jobs) live wherever you want capacity — a Linux box, a
VM, a Mac mini, a NAS at the edge — and **join the operator-managed control plane
over mTLS**. The operator does not install or provision clients; you run the
Nomad agent yourself and point it at the cluster. Once a client registers, a
[`NomadNode`](../runbooks/nomadnode.md) CR appears so you can manage its
eligibility and drain from Kubernetes.

This page is the **shared join procedure** — the prerequisites and base client
config every platform needs. Each platform guide below adds only its **task
driver** (how that host runs workloads) on top of this backbone.

## Platform guides

| Host | Task driver | Guide |
|------|-------------|-------|
| Linux bare metal | systemd-nspawn ([JanMa/nomad-driver-nspawn](https://github.com/JanMa/nomad-driver-nspawn)) | [bare-metal-nspawn.md](bare-metal-nspawn.md) |
| Mac mini (Apple Silicon) | Apple `container` ([anultravioletaurora/nomad-driver-container](https://github.com/anultravioletaurora/nomad-driver-container)) | [mac-mini-container.md](mac-mini-container.md) |
| Linux VM | built-in Isolated Fork/Exec (`exec`) | [linux-vm-exec.md](linux-vm-exec.md) |
| TrueNAS SCALE | Docker (client runs as a container spawning containers) | [truenas.md](truenas.md) |

> **Driver compatibility caveat.** Nomad's target here is **v2.0.4**. The `exec`
> and `docker` drivers are **built into** the Nomad binary. The nspawn and Apple
> `container` drivers are **third-party plugins whose authors last tested against
> Nomad 1.x** — the plugin ABI has been stable across 1.x→2.0 so they will very
> likely load, but neither is verified on 2.x. Bench-test on a throwaway client
> before production, and be ready to rebuild the plugin against a current Go
> toolchain. Each guide repeats the specific risk.

## Join backbone (do this first, every platform)

### 1. A Ready cluster and its address

The client dials the servers' **RPC** endpoint. Get the external address the
operator published:

```bash
kubectl get nomadcluster <cluster> -o jsonpath='{.status.externalAddress}'; echo
kubectl get nomadcluster <cluster> -o jsonpath='{.status.phase}'; echo   # want: Ready
```

The RPC endpoints depend on the cluster's external-access mode
(see [Architecture](../architecture.md#the-control-plane-nomadcluster)):

- **Gateway mode** — one RPC listener **per server**, on the ports you set in
  `spec.externalAccess.gateway.rpcPorts`. The client's server list is
  `["<address>:<port1>", "<address>:<port2>", ...]` (e.g. `:14647, :24647, :34647`).
- **LoadBalancer mode** (single server) — one endpoint, `"<address>:4647"`.

### 2. The CA certificate

The client verifies the servers against the **same CA** the operator consumes.
Pull `ca.crt` from the cluster's cert Secret (`spec.tls.certSecretRef`):

```bash
kubectl get secret <certSecretRef> -o jsonpath='{.data.ca\.crt}' | base64 -d > ca.crt
```

### 3. A client certificate — you must issue this

The operator issues **only the server certificate**. Each client needs its own
leaf cert with SAN **`client.<region>.nomad`**, signed by the **same CA**.
Nomad verifies the embedded role/region name, not the host's address.

With cert-manager (recommended — reuse the Issuer that signs the server cert):

```yaml
apiVersion: cert-manager.io/v1
kind: Certificate
metadata:
  name: nomad-client-edge01
spec:
  secretName: nomad-client-edge01-tls
  issuerRef: { name: <your-nomad-ca-issuer>, kind: Issuer }   # the same CA as the server cert
  commonName: client.global.nomad
  dnsNames: ["client.global.nomad"]     # role SAN; region defaults to "global"
  usages: ["client auth", "server auth"]
```

Then extract the leaf onto the edge host:

```bash
kubectl get secret nomad-client-edge01-tls -o jsonpath='{.data.tls\.crt}' | base64 -d > client.crt
kubectl get secret nomad-client-edge01-tls -o jsonpath='{.data.tls\.key}' | base64 -d > client.key
```

No cert-manager? Sign a `client.<region>.nomad` leaf against your CA by hand
(`openssl`/`cfssl`/`nomad tls cert create -client -region <region>`) — the only
requirements are the SAN and the shared CA.

> Clients do **not** need the gossip key or an ACL token. Gossip is server-only,
> and the client agent authenticates its registration with the mTLS certificate.

### 4. Base client configuration

Every platform starts from this `client.hcl`. Fill in the region, datacenter,
and server list from steps 1–3; each platform guide then adds one `plugin { }`
block for its driver.

```hcl
data_dir  = "/opt/nomad/data"
region    = "global"          # must match spec.region (default "global")
datacenter = "dc1"            # must match a cluster datacenter (default "dc1")

client {
  enabled = true
  servers = ["<address>:14647", "<address>:24647", "<address>:34647"]  # LB mode: ["<address>:4647"]
}

tls {
  http = true
  rpc  = true
  ca_file   = "/etc/nomad.d/ca.crt"
  cert_file = "/etc/nomad.d/client.crt"   # SAN client.<region>.nomad, same CA
  key_file  = "/etc/nomad.d/client.key"
  verify_server_hostname = true
  verify_https_client    = true
}

# + one plugin { } block per the platform guide
```

Copy `ca.crt`, `client.crt`, `client.key`, and `client.hcl` to `/etc/nomad.d/`
on the host.

### 5. Verify the join

Start the agent (each guide shows the exact command for its platform), then:

```bash
nomad node status -self          # on the client → should report "ready"
kubectl get nomadnode            # in Kubernetes → a NomadNode CR appears for this client
```

If the node never appears: confirm the RPC port is reachable
(`nc -vz <address> <rpcPort>`), the client cert's SAN is `client.<region>.nomad`,
and the region/datacenter match the cluster. In LoadBalancer mode, HTTP/CLI
calls that hit the LB address need `-tls-server-name server.<region>.nomad`
(the LB address is not a cert SAN).

---

Once the client is registered, manage it from Kubernetes with
[`NomadNode`](../runbooks/nomadnode.md) (eligibility, drain), and schedule work
onto it with [`NomadJob`](../runbooks/nomadjob.md),
[`NomadPool`](../runbooks/nomadpool.md), and
[`NomadNamespace`](../runbooks/nomadnamespace.md).
