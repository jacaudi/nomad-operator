# NomadCluster operator runbook

Operator procedures for the `NomadCluster` CRD (slice 2, "control plane"). See
`docs/designs/2026-07-10-nomadcluster-control-plane-design.md` for the full
design; this runbook covers deploy-time prerequisites, manual verification
steps that are not automated, and incident procedures.

Naming used below assumes a `NomadCluster` named `<name>` in namespace
`<ns>`, with `spec.region` left at its default `global` and
`spec.servers: 3`. Substitute your own values throughout.

## 1. Deploy prerequisites

The operator does not provision these; they must exist before a
`NomadCluster` will reach `Ready`.

### 1.1 cert-manager `Certificate` (mTLS material)

The operator reads mTLS material from the Secret named by
`spec.tls.certSecretRef` — it does **not** create the `Certificate` itself.
The Secret must carry `tls.crt`, `tls.key`, and `ca.crt`, and the certificate
**must** include every one of these SANs:

- `server.<region>.nomad` and `client.<region>.nomad` — Nomad's own mTLS
  verifies the peer's embedded role/region name, not the dialed address. The
  operator's `internal/nomad.Client` sets `TLSServerName = server.<region>.nomad`.
- `spec.gateway.httpHostname` — the external HTTP front door is a `TLSRoute`
  passthrough (SNI routing), so the cert must carry the hostname clients
  dial, or routing/verification fails.
- `localhost` and `127.0.0.1` — required for the in-pod `exec` readiness
  probe (`nomad operator api ... NOMAD_ADDR=https://127.0.0.1:4646`) and any
  in-pod CLI/debug session.

Example, for `region: global` and `httpHostname: nomad.example.com`:

```yaml
apiVersion: cert-manager.io/v1
kind: Certificate
metadata:
  name: <name>-nomad-tls
  namespace: <ns>
spec:
  secretName: <name>-nomad-tls
  issuerRef:
    name: <your-issuer>
    kind: ClusterIssuer
  dnsNames:
    - server.global.nomad
    - client.global.nomad
    - nomad.example.com
    - localhost
  ipAddresses:
    - 127.0.0.1
  duration: 2160h    # 90d
  renewBefore: 360h  # 15d
```

Set `spec.tls.certSecretRef: <name>-nomad-tls` on the `NomadCluster`. The
operator waits (with a clear `Pending` condition) if cert-manager has not
yet issued the Secret.

### 1.2 Gateway API experimental-channel CRDs

`TCPRoute` and `TLSRoute` are `v1alpha2` **experimental-channel** types, not
part of the Gateway API standard-channel install. Install the experimental
channel before creating any `NomadCluster`:

```bash
kubectl apply -f https://github.com/kubernetes-sigs/gateway-api/releases/download/v1.2.1/experimental-install.yaml
```

Confirm `TCPRoute` and `TLSRoute` are present:

```bash
kubectl get crd tcproutes.gateway.networking.k8s.io tlsroutes.gateway.networking.k8s.io
```

### 1.3 Cilium LBIPAM pool

Whether `spec.gateway.mode` is `Managed` (operator creates the Gateway) or
`Existing` (operator attaches to a user-owned Gateway), the underlying
`GatewayClass` needs an address to assign. With Cilium's Gateway API support,
that means an `CiliumLoadBalancerIPPool` (or equivalent LBIPAM configuration)
covering the range the Gateway controller draws from:

```yaml
apiVersion: cilium.io/v2alpha1
kind: CiliumLoadBalancerIPPool
metadata:
  name: nomad-gateway-pool
spec:
  blocks:
    - cidr: 10.0.20.0/28   # size to your environment
```

Without a pool, the Gateway never gets `status.addresses`, and reconcile
stalls at `GatewayReady=False` / `WaitingForAddress` indefinitely.

### 1.4 Schedulable node count

At least `spec.servers` nodes must be schedulable (untainted, capacity
available) — the StatefulSet uses pod anti-affinity to spread one server per
node, so with `servers: 3` you need 3 distinct eligible nodes or the third
(and any subsequent) pod stays `Pending`.

