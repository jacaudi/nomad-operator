# External Access Modes (Gateway + LoadBalancer) — Design

**Type:** design · **Date:** 2026-07-11 (amended 2026-07-12 after SGE review) · **Status:** proposed
**Feature:** restructure `spec.gateway` into `spec.externalAccess { mode: Gateway | LoadBalancer }` and add a `LoadBalancer` external-access mode for single-node control planes.

> **Amended 2026-07-12** after an independent sr-go-engineer design review (verdict: no blocking issues; four important gaps folded). The review verified the networking model (§§2–4, §6) against the merged slice-2 code and found it sound; the amendments correct the *mechanism* descriptions to match the real code (there is no Go "advertise renderer" — advertise is written by the init-container shell script; the restructure has nil-deref and reconcile-partition implications the first draft glossed) and close the HTTP-cert gap in LoadBalancer mode. Changed sections: §4, §5.2, §7, §8, §11.

Follows slice 2 (NomadCluster HA control plane, merged `a1e4d6a`) and FR-1 (single-node `servers: 1`, `349f5cc`). Companion to FR-1: gives single-node clusters a way to expose the control plane to external edge agents **without a Gateway API controller**.

---

## 1. Goal

Let a `NomadCluster` expose its control plane to out-of-cluster edge agents via one of two mechanisms:

- **Gateway** (as built in slice 2) — a Gateway API `Gateway` (operator-Managed or user-Existing). Supports `servers: 1/3/5`. Requires a Gateway controller (Cilium, Envoy Gateway, …).
- **LoadBalancer** (new) — a single `type: LoadBalancer` Service. Requires only a LoadBalancer provider (which most clusters already have). **Scoped to `servers: 1`.**

The CRD field `spec.gateway` is restructured into `spec.externalAccess` so the two mechanisms are a clean discriminated union and future modes (NodePort, …) slot in additively.

`v1alpha1` is unreleased, so the breaking rename is acceptable.

---

## 2. Motivation — the north-south / east-west lens

Nomad control-plane traffic splits into two lenses, and the whole design turns on how each mode serves them.

- **North-south** (outside ↔ control plane): edge agents' RPC 4647 and operator/UI HTTP 4646. Every request is "reach *a* server" — whichever server answers forwards to the leader (Nomad RPC forwarding). **A single round-robin front end is correct.**
- **East-west** (server ↔ server): Raft consensus (RPC 4647) and Serf gossip (4648). Raft is **point-to-point** — server A must reach server B *specifically*. Serf already stays on the pod network (`advertise.serf = POD_IP`); Raft is the problem.

