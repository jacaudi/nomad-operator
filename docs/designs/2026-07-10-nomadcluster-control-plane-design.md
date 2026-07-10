# Design — `nomad-operator` Control Plane Slice (`NomadCluster`)

| | |
|---|---|
| **Component** | `nomad-operator` — a Kubernetes operator that provisions and manages a Nomad control plane on K8s, with workloads on edge clients |
| **This document** | Design for the **Control Plane slice** (slice 2 of 6) — the first slice that stands up real Nomad servers and produces the authenticated endpoint later slices consume |
| **Target runtime** | Nomad **v2.0.4**; K8s 1.28+ with Gateway API + Cilium (LBIPAM + Gateway); Go **1.26.4** |
| **Status** | Approved 2026-07-10 — ready for implementation plan |
| **Builds on** | `docs/designs/2026-07-09-nomad-operator-foundation-design.md` (slice 1, merged) — §2 roadmap, §4.2 client seam |

---

## 1. Background & framing

Foundation (slice 1, merged) delivered the buildable skeleton: an idle controller-runtime
manager, a per-endpoint read-only `internal/nomad.Client` built from an explicit `api.Config`
(never `api.DefaultConfig()`), a compile-time `contract.go` pinning the exact `api` surface, and the
`nomad-pin`/`verify`/`test-integration` toolchain against `api` at commit `5b83b133998a` (== v2.0.4).

This slice is the first **consumer** of that client seam and the first to create Kubernetes
workloads. It introduces the `NomadCluster` CRD and a reconciler that turns one custom resource into
a **3-server HA Nomad control plane**: a StatefulSet with Raft quorum, persistent Raft storage,
mutual TLS, gossip encryption, and ACLs — reachable by **edge clients outside the cluster** (e.g.
TrueNAS) that join over Nomad RPC. The end state is an authenticated, reachable endpoint that the
per-cluster `internal/nomad.Client` consumes.

**Key reframe carried from Foundation (Appendix A of that doc):** Nomad 2.0 is a versioning-scheme
rename, not an API break. The `github.com/hashicorp/nomad/api` types are byte-identical to current
1.11.x; building against 2.0.4 is the same Go-API surface. No `/v2` import path; the `api` submodule
is pinned by commit, not semver.

### 1.1 Nomad networking facts this design rests on (verified against current docs)

- **Ports:** HTTP **4646** (TCP; UI + REST API), RPC **4647** (TCP; agent↔agent), Serf **4648**
  (TCP+UDP; **server-to-server** gossip only).
- **Edge clients connect on the RPC port** and hold a **persistent mTLS TCP connection** to a
  server; they do **not** participate in Serf or Raft. Clients are given a `servers` list (or
  `server_join.retry_join`) of server RPC addresses.
- **`advertise.rpc` is singular** — the same advertised address is used by **both** external clients
  **and** Raft peers (peers learn it from Serf tags: `rpc_addr` + `port`). It therefore must be
  reachable by both. There is no separate "advertise to clients" vs "advertise to peers".