## 2. External-client join — manual verification

This is **not automated** (design §4, Definition of Done). After the
`NomadCluster` reaches `Ready`, verify an out-of-cluster client (an edge
node, a TrueNAS box, etc.) can join over the Gateway's exposed RPC surface.

1. Fetch the assigned Gateway address and the CA used by the cluster:

   ```bash
   kubectl get nomadcluster <name> -n <ns> -o jsonpath='{.status.gatewayAddress}'
   kubectl get secret <name>-nomad-tls -n <ns> -o jsonpath='{.data.ca\.crt}' | base64 -d > ca.crt
   ```

2. On the external client, configure Nomad's client stanza to dial the
   Gateway's per-server RPC listeners (one TCP listener per server, on
   `spec.gateway.rpcPorts`) and present the same CA:

   ```hcl
   # /etc/nomad.d/client.hcl on the edge/TrueNAS host
   data_dir = "/opt/nomad/data"

   client {
     enabled = true
     servers = ["<gatewayAddress>:14647", "<gatewayAddress>:24647", "<gatewayAddress>:34647"]
   }

   tls {
     http = true
     rpc  = true
     ca_file   = "/etc/nomad.d/ca.crt"
     cert_file = "/etc/nomad.d/client.crt"
     key_file  = "/etc/nomad.d/client.key"
     verify_server_hostname = true
     verify_https_client    = true
   }
   ```

   The client's own cert must carry `client.<region>.nomad` as its Nomad
   role/region SAN (a separate cert-manager `Certificate`/leaf issued for the
   edge client, signed by the same CA — not the server cert from §1.1).

3. Start the client agent and confirm it registers and reaches `ready`:

   ```bash
   nomad node status -self
   # STATUS should read "ready" within ~30s
   ```

4. Cross-check from the cluster side:

   ```bash
   kubectl exec -n <ns> <name>-server-0 -- nomad node status
   # the external client's node ID should appear with Status=ready
   ```

If the client never advances past `initializing`, check §5's Gateway
listener-naming contract and §6's diagnosis steps first — a misnamed or
misconfigured RPC listener is the most common cause.

## 3. ACL-reset procedure (token Secret lost)

Nomad has **no `nomad acl bootstrap-reset` command.** If the token Secret
(`<name>-nomad-bootstrap-token`) is deleted or lost after a successful
bootstrap, ACL access is otherwise unrecoverable without this procedure. Do
not delete/edit the Secret casually — treat it as a one-of-a-kind credential
(design §3.8: it is deliberately retained across `NomadCluster` deletion for
exactly this reason).

**"Secret present but cluster un-bootstrapped" is not a stuck state.** The
operator only treats bootstrap as confirmed once it has annotated the token
Secret with `nomad.operator.io/acl-bootstrapped: "true"` — and it sets that
annotation *only* after a confirmed-successful (or confirmed-already-done)
`ACLBootstrap` call, never merely because the Secret exists. If a first
bootstrap attempt fails transiently (e.g. a leader flap right after
election), the Secret is left un-annotated and the next reconcile
**re-attempts `ACLBootstrap` with the same token** rather than skipping it.
So you should not normally need this procedure just because a reconcile
raced with a leader election — give it a couple of reconcile intervals to
self-heal first, and only fall back to the manual reset below if
`ACLBootstrapped` stays `False` and the Secret stays un-annotated.

1. Identify the current leader pod:

   ```bash
   kubectl exec -n <ns> <name>-server-0 -- nomad operator api /v1/status/leader
   ```

2. Attempt a bootstrap (from any pod, or via `internal/nomad.Client.ACLBootstrap`)
   to read the **reset index** out of the "ACL bootstrap already done" error:

   ```bash
   kubectl exec -n <ns> <leader-pod> -- nomad acl bootstrap
   # Error: ... ACL bootstrap already done ...
   # Error output includes: "reset index: <N>"
   ```