The knot: Nomad exposes a **single `advertise.rpc`** per server, used for **both** lenses (clients dial it; Raft peers learn each other's `advertise.rpc` via Serf). Clients *adopt* the discovered `advertise.rpc` and require it reachable (verified against Nomad 2.0 docs and reproduced in the slice-2 local e2e). So one address must satisfy both lenses.

**Consequence — east-west needs a per-server, individually-reachable address.** "Which server" can be encoded three ways:

| Encoding | Mode | Notes |
|---|---|---|
| **port** (one IP, N ports) | **Gateway** | `advertise.rpc = gwIP:<per-server-port>`; Gateway routes by port to the right pod |
| **name** (split-horizon DNS) | LB for HA — **not built** (see §6) | one name per server; pod IP inside, front-end outside |
| **IP** (N IPs) | per-server LB — **not built** | N LoadBalancer IPs |

**`servers: 1` has no east-west Raft at all**, so a single front end (one LB VIP) is a *complete* answer with no per-server encoding. That is exactly why LoadBalancer mode is scoped to single-node: it is the case where north-south is the *only* lens.

---

## 3. The Gateway is one Gateway, N listeners

A Gateway-mode cluster uses **one** `Gateway` object (one external IP). Per-server-ness is listeners/routes on that single Gateway, not one Gateway per server:

```
        ┌──────────── ONE Gateway (one external IP = gwIP) ────────────┐
edge   │  listener "http"  :4646 (TLS)  ─► TLSRoute ──► any server :4646 │
agents │  listener "rpc-0" :14647 (TCP) ─► TCPRoute rpc-0 ─► svc …-server-0-rpc ─► pod S0:4647
  +    │  listener "rpc-1" :24647 (TCP) ─► TCPRoute rpc-1 ─► svc …-server-1-rpc ─► pod S1:4647
Raft  │  listener "rpc-2" :34647 (TCP) ─► TCPRoute rpc-2 ─► svc …-server-2-rpc ─► pod S2:4647
peers  └───────────────────────────────────────────────────────────────┘
```

| Object | Count | Per-server |
|---|---|---|
| `Gateway` (the IP) | 1 | no |
| HTTP listener + TLSRoute | 1 | no |
| RPC listener (distinct port) + TCPRoute + per-pod backend Service | N | yes |

RPC 4647 is a raw multiplexed mTLS TCP stream (not SNI-routable), so the Gateway distinguishes servers by **listener port**, not hostname. East-west Raft rides the Gateway too: `S0 → gwIP:24647 → Gateway → S1` (deterministic by port) — which is why the **Gateway IP must be pod-routable** (existing slice-2 assumption). This section is unchanged from slice 2; it is documented here because the LB comparison depends on it.

Managed vs Existing (unchanged, moves under `externalAccess.gateway`):

| | Managed | Existing |
|---|---|---|
| Gateway object | operator creates + owns | user owns; operator never mutates it |
| Listeners (`http`, `rpc-0…N`) | operator defines | user must pre-define, **named** `http`/`rpc-<ordinal>` (operator routes attach by `sectionName`) |
| `gatewayClassName` | user supplies (`className`); class must exist | already on the user's Gateway |
| Routes + backend Services | operator creates + owns | operator creates + owns |

The `GatewayClass` points at a Gateway **controller** (Cilium/Envoy/…) that assigns the IP and does L4 forwarding — the operator only creates Gateway API objects, it does not provide the controller.

---

## 4. LoadBalancer mode (new)

A single `type: LoadBalancer` Service fronts the (single) server. North-south only; no east-west because `servers: 1`.

```
edge agents ─► LB VIP :4647 (RPC) ─► pod S0 :4647
            ─► LB VIP :4646 (HTTP) ─► pod S0 :4646
advertise.rpc  = <lb-ingress-addr>:4647
advertise.http = <lb-ingress-addr>:4646
advertise.serf = POD_IP   (in-cluster, unchanged)
```

**What the operator provisions in LoadBalancer mode:**
- **One `type: LoadBalancer` Service** (`<name>-lb`) selecting the server pods, exposing RPC `4647` and HTTP `4646`. Operator-owned (ownerRef). Optional `spec.loadBalancerClass` and `annotations` are applied to it.
- Everything shared with Gateway mode: StatefulSet, headless Service (Serf/`retry_join`), API ClusterIP Service (the in-cluster endpoint the operator's per-cluster Client uses), ConfigMap, PDB, gossip Secret, token Secret.
- **Not created in LoadBalancer mode:** any `Gateway`, `TLSRoute`, `TCPRoute`, or the per-pod RPC ClusterIP Services (those exist only as Gateway TCPRoute backends; the LB Service selects the pod directly).

**External address discovery + gate:** the operator reads the LB's assigned address from the Service `status.loadBalancer.ingress` (IP or hostname), exactly as Gateway mode reads `Gateway.status.addresses`. Until an ingress address exists, the cluster stays `Pending` (same gate as the Gateway-address gate). Issue-7's `Owns(&corev1.Service{})` means the operator automatically re-reconciles when the LB IP is assigned — no manual nudge.

**Advertise rendering:** there is no Go "advertise renderer" — the per-pod advertise stanza is written at boot by the init-container shell script (`initEntrypoint` in `resources_workload.go`), which reads two ConfigMap keys: `gateway_address` and `rpc_ports` (a space-separated list; the script indexes it by the pod ordinal). `buildConfigMap` currently populates `rpc_ports` from `spec.gateway.rpcPorts` and `renderConfig` folds the same list into the rollout hash. **LoadBalancer-mode reuse** therefore means: feed the LB ingress address as `gateway_address`, and have `buildConfigMap`/`renderConfig` **synthesize a single-element `rpc_ports = "4647"`** (the `gateway` block — and thus `rpcPorts` — is absent in LB mode). The shell script is unchanged (`ORD=0` selects the sole port; it already emits `advertise.http = <addr>:4646`, `advertise.rpc = <addr>:4647`, `advertise.serf = POD_IP`). No new renderer, but `buildConfigMap`/`renderConfig` gain a mode branch that produces the single RPC port instead of dereferencing `spec.gateway`.

**mTLS/cert — RPC is settled, HTTP needs a note.** Nomad **RPC** is role-verified (`server.<region>.nomad`), so the LB address does **not** need to be in the cert for edge agents to join over RPC 4647 — the existing `certSecretRef` cert works as-is for the RPC lens. **HTTP is different:** the operator advertises `http = <lb-ingress>:4646` and exposes 4646 on the LB Service, and HTTP is verified against the *dialed address*, not a role. In Gateway mode the cert covers `httpHostname` (a required SAN, `TLSSpec` in `nomadcluster_types.go`); LoadBalancer mode has no `httpHostname`. So an external UI/CLI client dialing `https://<lb>:4646` hits a hostname mismatch unless it passes `-tls-server-name server.<region>.nomad` (which the cert already covers) or the LB address is added to the cert SANs. **Decision (KISS/YAGNI):** do not add an `httpHostname` field to the `loadBalancer` block yet — document in the runbook that external HTTP/UI access in LB mode requires `-tls-server-name server.<region>.nomad`. Add the field additively (No-Wall) only if a real "native-hostname UI over the LB" need appears. The operator's own in-cluster client is unaffected (it already sets `TLSServerName` in `clientFor`).

---

## 5. CRD changes

### 5.1 Shape

```
spec:
  servers: 1|3|5                       # unchanged (FR-1)
  externalAccess:                      # NEW — replaces spec.gateway
    mode: Gateway | LoadBalancer       # discriminated union
    gateway:                           # required iff mode == Gateway
      mode: Managed | Existing
      className: <string>              # required iff gateway.mode == Managed
      ref: { name, namespace }         # required iff gateway.mode == Existing
      rpcPorts: [<int32>, ...]         # len == servers; immutable
      httpHostname: <string>
    loadBalancer:                      # optional even when mode == LoadBalancer
      loadBalancerClass: <string>      # optional
      annotations: { <k>: <v>, ... }   # optional — applied to the LB Service (cloud LB config)
```

`spec.gateway` (slice 2) → moves verbatim under `spec.externalAccess.gateway` (same fields: `mode`, `className`, `ref`, `rpcPorts`, `httpHostname`).

### 5.2 CEL validation

- `externalAccess.mode`: `Enum=Gateway;LoadBalancer`. **Immutable** (bears the networking identity, like `rpcPorts`).
- **`externalAccess.mode == LoadBalancer` ⇒ `servers == 1`.** With `servers` immutable, a 3/5-server cluster can never be LoadBalancer, and a 1-server cluster may use either mode.
- `externalAccess.mode == Gateway` ⇒ `externalAccess.gateway` present; `size(externalAccess.gateway.rpcPorts) == servers` (the current cross-check, re-scoped under the Gateway branch).
- `gateway` union: `className` required when `gateway.mode == Managed`; `ref` required when `gateway.mode == Existing` (unchanged, re-homed).
- `externalAccess.mode == LoadBalancer` ⇒ `externalAccess.gateway` must be absent (and vice-versa) — enforce the union so only the active mode's block is set.

**CEL placement (verified against the current markers):** the two cross-field rules — `mode==LoadBalancer ⇒ servers==1` and `mode==Gateway ⇒ size(gateway.rpcPorts)==servers` — reference both `self.servers` and `self.externalAccess.*`, so they live on `NomadClusterSpec` (root), **replacing** the current root rule `size(self.gateway.rpcPorts) == self.servers` (`nomadcluster_types.go:101`). Both must be written as **guarded implications** (e.g. `self.externalAccess.mode != 'Gateway' || size(self.externalAccess.gateway.rpcPorts) == self.servers`) so CEL never dereferences `gateway.rpcPorts` when the gateway block is absent. The `rpcPorts` immutability rule and the `className`/`ref` union rules move **verbatim** onto the re-homed `GatewaySpec` (they only reference `self` within that struct). No CEL feasibility blocker.

---

## 6. Not built: split-horizon DNS (documented rationale)

"Single LoadBalancer for **HA**" would require **split-horizon DNS**: per-server names resolving to the pod IP inside the cluster (east-west Raft goes pod-direct) and to the LB VIP outside (north-south round-robins). A wildcard (`*.nomad.example.com → LB`) makes the *external* side one record, and a single CoreDNS rewrite makes the *internal* side one rule — but it still couples the operator to cluster-DNS manipulation, external-DNS management, and dynamic pod-IP tracking.

**It is deliberately not built**, for two reasons:
1. **KISS.** It is a LoadBalancer-only crutch for the narrow "HA without a Gateway controller" case, and its cost (moving parts, external coupling) is high for that niche.
2. **It does not unify with the Gateway.** The Gateway discriminates by **port**; DNS resolves names→IPs and cannot remap a **port**, and `advertise.rpc` carries a single `host:port`. The Gateway's external port (`14647`) differs from the pod port (`4647`) by design, so split-horizon names cannot bridge it. The mechanisms are irreducibly different, so enforcing split-horizon "for both modes" is impossible — which is the signal to drop it entirely.

**HA is served by Gateway mode**, which already solves east-west via per-server ports with no DNS. If a real "HA on a plain LoadBalancer" need ever appears, this section is the starting point (add it additively as a `loadBalancer.dns` sub-block — No-Wall).

---

## 7. Reconcile changes

- `Reconcile` dispatches on `externalAccess.mode` for the **address gate**:
  - `Gateway` → existing `ensureGateway` (Managed/Existing), `gwAddr` from Gateway status.
  - `LoadBalancer` → new `ensureLoadBalancer`: apply the `type: LoadBalancer` Service, read `status.loadBalancer.ingress`, return `(addr, ready)`.
- **The step-3 provisioning block must be partitioned by mode** (it is not today — the routes and per-pod Services are provisioned *inline in `Reconcile`* after `ensureGateway` returns, `nomadcluster_controller.go:128-140`, mixed with the shared objects):
  - **shared, always:** ConfigMap, headless Service, API Service, StatefulSet, PDB.
  - **Gateway-only:** `buildTLSRoute`, `buildTCPRoutes`, and the per-pod `buildPodService` loop.
  - **LoadBalancer-only:** the new `buildLoadBalancerService`.
  This is a restructure of the reconcile body (shared vs Gateway-only vs LB-only), not a one-line `ensureGateway`→`ensureLoadBalancer` swap.
- **Read sites of `spec.gateway` move to `spec.externalAccess.gateway`, which is now an optional pointer (nil in LB mode) — so the move is not purely mechanical; three sites need nil guards:**
  - `buildConfigMap` and `renderConfig` read `spec.gateway.rpcPorts` on **every** mode → must synthesize the single RPC port in LB mode (per §4) instead of dereferencing.
  - **`gatewayToClusters` (`nomadcluster_controller.go:255-256`) is the sharp one:** it iterates **all** `NomadCluster`s on **any** Gateway event and reads `nc.Spec.Gateway.Ref/.Mode`. With even one LB-mode cluster present, `ExternalAccess.Gateway == nil` → the map func panics on every Gateway event, **crash-looping the controller for the whole cluster set.** Guard with `if nc.Spec.ExternalAccess.Gateway == nil { continue }` before the dereference.
- **Status naming:** in LB mode the discovered address currently lands in `status.gatewayAddress` gated by `CondGatewayReady` — a "gateway" name for a non-Gateway address. Since `v1alpha1` is unreleased, rename to mode-neutral `status.externalAddress` + an `ExternalAccessReady` condition now (free now, painful later). Gateway mode keeps the same semantics under the neutral name.
- `SetupWithManager` (post Issue-7) already `Owns(&corev1.Service{})`, so the status-only `status.loadBalancer.ingress` write re-enqueues the owning cluster and the `Pending`-until-ingress gate re-fires — reactivity is free, no Watch/predicate needed. **Do not add a `GenerationChangedPredicate` to `Owns(&corev1.Service{})`** — a status-only update carries no generation bump, so a generation filter would silently break LB-address reactivity. Existing-mode Gateway watch unchanged.
- Phase machine, gossip, cert gate, ACL bootstrap, teardown retention — all unchanged; they are external-access-agnostic.

---

## 8. Migration

`v1alpha1` unreleased ⇒ breaking change, no conversion webhook needed:
- Rename the Go types (`GatewaySpec` stays, gains an `ExternalAccessSpec` parent — with `Gateway *GatewaySpec` optional + `LoadBalancer *LoadBalancerSpec` optional), update `NomadClusterSpec` (`Gateway GatewaySpec` → `ExternalAccess ExternalAccessSpec`).
- Regenerate CRD + deepcopy (`make manifests generate`).
- Update every reconcile read site (see §7's nil-guard list) and the mode-neutral status field.
- **The real fixture churn is the test suite, not the sample CR** (`config/samples/nomad_v1alpha1_nomadcluster.yaml` is empty `spec: # TODO(user)` scaffolding — filling it with a concrete `externalAccess` example is a docs task, not a migration). The specs that build `nc.Spec.Gateway = GatewaySpec{...}` and must move to `nc.Spec.ExternalAccess.Gateway` are: `resources_gateway_test.go`, `nomadcluster_controller_test.go`, `gatewaywatch_test.go`. Add new LoadBalancer-mode specs (§9).
- Update `docs/runbooks/nomadcluster.md` (add the LB-mode section + the `-tls-server-name` HTTP note from §4).

**Plan-time safeguards (from the SGE double-check):**
- **Grep `Spec.Gateway` exhaustively** before wiring, and confirm each hit is either mode-gated (Gateway-only builders in `resources_gateway.go`, which only run in Gateway mode after the §7 partition) or nil-guarded (the three shared/LB-path sites in §7). The read-site list in §7 is derived from a read of the touched files, not an exhaustive grep — the grep makes "no missed deref" certain.
- **Thread the LB ingress address into `renderConfig` as its `gatewayAddress` arg** (not only into `buildConfigMap`), so the `gw=` term in the rollout hash covers an LB-IP change — a changed LB address must re-render `advertise.rpc` and therefore must roll the StatefulSet.
- Also fix the `TLSSpec` SAN doc comment (`nomadcluster_types.go:66`) that names `spec.gateway.httpHostname` (Gateway-mode-only guidance now).

---

## 9. Testing

- **CEL (envtest):** `LoadBalancer ⇒ servers==1` accepted; `LoadBalancer` + `servers:3` rejected; `mode` immutable; Gateway-mode `rpcPorts==servers` still enforced under the new path; union exclusivity (only the active mode's block).
- **LoadBalancer reconcile (envtest):** create a `servers:1` LoadBalancer-mode cluster; the operator applies the LB Service and stays `Pending` until its `status.loadBalancer.ingress` is stubbed; then provisions and reaches `Ready` with `advertise.rpc = <lb>:4647`; no Gateway/route/per-pod-Service objects created.
- **Builders (unit):** `buildLoadBalancerService` (selector, RPC+HTTP ports, class/annotations); advertise renderer emits `:4647` in LB mode.
- **Regression:** all existing Gateway-mode specs pass under the re-homed path.
- **Crash-loop guard (unit):** `gatewayToClusters` returns cleanly (no panic) when the cluster list contains a LoadBalancer-mode cluster (`ExternalAccess.Gateway == nil`) — the §7 nil guard. Similarly `buildConfigMap`/`renderConfig` return the single `:4647` port for a LB-mode cluster without dereferencing an absent gateway block.
- **Local e2e (optional, out of plan):** re-run the slice-2 single-node e2e in LoadBalancer mode on kind + a LB provider (metallb) instead of the Gateway stub + socat proxy.

---

## 10. Interactions

- **FR-1** (`servers:1`): LoadBalancer mode is its external-access companion.
- **Issue 7** (`Owns(Service)`): gives LB-IP-assignment reactivity for free.
- **Gateway mode**: unchanged in behavior; only re-homed under `externalAccess.gateway`.

---

## 11. Open items / assumptions

- Assumes a LoadBalancer provider exists in the cluster (cloud LB, metallb, Cilium LBIPAM). If none, the LB Service stays address-less and the cluster stays `Pending` (same failure shape as a missing Gateway controller) — document in the runbook.
- `annotations` pass-through is untyped by design (cloud-specific); no validation beyond map[string]string. It already covers the **static-IP** case (modern providers request a fixed address via annotation; `Service.spec.loadBalancerIP` is deprecated), so no dedicated IP field is needed.
- **Two typed `Service` fields are deliberately out of reach of `annotations` and deferred (YAGNI), recorded so the deferral is conscious:** `externalTrafficPolicy: Local` (source-IP preservation / single hop) and `loadBalancerSourceRanges` (CIDR firewall). The latter is genuine defense-in-depth for an internet-facing edge control plane whose only gate is mTLS — a plausible near-term follow-up. Add either additively to the `loadBalancer` block when a real need appears (No-Wall).
- Whether `externalAccess.mode` should be immutable vs. mutable-with-rollout: chosen **immutable** for KISS (mode switching would tear down/rebuild the entire external surface — Gateway+routes ↔ LB Service — and change the advertised address, forcing every edge agent to re-learn `advertise.rpc`). **Tension to note:** `servers: 1` legitimately supports *both* modes, so "I'm on Gateway at `servers: 1` and want to drop the Gateway-controller dependency for a plain LB" is a plausible real want that immutability forecloses. Keep immutable for v1; if that switch-in-place need materializes, a mutable-with-rollout path is a clean additive follow-up.