- **mTLS is role/region-based, not hostname-based.** Nomad verifies the peer's embedded name
  (`server.<region>.nomad`, `client.<region>.nomad`), not the address you dialed. A standard TLS
  client (the operator's `api.Client`) must therefore set `TLSServerName=server.<region>.nomad`
  rather than rely on IP/DNS SANs matching the dialed address.
- **Nomad has no gRPC agent API.** The public API is REST on 4646; 4647 is the internal RPC protocol
  (a long-lived multiplexed mTLS TCP stream, **not** SNI-routable HTTPS).
- **ACL bootstrap** (`ACLTokens().Bootstrap()` / `nomad acl bootstrap`) requires a running cluster
  that has **elected a leader**, returns the management token **secret ID once only**, and errors if
  already bootstrapped.

## 2. Scope of this slice

**In scope.**

1. The `NomadCluster` CRD (spec + status), group `nomad.operator.io/v1alpha1`.
2. A reconciler that provisions a 3-server Raft-quorum StatefulSet with anti-affinity and persistent
   Raft volumes, gated on quorum health.
3. Security: mutual TLS (cert-manager-issued certs), gossip encryption (operator-generated key), ACL
   bootstrap (operator-run, token captured to a Secret).
4. An **all-under-Gateway** external join surface (Gateway API): HTTP via HTTP/TLS route, RPC via
   per-server TCP routes; Serf stays in-cluster. Two Gateway-ownership modes (Managed, Existing).
5. Quorum-safe rolling updates via Kubernetes primitives.
6. Construction of one per-cluster `internal/nomad.Client` from the CR — the authenticated endpoint.

**Out of scope (YAGNI boundary).** Server-count scaling / Raft peer-set membership changes (the
`servers` field is immutable); custom drain-and-step-down upgrade orchestration; multi-region /
federation; automated cert **rotation**; Secret-watching / config hot-reload; and any
`NomadNode` / `NomadPool` / `NomadJob` behavior (later slices). Actually joining a real external
client is a **documented manual verification**, not automated slice-2 acceptance (client management
is slice 3).

## 3. Design

### 3.1 CRD — `nomad.operator.io/v1alpha1`, kind `NomadCluster`

```yaml
apiVersion: nomad.operator.io/v1alpha1
kind: NomadCluster
metadata:
  name: prod
spec:
  image: hashicorp/nomad:2.0.4        # required; the Nomad server image/version
  servers: 3                          # default 3; IMMUTABLE (CEL); allowed ∈ {3,5}
  region: global                      # default "global"
  datacenters: [dc1]                  # default ["dc1"]

  storage:
    size: 10Gi                        # required; volumeClaimTemplate request
    storageClassName: ""              # optional; "" = cluster default StorageClass

  tls:
    certSecretRef: nomad-server-tls   # required; a Secret (kubernetes.io/tls + ca.crt) issued by
                                      #   cert-manager. Cert SANs MUST include
                                      #   server.<region>.nomad and client.<region>.nomad.

  gateway:
    mode: Managed                     # Managed | Existing; default Managed
    className: cilium                 # required iff mode=Managed (GatewayClass to instantiate)
    ref:                              # required iff mode=Existing
      name: shared-gw
      namespace: gateway-system
    rpcPorts: [14647, 24647, 34647]   # len == servers; one L4 listener port per server
    httpHostname: nomad.example.com   # hostname for the HTTP/TLS route to the API/UI

  resources: {}                       # optional; pod resource requests/limits

status:
  phase: Pending                      # Pending | Bootstrapping | Ready | Degraded
  conditions: []                      # Reconciled, GatewayReady, QuorumHealthy, ACLBootstrapped, Ready
  observedGeneration: 0
  gatewayAddress: ""                  # the Gateway's assigned address (Managed: created; Existing: read)
  members: []                         # [{name, addr, status, leader bool}]
  leader: ""                          # e.g. "prod-server-1.global"
  quorum: ""                          # e.g. "3/3"
  endpoint: ""                        # in-cluster HTTPS the operator's Client dials, e.g.
                                      #   https://prod-nomad.<ns>.svc:4646
  bootstrapTokenSecretRef: ""         # operator-written Secret holding the ACL management token
  gossipKeySecretRef: ""              # operator-written Secret holding the gossip key
```

**Design rationale.**

- **`servers` immutable, ∈ {3,5}.** A defaulted, CEL-validated field reads better than a hardcoded
  literal and costs nothing (No-Wall seam), while immutability sidesteps the hard problem of changing
  a live Raft peer set — explicitly out of scope. Quorum math (`floor(n/2)+1`) generalizes for free.
- **Security-material ownership is expressed in the type.** The one Secret the *user* provides
  (cert-manager `certSecretRef`) is a **spec input**. The two Secrets the *operator* owns (gossip
  key, ACL management token) are **status outputs** — they are generated, not supplied, and the
  reconciler records where it put them.
- **`endpoint` is the seam output.** It is exactly the address the reconciler feeds into
  `internal/nomad.New(Config)` (§3.6) — an in-cluster ClusterIP, not the external Gateway, because
  the operator runs in the same cluster.
- **`gateway` is a discriminated union** (§3.4). `rpcPorts` length is validated to equal `servers`.

### 3.2 HA topology

- **StatefulSet**, `spec.servers` replicas, stable identities `<name>-server-{0..N-1}`,
  `bootstrap_expect = servers`.
- **Raft peer discovery.** A **headless Service** gives each pod stable DNS. Each server's
  `server_join.retry_join` lists the pod DNS names; Serf gossip (4648) forms membership over the
  **pod network**.
- **Persistent Raft state.** `volumeClaimTemplates` → one PV per pod mounted at Nomad's `data_dir`,
  so Raft logs/snapshots survive pod restarts and rescheduling.
- **Anti-affinity.** `requiredDuringSchedulingIgnoredDuringExecution` pod anti-affinity on hostname —
  one server per node, so a single node loss never costs quorum.
- **Per-pod config via an init container.** StatefulSet pods share a template but each server needs a
  distinct `advertise.rpc`. An init container reads the pod ordinal from its own name, selects
  `rpcPorts[ordinal]`, and renders the final agent config with:
  - `advertise.rpc  = <status.gatewayAddress>:<rpcPorts[ordinal]>` (external + peer reachable),
  - `advertise.serf = <POD_IP>` (downward API — gossip stays on the pod network; only RPC/Raft
    traverse the Gateway),
  - `advertise.http = <status.gatewayAddress>` (for the external HTTP route),
  - `bind_addr = 0.0.0.0`.

  This is the standard StatefulSet per-pod-config idiom. `status.gatewayAddress` + `rpcPorts` are
  delivered to the init container via an operator-rendered **ConfigMap** (the init container cannot
  read CR status directly).

### 3.3 Security (hybrid)

Three distinct materials, each owned by whoever is best at it:

- **mTLS — cert-manager.** The user creates a cert-manager `Certificate` (referencing their issuer)
  whose Secret is named by `spec.tls.certSecretRef`, with SANs `server.<region>.nomad` and
  `client.<region>.nomad`. The operator mounts it into the server pods and enables
  `tls { http = true, rpc = true, verify_server_hostname = true, verify_https_client = true }`.
  Because verification is role/region-based, the operator's own `api.Client` sets
  `TLSServerName = server.<region>.nomad` and presents the client cert — it does **not** depend on
  the dialed address appearing in a SAN.
- **Gossip — operator-generated.** On first reconcile the operator generates a 32-byte base64 key,
  stores it in the Secret named by `status.gossipKeySecretRef`, and mounts it as
  `server { encrypt = "…" }`. The same key is used by all servers in the region. (Key **rotation** is
  out of scope.)
- **ACL — operator-bootstrapped.** Servers run with `acl { enabled = true }`. After the cluster
  reaches quorum and elects a leader, the reconciler calls `ACLTokens().Bootstrap()` **exactly once**
  and captures the management token.

  **Correctness requirement — the bootstrap token is returned once.** The reconciler MUST write the
  management-token Secret **before** it sets the `ACLBootstrapped` condition to true, and MUST NOT
  re-attempt bootstrap once that condition is true and the Secret exists. If `Bootstrap()` returns an
  "already bootstrapped" error while the condition/Secret are absent (e.g. a manual bootstrap
  happened out of band), the reconciler surfaces a `Degraded`/`ACLBootstrapped=False` condition with
  a clear message rather than looping. **Documented recovery:** if the token Secret is ever lost
  after a successful bootstrap, an operator must run `nomad acl bootstrap-reset` on the cluster and
  let the reconciler re-bootstrap — this is a manual, destructive-to-the-token operation, called out
  in the runbook.

### 3.4 External join surface (all-under-Gateway)

Nomad's architecture constrains what a Gateway can carry, so the surface splits by port:

- **HTTP 4646 → Gateway (`HTTPRoute`, or `TLSRoute` passthrough to preserve end-to-end mTLS).**
  Plain HTTPS, stateless, any server can answer — a single Gateway VIP + `httpHostname` is correct.
  This is the human/CLI/API front door.
- **RPC 4647 → per-server L4 exposure via Gateway `TCPRoute`s.** RPC **cannot** go through a single
  SNI VIP: (a) each server must advertise a **distinct** RPC address (its Raft identity) — one shared
  VIP would collide three identities and break Raft; (b) Nomad RPC is a long-lived multiplexed mTLS
  TCP stream, not SNI-routable HTTPS. So the operator creates **one TCP listener per server** (on
  `rpcPorts`) and **one `TCPRoute` per server**, each forwarding to a **per-pod ClusterIP Service**
  (selects a single pod via `statefulset.kubernetes.io/pod-name`). Edge clients get
  `servers = ["<gatewayAddress>:<rpcPorts[0]>", …]` and reach RPC over mTLS.
- **Serf 4648 → never exposed.** Server-to-server only; stays on the pod network (`advertise.serf`).

**Consequence (accepted, documented):** since `advertise.rpc` is shared by clients and peers,
inter-server Raft traverses the Gateway data path. Gossip does **not** (it uses `advertise.serf` on
the pod network), so only the RPC/Raft streams ride the Gateway. This requires the Gateway address to
be **in-cluster routable** — see §7 assumption 1.

**Two Gateway-ownership modes (discriminated by `spec.gateway.mode`).** Only Gateway ownership
differs; the Route shapes, per-pod backend Services, the `rpcPorts` contract, and advertise-address
rendering are **single-sourced** across both modes.

| Concern | **Managed** *(default)* | **Existing** |
|---|---|---|
| Gateway resource | Operator creates a **dedicated** Gateway from `spec.gateway.className`, with the HTTP listener + `servers` TCP listeners | User owns a **shared** Gateway (`spec.gateway.ref`); operator **never mutates** it |
| Listeners on `rpcPorts` + HTTP | Operator owns and reconciles them | **User pre-provisions** them; reconcile sets `GatewayReady=False`/`Pending` with a precise message if a required listener/port is missing |
| `TCPRoute`s / HTTP route + per-pod Services | Operator owns (attach via `parentRefs` to the created Gateway) | Operator owns (attach via `parentRefs` → `spec.gateway.ref`, by port / `sectionName`) |
| `status.gatewayAddress` | Observed from the created Gateway's assigned address | Read from the referenced Gateway's assigned address |

**Fallback (documented, not built by default):** if the Gateway path proves impractical in a given
environment, the same per-server L4 semantics can be delivered by **3 `type=LoadBalancer` Services**
(one stable LBIPAM IP per pod, advertising on the standard 4647). This is a drop-in replacement for
the RPC exposure only; it is recorded here as the escape hatch, not implemented in slice 2.

### 3.5 Reconcile ordering & phase machine

Pods need `advertise.rpc` — hence `status.gatewayAddress` — **before** they boot, and ACL bootstrap
needs a live leader **after** they boot. The reconcile is therefore staged and idempotent:

1. **Pending** — validate spec; ensure the gossip-key Secret exists (generate if absent); ensure the
   cert Secret (`certSecretRef`) is present (wait, with a clear condition, if cert-manager hasn't
   issued it yet).
2. **Ensure Gateway** — Managed: create/reconcile the dedicated Gateway; Existing: look up
   `spec.gateway.ref`, verify the required listeners exist. Wait until an address is assigned; record
   it in `status.gatewayAddress` and set `GatewayReady`.
3. **Render config** — write the ConfigMap carrying `gatewayAddress` + `rpcPorts` + region/DC/gossip
   wiring for the init container.
4. **Provision workloads** — headless Service, per-pod ClusterIP Services, the in-cluster API
   ClusterIP Service, the StatefulSet, the PDB, the HTTP route, and the per-server `TCPRoute`s.
5. **Bootstrapping** — wait for quorum + leader (via the read client against the in-cluster
   endpoint); then run ACL bootstrap once and write the token Secret (§3.3); set `ACLBootstrapped`.
6. **Ready** — publish `endpoint`, `members`, `leader`, `quorum`; set `QuorumHealthy` and `Ready`.
   Drop to **Degraded** if quorum is later lost.

### 3.6 Per-cluster Client construction (the No-Wall seam)

The reconciler builds **one `internal/nomad.Client` per `NomadCluster`** from the CR — no global
singleton, explicit `api.Config` only:

```go
cfg := nomad.Config{
    Address:       status.endpoint,          // in-cluster ClusterIP, https://…:4646
    Region:        spec.region,
    Token:         <read from status.bootstrapTokenSecretRef>,
    TLS: nomad.TLSConfig{
        CACert:     <ca.crt   from certSecretRef>,
        ClientCert: <tls.crt  from certSecretRef>,
        ClientKey:  <tls.key  from certSecretRef>,
    },
    TLSServerName: "server." + spec.region + ".nomad",   // NEW field (see below)
}
c, err := nomad.New(cfg)
```

**Additive change to the Foundation seam.** `internal/nomad.Config` + `TLSConfig` gain a
`TLSServerName string` field, plumbed into `api.TLSConfig.TLSServerName` in `New`. This is the only
change to the Foundation package and is purely additive (empty string preserves current behavior).

**Contract extension (honoring the Foundation existence-only-pin gotcha).** `contract.go` gains pins
for the new `api` surface this slice binds to — the ACL bootstrap call, leader/peer reads, and a
server-health read used for readiness/quorum:

- `(*api.ACLTokens).Bootstrap`, `api.ACLToken`
- `(*api.Status).Leader`, `(*api.Status).Peers`
- `(*api.Agent).Health` (or `(*api.Operator).RaftGetConfiguration` + `api.RaftConfiguration`)

Each new pin **must be backed by a real call** in the reconciler/client (method-expression pins guard
symbol *existence*, not *signature shape*; drift is only caught because the code actually calls them
with concrete arguments). Exact symbols are confirmed against the pinned `api` during implementation
and reflected in the client wrapper.

### 3.7 Rolling upgrades (KISS, quorum-safe)

No custom upgrade controller. Quorum safety is delegated to Kubernetes primitives:

- **StatefulSet `RollingUpdate`** — ordered, one pod replaced at a time.
- **PodDisruptionBudget `minAvailable = servers - 1`** (2 of 3) — voluntary disruptions never take a
  second server.
- **Raft-aware readiness probe** — the pod reports Ready only when the server has rejoined and quorum
  is intact (`GET /v1/agent/health?type=server`). Because the StatefulSet waits for pod *N* to be
  Ready before touching pod *N+1*, K8s never removes a second server until the first is healthy
  again.

This covers image/version bumps and config changes. Explicit leader step-down / drain orchestration
is a later hardening concern, not slice 2.

## 4. Definition of Done

- `make generate && make verify` green; `contract.go` compiles against the v2.0.4 pin with the new
  pins backed by real calls.
- A `NomadCluster` CR reconciles to a healthy **3/3** Raft cluster: StatefulSet Ready, PVs bound,
  anti-affinity honored, leader elected.
- mTLS, gossip encryption, and ACLs are enabled; the ACL management token is captured to a Secret;
  the operator's per-cluster `Client` authenticates against the in-cluster endpoint.
- The external RPC surface is exposed with correct per-server advertise addresses through the Gateway
  (both `Managed` and `Existing` modes reconcile correctly), and the HTTP route serves the API.
- Quorum-safe rolling update verified: a rollout replaces servers one at a time without dropping
  below quorum.
- **Manual verification (documented, not automated):** an out-of-cluster Nomad client configured with
  the Gateway RPC addresses registers and reaches `ready`.

## 5. Testing

- **Unit:** CRD defaulting/validation (incl. `servers` immutability, `rpcPorts` length == `servers`,
  gateway-mode field requirements); per-ordinal advertise-address rendering; Client construction from
  a CR (incl. `TLSServerName`); quorum math.
- **Envtest:** reconcile creates the expected StatefulSet / headless + per-pod + API Services / PDB /
  HTTP route / per-server `TCPRoute`s / Gateway (Managed) or the correct `parentRefs` (Existing);
  status phase transitions; the ACL-bootstrap-once ordering (token Secret written before the
  condition flips; no re-bootstrap when the condition is set).
- **Hermetic integration (extends Foundation's dev-agent test):** boot an ephemeral **ACL-enabled**
  Nomad v2.0.4, exercise `Bootstrap()` → capture token → construct the authenticated `Client` → read.
  This also closes Foundation open-item #1 (observe the real node-status value set) when a `nomad`
  v2.0.4 binary is present.
- **Manual/live:** the documented external client join.

## 6. Interaction with the slice roadmap

This slice produces the authenticated, reachable endpoint that slice 3 (`NomadNode`) and beyond
consume via the same per-cluster `Client`. It touches `cmd/main.go` for the first time (registers the
`NomadCluster` controller), adds `api/v1alpha1/nomadcluster_types.go`, an
`internal/controller/nomadcluster_controller.go`, and extends `internal/nomad` (the `TLSServerName`
field + the write/health surface + `contract.go` pins). It introduces no new heavyweight
dependencies: server-side `Jobs().ParseHCL` remains the job path for later slices; no `jobspec2`, no
`nomad-openapi`.

## 7. Explicit assumptions to verify at plan/implementation time

1. **The Cilium Gateway address is in-cluster routable** (⚠️ load-bearing). Inter-server Raft dials
   `advertise.rpc = gatewayAddress:port`; if the Gateway address is not reachable from pods, the
   cluster cannot form. Verify with a pod-to-gatewayAddress dial before relying on it; the
   LoadBalancer fallback (§3.4) is the escape hatch if this fails.
2. **The Cilium `GatewayClass` supports multiple TCP (L4) listeners** and `TCPRoute` on one Gateway
   (Managed mode), and exposes an address that can host `servers` distinct listener ports.
3. **A `nomad` v2.0.4 binary is available in CI/dev** to exercise the hermetic ACL test (still
   outstanding from Foundation; also closes open-item #1).
4. The exact `api` symbols for leader/peer/health reads (§3.6) are confirmed against the pinned `api`
   during implementation; the client wrapper and `contract.go` reflect the confirmed names.

## 8. Open questions carried forward (not blocking this design)

- Whether the HTTP route should terminate TLS at the Gateway (`HTTPRoute`, Gateway holds a cert) or
  pass through to preserve Nomad's end-to-end mTLS (`TLSRoute`). Default: **`TLSRoute` passthrough**,
  so `verify_https_client` remains meaningful end-to-end; revisit if a browser-friendly UI endpoint
  is wanted.
- Cert **rotation** mechanics (cert-manager renews the Secret; how the operator triggers a Nomad TLS
  reload without a full rollout) — deferred to hardening (slice 6).
