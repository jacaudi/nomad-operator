# Known Issues — deferred non-blocking items

These are non-blocking follow-ups deferred out of the slice-2 (NomadCluster) merge
(`a1e4d6a`). Both the per-task reviews and the independent whole-branch review agreed
none block merge. Each entry is written to be portable to a GitHub issue verbatim once
`github.com/jacaudi/nomad-operator` exists as a repository.

Source: slice-2 whole-branch review, 2026-07-11.

---

## 1. `status.quorum` is fabricated `N/N`, not measured

- **Severity:** Minor · **Area:** reconciler / status
- **Location:** `internal/controller/nomadcluster_controller.go` (`bootstrapAndReady`, the
  `nc.Status.Quorum = fmt.Sprintf("%d/%d", servers, servers)` line, ~:219)
- **Problem:** `status.quorum` is set to `"<servers>/<servers>"` (e.g. `3/3`) whenever a
  leader exists, without counting healthy peers. A leader-with-2-of-3-up cluster still
  reports `3/3`, so the field is misleading.
- **Why deferred:** real peer counting requires `Status().Peers()` and `status.members`,
  which the design explicitly defers to slice 6 (hardening). The DoD only requires
  `leader`/`quorum` be populated.
- **Proposed fix:** in slice 6, populate `status.quorum` from the actual peer set
  (`Status().Peers()`), alongside `status.members` and the friendly-leader-name mapping.

## 2. Two `golangci-lint` findings (prealloc, unparam)

- **Severity:** Minor · **Area:** lint / cleanup
- **Locations:**
  - `internal/controller/resources_gateway.go:30` — `prealloc`: `listeners` slice should be
    preallocated with capacity `1 + len(nc.Spec.Gateway.RPCPorts)`.
  - `internal/controller/security_test.go:58` — `unparam`: `makeCertSecret`'s `name`
    parameter always receives `"nomad-tls"`.
- **Problem:** `make lint` reports these two findings. Lint is not part of the acceptance
  build gate (`make manifests generate fmt vet` + `make test`), so they did not block merge.
- **Proposed fix:** preallocate the `listeners` slice; drop or use the `makeCertSecret`
  `name` parameter. Trivial, no behavior change.

## 3. Unused `NomadOps` interface methods (`Ping`, `ServerHealthy`)

- **Severity:** Minor · **Area:** API surface / YAGNI
- **Location:** `internal/controller/nomadcluster_controller.go` (`NomadOps` interface, ~:42-44)
- **Problem:** `NomadOps.Ping` and `NomadOps.ServerHealthy` are never called by the
  reconciler. They are dead interface surface.
- **Note:** `(*nomad.Client).ServerHealthy` (`internal/nomad/client.go`) must remain — it
  backs the `(*api.Agent).Health` `contract.go` pin via a real call. Only the *interface*
  members are unused; trimming the interface is safe, the concrete method stays.
- **Proposed fix:** trim `NomadOps` to what the reconciler uses (`Leader`, `ACLBootstrap`);
  re-add methods when a consumer actually calls them.

## 4. Redundant gossip Secret mount on the main container

- **Severity:** Minor · **Area:** workload builder
- **Location:** `internal/controller/resources_workload.go` (main `nomad` container volume
  mounts, ~:212)
- **Problem:** the `gossip` Secret is mounted read-only at `/nomad/gossip` on the main
  `nomad` container, but the encrypt key is baked into `overlay.hcl` by the **init**
  container; the main container never reads `/nomad/gossip`. Harmless but dead.
- **Proposed fix:** remove the gossip mount from the main container (keep it on the init
  container). Add/confirm a builder test that the main container has no `/nomad/gossip` mount.

## 5. `Ready`→`Pending` flap on a transient cert/gateway read

- **Severity:** Minor · **Area:** reconciler robustness
- **Location:** `internal/controller/nomadcluster_controller.go` (cert gate ~:92-96 and
  gateway gate ~:103-107)
- **Problem:** the cert and gateway gates set `Phase = Pending` and return early if the cert
  Secret or gateway address read momentarily fails — even for a `Ready`/`Degraded` cluster.
  In Existing mode a shared-Gateway blip could flap a healthy cluster to `Pending`.
- **Proposed fix:** don't demote a `Ready`/`Degraded` cluster to `Pending` on a transient
  read; distinguish "never provisioned" from "already provisioned, transient dependency
  blip" (e.g. only gate to `Pending` when phase is empty/`Pending`, mirroring the
  Bootstrapping-seed guard added for the Ready→Degraded fix).

## 6. Existing-mode `GatewayReady=False` reason is imprecise

- **Severity:** Minor · **Area:** Existing-mode diagnostics
- **Location:** `internal/controller/nomadcluster_controller.go` (gateway gate condition,
  ~:105) and `ensureExistingGateway` in `internal/controller/resources_gateway.go`
- **Problem:** all Existing-mode verification failures (Gateway not found, missing/misnamed
  listener, namespace not admitted, no address yet) collapse into a single generic
  `GatewayReady=False` / `"WaitingForAddress"` reason. Operators can't tell which prerequisite
  failed from status alone.
- **Mitigation in place:** documented as a manual diagnosis checklist in
  `docs/runbooks/nomadcluster.md` §6.
- **Why deferred:** a precise per-failure reason requires threading a reason string through
  the fixed `ensureGateway`/`ensureExistingGateway` `(string, bool, error)` signature — a
  design change beyond the slice.
- **Proposed fix:** return a typed verification result (reason enum + message) from
  `ensureExistingGateway` and surface it in the `GatewayReady` condition.
