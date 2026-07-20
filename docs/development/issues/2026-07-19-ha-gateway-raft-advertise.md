# HA (Gateway mode) never forms raft quorum — `advertise.rpc` points at an unreachable address

- **Severity:** Critical (HA control plane unusable in Gateway mode) · **Area:** controller / Nomad server config rendering
- **Source:** live kind end-to-end run, 2026-07-19.
- **Status:** RESOLVED — 2026-07-19, commits `7c8cb4b` (fix) + `4bc0e1f` (test guard), merged fast-forward to local `main`.

## Resolution

Fixed by making `advertise.rpc` mode-aware in the init-container overlay
(`internal/controller/resources_workload.go`): a multi-voter raft (Gateway mode,
`servers` 3/5) now advertises `${POD_IP}:4647` (pod-network, the actually-bound
RPC port), while a single voter (`servers == 1`, LoadBalancer/single-node) keeps
the external-stable `${GW}:${RPCPORT}` address (slice-6b wedge protection). The
predicate keys on `servers == 1` — the wedge risk is a property of voter count,
not external-access mode, and this is correct for every CRD-legal `(mode, servers)`
combination (Gateway+{1,3,5}, LoadBalancer+1). Independently reviewed
*Ready-to-merge* (0 Critical/0 Important) and proven by a fresh, operator-driven
3-server cluster reaching `Ready` + `QuorumHealthy=True`.

## Problem

In Gateway external-access mode with `servers: 3` (or 5), the `NomadCluster` never
forms raft quorum — it stalls at `phase: Bootstrapping` / `QuorumHealthy=False
(NoLeader)` indefinitely. All three server pods run but stay `0/1` (readiness is
leader-gated).

Root cause: the init overlay rendered

```hcl
advertise {
  http = "${GW}:4646"
  rpc  = "${GW}:${RPCPORT}"   # GW = gateway/LB VIP, RPCPORT = per-ordinal EXTERNAL port (14647/24647/34647)
  serf = "${POD_IP}"
}
```

but every server pod binds RPC on the default **4647** (there is no `ports{rpc}`
override). Nomad addresses a *remote* peer as **(that peer's serf IP = POD_IP) +
(its advertised RPC port)** = `POD_IP:24647` — which nothing listens on:

```
raft: entering follower state: Node at 10.244.3.7:24647
[ERROR] nomad: failed to confirm peer status: peer=nomad-server-1.global
        error="dial tcp 10.244.3.7:24647: connect: connection refused"
```

→ no leader, forever. The external edge path is fine (`LB_IP:24647` → Envoy
TCPRoute → pod `4647`); only *intra-cluster* server-to-server raft is broken.

## Why it went unnoticed

The controller tests fake the Nomad API (envtest), and no test ever drove a real
3-server HA Gateway cluster to quorum against a live Nomad. `servers: 1` is
unaffected (a single voter self-elects with no peer dial), which is why the
single-node/LoadBalancer path always worked and this HA-to-quorum path was never
validated.

## Empirical validation

On a live kind cluster stuck in exactly this state, hand-patching the overlay to
`advertise.rpc = ${POD_IP}:4647` and fresh-restarting the servers elected a leader
and formed quorum in ~16 s (raft peers became pod IPs on 4647, zero
connection-refused). The committed fix reproduces this operator-driven.

## Fix / test

- `internal/controller/resources_workload.go` — `rpcAdvertiseStrategy` (keyed on
  `servers == 1`) selects a `rpc_advertise` ConfigMap value; the entrypoint branch
  emits `${POD_IP}:4647` (pod) or `${GW}:${RPCPORT}` (external).
- `internal/controller/resources_workload_test.go` — asserts the per-mode rendered
  overlay (Gateway/HA → pod-network; servers:1 → external-stable), plus a guard
  tying the shell branch literal to the Go const.
- `serf`, `http`, the external per-ordinal RPC listeners / TCPRoutes / per-pod
  Services, container ports, and the LoadBalancer path are unchanged.

## Known limitation (pre-existing, non-blocking)

The config hash does not cover `entrypoint.sh` / `rpc_advertise`, so activating
this fix on an *already-deployed, already-broken* HA cluster does not auto-roll the
StatefulSet — pods pick up the corrected overlay on their next restart
(`kubectl rollout restart`). A brand-new deploy is unaffected.