3. Write that index to the reset file **on the leader pod's data volume**
   (`data_dir = /var/lib/nomad`, per `internal/controller/config_render.go`):

   ```bash
   kubectl exec -n <ns> <leader-pod> -- sh -c 'echo <N> > /var/lib/nomad/server/acl-bootstrap-reset'
   ```

4. Delete the (now-invalid) token Secret if it still exists, so the
   reconciler regenerates a fresh bootstrap token and Secret:

   ```bash
   kubectl delete secret <name>-nomad-bootstrap-token -n <ns>
   ```

5. Let the reconciler run (or force it: annotate/touch the `NomadCluster`).
   It writes a new token Secret **before** calling `BootstrapOpts` (design
   §3.3 ordering), then re-bootstraps against the reset index. Verify:

   ```bash
   kubectl get secret <name>-nomad-bootstrap-token -n <ns>
   kubectl get nomadcluster <name> -n <ns> -o jsonpath='{.status.conditions}'
   # ACLBootstrapped should flip back to True
   ```

This is manual and invasive by design — it is a last resort, not a routine
operation.

## 4. Verify: `gatewayAddress` must be pod-routable

**Load-bearing assumption (design §7.1).** Inter-server Raft rides
`advertise.rpc = <status.gatewayAddress>:<rpcPorts[ordinal]>` — the same
address external clients dial. If `status.gatewayAddress` is not reachable
*from inside the cluster's pod network*, the Nomad servers cannot form quorum
with each other, regardless of how healthy the Gateway looks externally.

Verify this explicitly before relying on a new environment, and any time
quorum fails to form after a Gateway address change:

```bash
kubectl run gw-dial-test -n <ns> --rm -it --restart=Never --image=busybox -- \
  sh -c 'nc -zv <gatewayAddress> 14647 && nc -zv <gatewayAddress> 24647 && nc -zv <gatewayAddress> 34647'
```

All three (one per `rpcPorts` entry) must connect. If any fail, the Cilium
Gateway is not pod-routable in this environment and the design's documented
fallback applies: replace per-server RPC exposure with 3
`type=LoadBalancer` Services (one stable LBIPAM IP per pod, port 4647) —
see design §3.4, "Fallback." This is not built by default; it requires a
manual migration off the Gateway `TCPRoute`s.

## 5. Existing-mode Gateway listener-naming contract

When `spec.gateway.mode: Existing`, the operator never creates or mutates
the referenced Gateway (`spec.gateway.ref`) — the user pre-provisions it, and
the operator only attaches `TLSRoute`/`TCPRoute`s to it. Those routes attach
via a **fixed `parentRefs[].sectionName`**
(`internal/controller/resources_gateway.go`), so the user's Gateway **must**
carry listeners named exactly:

| Listener name | Protocol | Port | Purpose |
|---|---|---|---|
| `http` | `TLS` (passthrough) | matches `spec.gateway.httpHostname`'s route; `hostname` on the listener **must equal** `spec.gateway.httpHostname` | API/UI front door (`TLSRoute`) |
| `rpc-0` | `TCP` | `spec.gateway.rpcPorts[0]` | server-0 RPC (`TCPRoute`) |
| `rpc-1` | `TCP` | `spec.gateway.rpcPorts[1]` | server-1 RPC (`TCPRoute`) |
| … | `TCP` | … | one per server — `rpc-<ordinal>` up to `rpc-<servers-1>` |

Gateway API matches a route's `sectionName` against the listener's **literal
`Name`** — not its port or protocol. A listener with the right port and
protocol but a different name (e.g. `nomad-rpc-0` instead of `rpc-0`) will
**not** be matched, and the route silently fails to attach even though the
Gateway itself looks healthy.

Additionally, every one of these listeners' `allowedRoutes.namespaces` must
admit the `NomadCluster`'s namespace — either `From: All`, or `From: Same`
**and** the Gateway lives in the same namespace as the `NomadCluster`.
Cross-namespace `parentRefs` are otherwise silently rejected by the Gateway
API implementation, not just by the operator's own check.

