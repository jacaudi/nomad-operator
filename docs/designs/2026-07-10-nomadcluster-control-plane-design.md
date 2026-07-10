# Design — `nomad-operator` Control Plane Slice (`NomadCluster`)

| | |
|---|---|
| **Component** | `nomad-operator` — a Kubernetes operator that provisions and manages a Nomad control plane on K8s, with workloads on edge clients |
| **This document** | Design for the **Control Plane slice** (slice 2 of 6) — the first slice that stands up real Nomad servers and produces the authenticated endpoint later slices consume |
| **Target runtime** | Nomad **v2.0.4**; K8s 1.28+ with Gateway API + Cilium (LBIPAM + Gateway); Go **1.26.4** |
| **Status** | Approved 2026-07-10; amended 2026-07-10 after an independent sr-go-engineer (Fable) design review — ready for implementation plan |
| **Review amendments** | Folded review findings: bootstrap-deadlock fix (`podManagementPolicy: Parallel` + `publishNotReadyAddresses`, §3.2); ACL made idempotent via `BootstrapOpts` + Secret-as-source-of-truth + corrected reset runbook (§3.3); config-hash rollout + `rpcPorts` immutable (§3.1/§3.2/§3.7); `httpHostname`/`localhost` cert SANs (§3.3); injectable client factory + Gateway-API experimental-channel CRDs (§3.6/§5/§7); deletion/teardown retention (§3.8); plus minors M2/M4/M5/M8/M9 |
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
    rpcPorts: [14647, 24647, 34647]   # len == servers; IMMUTABLE (bears Raft identity); one port/server
    httpHostname: nomad.example.com   # hostname for the HTTP/TLS route to the API/UI (must be a cert SAN)

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
  one server per node, so a single node loss never costs quorum. This requires **≥ `servers`
  schedulable nodes**; with fewer, pods stay `Pending` (surfaced as a clear condition, not a silent
  hang).
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
  read CR status directly). Gossip **key material is never in the ConfigMap** — it is a Secret mount
  (§3.3); the ConfigMap carries only non-secret wiring (addresses, ports, region/DC).
- **Bootstrap reachability — two non-default knobs are REQUIRED, or the cluster deadlocks.** With
  `bootstrap_expect = servers` and a leader-gated readiness probe (§3.7), no pod is Ready until a
  quorum forms, and no quorum can form unless pre-Ready pods can reach each other:
  - **`podManagementPolicy: Parallel`** on the StatefulSet — the default `OrderedReady` never creates
    pod-1 until pod-0 is Ready, which can never happen alone. (This is independent of
    `updateStrategy`, so the ordered rolling update in §3.7 still holds.)
  - **`publishNotReadyAddresses: true`** on the headless Service **and** the per-pod ClusterIP
    Services — otherwise NotReady endpoints are withheld, so Serf `retry_join` can't resolve peers and
    the `TCPRoute` backends are empty (Envoy has nothing to forward Raft/RPC to) until quorum exists,
    which it never will. This is exactly what the Consul/Vault Helm charts set for the same reason.
- **ConfigMap changes must roll the StatefulSet.** The init container reads the ConfigMap only at pod
  start, so a ConfigMap update alone changes nothing. The operator therefore stamps a **hash of the
  rendered config into a pod-template annotation**; when `gatewayAddress`/ports/region change, the
  hash changes, the pod template changes, and the StatefulSet rolls (quorum-safe, per §3.7). Without
  this, servers advertise stale addresses indefinitely (e.g. after an LBIPAM address reassignment).

### 3.3 Security (hybrid)

Three distinct materials, each owned by whoever is best at it:

- **mTLS — cert-manager.** The user creates a cert-manager `Certificate` (referencing their issuer)
  whose Secret is named by `spec.tls.certSecretRef`. Required SANs:
  - `server.<region>.nomad` and `client.<region>.nomad` — the role/region names Nomad's own mTLS
    verifies (the operator's `api.Client` sets `TLSServerName = server.<region>.nomad` and presents
    the client cert; it does **not** depend on the dialed address appearing in a SAN);
  - **`spec.gateway.httpHostname`** — so a standard TLS client (browser/`curl`/API SDK) dialing the
    external HTTP front door verifies natively. With `TLSRoute` passthrough (§8 default) the route
    matches on SNI == `httpHostname`, so the cert **must** carry it or verification/routing conflict
    (a client that overrides `tls_server_name=server.<region>.nomad` sends that as SNI and misses the
    route — documented caveat for the `nomad` CLI hitting the external endpoint);
  - **`localhost` / `127.0.0.1`** — so in-pod CLI/debug (`nomad`/`curl` exec'd in a server pod) works,
    per Nomad's TLS guide.

  The operator mounts the Secret into the pods and enables
  `tls { http = true, rpc = true, verify_server_hostname = true, verify_https_client = true }`.
- **Gossip — operator-generated.** On first reconcile the operator generates a 32-byte base64 key,
  stores it in the Secret named by `status.gossipKeySecretRef`, and mounts it as
  `server { encrypt = "…" }`. The same key is used by all servers in the region. (Key **rotation** is
  out of scope.)
- **ACL — operator-bootstrapped, made idempotent via an operator-supplied token.** Servers run with
  `acl { enabled = true }`. The naive `ACLTokens().Bootstrap()` returns the management secret-ID
  **once only**, opening a lost-token window (crash between the call and the Secret write = the token
  is irrecoverable). The pinned `api` exposes `ACLTokens().BootstrapOpts(bootstrapToken, q)` (an
  operator-supplied bootstrap token, supported since Nomad 1.1), which removes that window entirely:

  1. Generate the management token value and **write the token Secret first**
     (`status.bootstrapTokenSecretRef`).
  2. After the cluster reaches quorum and elects a leader, call `BootstrapOpts(<that token>, …)`.
  3. Treat "already bootstrapped" as success **iff** our Secret already holds the active token; the
     step is now safely retryable — a crash-and-retry re-submits the same token.

  **Source of truth is the Secret, not the condition.** Re-bootstrap is gated on the existence of the
  deterministically-named token Secret, never on the `ACLBootstrapped` status condition (status is not
  durable — a restore or status wipe must not trigger a re-bootstrap, and the token can't be
  re-observed). The condition is derived from the Secret, never load-bearing.

  **Documented recovery (corrected — there is no `nomad acl bootstrap-reset` command).** If the token
  Secret is ever lost after a successful bootstrap: attempt a bootstrap to read the **reset index**
  from the error, write that index to `<data_dir>/server/acl-bootstrap-reset` **on the leader pod**
  (exec into it / its PVC), then let the reconciler re-bootstrap with a freshly generated token. This
  is manual and invasive; it is spelled out in the runbook.

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
| Listeners on `rpcPorts` + HTTP | Operator owns and reconciles them | **User pre-provisions** them; reconcile sets `GatewayReady=False`/`Pending` with a precise message if a required listener/port is missing, **or** if the listeners' `allowedRoutes.namespaces` do not admit the CR's namespace (cross-namespace `parentRefs` are otherwise silently rejected) |
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
server-health read used for readiness/quorum (all confirmed present in the pinned `api`
`v0.0.0-20260707172059-5b83b133998a`):

- `(*api.ACLTokens).BootstrapOpts`, `api.ACLToken` (operator-supplied token — see §3.3)
- `(*api.Status).Leader`, `(*api.Status).Peers`
- `(*api.Agent).Health` + `api.AgentHealthResponse` (or `(*api.Operator).RaftGetConfiguration` +
  `api.RaftConfiguration`)

Each new pin **must be backed by a real call** in the reconciler/client (method-expression pins guard
symbol *existence*, not *signature shape*; drift is only caught because the code actually calls them
with concrete arguments).

**Two wrapper notes.** (1) `Status().Leader`, `Status().Peers`, and `Agent().Health` take no
`QueryOptions`, so `ctx` cannot be threaded through them — the same quirk already documented for
`Ping` in `internal/nomad/client.go`; keep the uniform `ctx`-accepting signatures and note it.
(2) `Status().Leader()` returns `"ip:port"`; mapping it to a friendly `status.leader`
(e.g. `prod-server-1.global`) requires a port→ordinal lookup via `rpcPorts` — a small documented
helper, not guesswork.

**Injectable client factory (a real seam with a present consumer — the envtest).** envtest runs no
Nomad, so the reconciler must construct its `Client` through an injectable factory field
(`func(nomad.Config) (NomadReader, error)`, defaulting to `nomad.New`) that tests replace with a fake.
This is not speculative abstraction — §5's phase-transition and ACL-bootstrap-once tests are the
consumer that exists now. The consumer-side interface (`NomadReader`/`NomadBootstrapper`) is defined
at the reconciler per the Go interface-at-consumer convention.

### 3.7 Rolling upgrades (KISS, quorum-safe)

No custom upgrade controller. Quorum safety is delegated to Kubernetes primitives:

- **StatefulSet `RollingUpdate`** — ordered, one pod replaced at a time.
- **PodDisruptionBudget `minAvailable = servers - 1`** (2 of 3) — voluntary disruptions never take a
  second server.
- **Raft-aware readiness probe** — the pod reports Ready only when the server has rejoined and a
  leader is known (`GET /v1/agent/health?type=server` is healthy ⇔ leader known). Because the
  StatefulSet waits for pod *N* to be Ready before touching pod *N+1*, K8s never removes a second
  server until the first is healthy again.
- **Liveness, if any, is process-level — NOT the leader-gated check.** A leader-gated liveness probe
  would fail *all* servers during a quorum loss and restart-storm them, which makes Raft recovery
  strictly worse. Liveness checks only that the agent process is up.

This covers image/version bumps. It also covers **config changes**, but only via the config-hash
pod-template annotation from §3.2 — a ConfigMap edit alone does not roll the StatefulSet. Explicit
leader step-down / drain orchestration is a later hardening concern, not slice 2.

**Observability caveat (M9).** Because the in-cluster API ClusterIP Service is Ready-gated, a full
quorum loss empties it, so the operator observes `Degraded` via read *errors*, not health reads. This
is acceptable; if richer signal is wanted, the operator can poll the per-pod Services (which carry
`publishNotReadyAddresses`, §3.2).

### 3.8 Deletion & teardown

On `NomadCluster` delete, ownerReferences cascade-delete the operator-created workloads and networking
(StatefulSet, Services, PDB, ConfigMap, HTTP route, `TCPRoute`s, and — in Managed mode — the dedicated
Gateway). Two things are **deliberately retained** and must not be swept by the cascade:

- **PVCs (Raft state).** `volumeClaimTemplates` PVCs are retained by default (StatefulSet does not
  delete them); this is intentional — accidental deletion is data loss. Documented, not automated.
- **The token & gossip Secrets.** The ACL management-token Secret is the **only copy of an
  unrecoverable credential**; it (and the gossip key) are retained unless the user explicitly opts
  into cleanup. A finalizer is **not** used to auto-purge them.

Existing-mode Gateways are never touched (the operator only owns the Routes/backends it created).

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
- **Envtest:** reconcile creates the expected StatefulSet (with `podManagementPolicy: Parallel` +
  config-hash annotation) / headless + per-pod + API Services (with `publishNotReadyAddresses`) / PDB /
  HTTP route / per-server `TCPRoute`s / Gateway (Managed) or the correct `parentRefs` (Existing);
  status phase transitions; ACL-bootstrap ordering (**token Secret written before `BootstrapOpts`;
  Secret existence — not the condition — gates re-bootstrap**). Two envtest prerequisites: (i) the
  injectable Nomad-client factory (§3.6) is stubbed with a fake, since envtest runs no Nomad and no
  pod/Gateway controllers (the operator manually stubs the Gateway's assigned address); (ii) the
  **Gateway API CRDs must be installed into envtest, from the experimental channel** — `TCPRoute` and
  `TLSRoute` are `v1alpha2` experimental-channel types absent from the standard install.
- **Hermetic integration (extends Foundation's dev-agent test):** boot an ephemeral **ACL-enabled**
  Nomad v2.0.4, exercise `BootstrapOpts()` → capture token → construct the authenticated `Client` →
  read. This also closes Foundation open-item #1 (observe the real node-status value set) when a
  `nomad` v2.0.4 binary is present.
- **Manual/live:** the documented external client join.

## 6. Interaction with the slice roadmap

This slice produces the authenticated, reachable endpoint that slice 3 (`NomadNode`) and beyond
consume via the same per-cluster `Client`. It touches `cmd/main.go` for the first time (registers the
`NomadCluster` controller), adds `api/v1alpha1/nomadcluster_types.go`, an
`internal/controller/nomadcluster_controller.go`, and extends `internal/nomad` (the `TLSServerName`
field + the write/health surface + `contract.go` pins). **One new direct dependency** is warranted by
a present requirement: `sigs.k8s.io/gateway-api` (typed `Gateway`/`TCPRoute`/`TLSRoute`/`HTTPRoute`),
pinned to a version whose **experimental channel** carries `TCPRoute`/`TLSRoute` (they are not in the
standard channel). No other additions: server-side `Jobs().ParseHCL` remains the job path for later
slices; no `jobspec2`, no `nomad-openapi`.

## 7. Explicit assumptions to verify at plan/implementation time

1. **The Cilium Gateway address is in-cluster routable** (⚠️ load-bearing). Inter-server Raft dials
   `advertise.rpc = gatewayAddress:port`; if the Gateway address is not reachable from pods, the
   cluster cannot form. Verify with a pod-to-gatewayAddress dial before relying on it; the
   LoadBalancer fallback (§3.4) is the escape hatch if this fails.
2. **The Cilium `GatewayClass` supports multiple TCP (L4) listeners** and `TCPRoute` on one Gateway
   (Managed mode), and exposes an address that can host `servers` distinct listener ports. This also
   means the **Gateway API experimental-channel CRDs** (`TCPRoute`/`TLSRoute`, `v1alpha2`) must be
   installed in the target cluster — a real deploy-time prerequisite, not only an envtest one.
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