Minimal compliant listener block for a 3-server cluster
(`httpHostname: nomad.example.com`, `rpcPorts: [14647, 24647, 34647]`,
Gateway and `NomadCluster` in the same namespace):

```yaml
listeners:
  - name: http
    protocol: TLS
    port: 443
    hostname: nomad.example.com
    tls:
      mode: Passthrough
    allowedRoutes:
      namespaces:
        from: Same
  - name: rpc-0
    protocol: TCP
    port: 14647
    allowedRoutes:
      namespaces:
        from: Same
  - name: rpc-1
    protocol: TCP
    port: 24647
    allowedRoutes:
      namespaces:
        from: Same
  - name: rpc-2
    protocol: TCP
    port: 34647
    allowedRoutes:
      namespaces:
        from: Same
```

## 6. Diagnosing Existing-mode "not Ready"

The operator currently surfaces a **single generic reason** for every
Existing-mode Gateway verification failure:
`GatewayReady=False` / `WaitingForAddress` /
`"gateway address not assigned"`. This one message covers several distinct
root causes — a missing or misnamed listener, a namespace `allowedRoutes`
that doesn't admit the cluster's namespace, or simply no address assigned
yet — so it does not by itself tell you which one applies.

When a `NomadCluster` with `spec.gateway.mode: Existing` is stuck at
`GatewayReady=False`, check these in order:

1. **Does the referenced Gateway exist?**

   ```bash
   kubectl get gateway <spec.gateway.ref.name> -n <spec.gateway.ref.namespace>
   ```

   If not found, the reconciler treats this as `ready=false` (not an error)
   and waits — create the Gateway.

2. **Do its listeners match the naming/port/protocol contract in §5?**

   ```bash
   kubectl get gateway <ref.name> -n <ref.namespace> -o jsonpath='{.spec.listeners}' | jq
   ```

   Confirm: one listener named exactly `http` (protocol `TLS`, `hostname`
   equal to `spec.gateway.httpHostname`), and one listener named exactly
   `rpc-<ordinal>` per entry in `spec.gateway.rpcPorts`, each `protocol: TCP`
   with the matching port.

3. **Does `allowedRoutes.namespaces` on those listeners admit the
   `NomadCluster`'s namespace?**

   Same output as step 2 — check `allowedRoutes.namespaces.from` per
   listener. `Same` only works if the Gateway and the `NomadCluster` share a
   namespace; otherwise it needs `All` (or a `Selector`, which this operator
   version treats as **not admitted** — fail closed, no selector support
   yet).

4. **Has the Gateway been assigned an address?**

   ```bash
   kubectl get gateway <ref.name> -n <ref.namespace> -o jsonpath='{.status.addresses}'
   ```

   Empty means the underlying `GatewayClass`/LBIPAM hasn't provisioned one
   yet — see §1.3.

Steps 1–3 fail *silently* from the operator's perspective (`ready=false`,
no error, no differentiated condition reason) — the only way to tell them
apart today is by inspecting the Gateway object directly as above.

---

## 7. Integration-test verification (Nomad node status set)

The hermetic ACL integration test (`internal/nomad/client_write_integration_test.go`,
`//go:build integration`) boots a real ACL-enabled `nomad agent -dev`, bootstraps ACLs
with an operator-supplied token, and reads back the node set.

**Verified 2026-07-11** against **Nomad v2.0.4** (revision `5b83b133998a`, the exact commit
the `github.com/hashicorp/nomad/api` module is pinned to) running inside a Linux container:

- `ACLTokens().BootstrapOpts(token)` echoes the supplied token back as the secret ID
  (confirms the operator's idempotent Secret-first bootstrap contract).
- An authenticated client (token from the bootstrap Secret) can `Ping` and `ListNodes`.
- **Observed node status value: `ready`** — this closes Foundation open-item #1
  (the `Node.Status` value set surfaced into `NomadNodeStatus` in slice 3 is `ready`,
  plus `initializing`/`down`/`disconnected`/`draining` per the Nomad node lifecycle).

Reproduce (requires Docker; the binary is Linux-only so it runs in a container):

```bash
# Build a golang image with the pinned nomad binary
printf 'FROM golang:1.26\nCOPY --from=hashicorp/nomad:2.0.4 /bin/nomad /usr/local/bin/nomad\n' \
  | docker build -t nomad-itest:local -f - .
# Run the tagged integration tests (nomad -dev needs cgroup access)
docker run --rm --privileged --cgroupns=host -v "$PWD":/src -w /src \
  nomad-itest:local \
  go test -tags integration ./internal/nomad/ -run TestACLBootstrapAndLeaderLive -v
```

On a host with a native `nomad` v2.0.4 binary on `PATH`, `make test-integration` runs the
same test directly; without a `nomad` binary it skips cleanly.

## 8. External access modes

`spec.externalAccess.mode` selects how the control plane is exposed to
out-of-cluster clients:

| Mode | `servers` | Exposure | External objects |
|---|---|---|---|
| `Gateway` (default) | 1, 3, or 5 | One RPC listener per server behind a Gateway (`TLSRoute` + per-ordinal `TCPRoute`s), addressed by `status.gatewayAddress` | Gateway, `TLSRoute`, per-pod `Service`s, `TCPRoute`s |
| `LoadBalancer` | **1 only** | A single `type: LoadBalancer` Service in front of the lone server | `<name>-lb` Service |

`LoadBalancer` mode is single-VIP and **north-south only**: with `servers: 1`
there is no east-west Raft quorum to serve, so a single VIP suffices. It is
rejected (by CEL) for any multi-server cluster — a 3- or 5-server cluster
needs the Gateway's per-server RPC listeners for inter-server Raft.

### 8.1 LoadBalancer provisioning and the `Pending` gate

`LoadBalancer` mode provisions exactly one Service named `<name>-lb`
(`type: LoadBalancer`) exposing RPC `4647` and HTTP `4646`, selecting the
single server pod directly. It provisions **no** Gateway, per-pod Services,
or routes.

The cluster stays `Pending` (condition `ExternalAccessReady=False` /
`WaitingForAddress`) until the LB provider assigns
`status.loadBalancer.ingress` on that Service — the operator reads the
assigned IP/hostname from there and advertises it as `status.externalAddress`.
This is the **same failure shape as a missing Gateway controller** (§1.3): a
`type: LoadBalancer` Service needs something to fulfil it — a cloud load
balancer, MetalLB, or Cilium LBIPAM. Without one, the Service never gets an
ingress address and reconcile stalls at `WaitingForAddress` indefinitely.

Verify the assigned address:

```bash
kubectl get svc <name>-lb -n <ns> -o jsonpath='{.status.loadBalancer.ingress}'
kubectl get nomadcluster <name> -n <ns> -o jsonpath='{.status.externalAddress}'
```

### 8.2 HTTP/UI over the LoadBalancer needs `-tls-server-name`

Nomad RPC is **role-verified**: the server presents `server.<region>.nomad`
and clients verify that embedded role/region name, not the dialed address. So
an edge agent joins over the LB address on `4647` with **no extra flag** — the
same as Gateway mode (§2).

Nomad HTTP, however, is **address-verified**: the CLI/UI verifies the
certificate against the address it dialed, and the LB address is **not** in
the cert SANs (§1.1). So HTTP/UI access over the LB address requires overriding
the expected server name:

```bash
nomad status -address=https://<externalAddress>:4646 \
  -tls-server-name server.<region>.nomad
# or: export NOMAD_TLS_SERVER_NAME=server.<region>.nomad
```

The operator's own in-cluster client is **unaffected** — it dials the
in-cluster API Service and already sets `TLSServerName = server.<region>.nomad`
(`internal/controller/nomadcluster_controller.go`).

### 8.3 Immutability

Both `spec.externalAccess.mode` and `spec.servers` are immutable (enforced by
CEL). A cluster cannot switch modes in place, and because `servers` is fixed, a
3- or 5-server cluster can **never** become `LoadBalancer` — the mode is
decided once, at creation.
