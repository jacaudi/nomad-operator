# Architecture

nomad-operator runs a [HashiCorp Nomad](https://www.nomadproject.io/) **server
control plane** inside Kubernetes and lets you manage that cluster's day-2
objects declaratively, as Kubernetes custom resources. You describe the desired
state in YAML; the operator reconciles Nomad to match.

It does **not** run your Nomad *clients* (workers). Clients live wherever your
workloads need to run — bare metal, VMs, a Mac mini, a NAS — and join the
operator-managed servers over mTLS. See [Edge agents](agents/README.md) for how
to attach them.

## Custom resources

| Kind | Scope | What it manages | Lifecycle |
|------|-------|-----------------|-----------|
| **NomadCluster** | control plane | A StatefulSet of Nomad servers with mTLS, gossip encryption, ACLs, storage, and an external-access surface | Operator creates & owns everything |
| **NomadNode** | one client node | The eligibility and drain state of an *already-registered* client | Reflected from Nomad; the CR is a control surface, not the machine |
| **NomadPool** | node pool | A Nomad [node pool](https://developer.hashicorp.com/nomad/docs/concepts/node-pools) (description, metadata, scheduler config) | Managed: you own CRUD, operator applies |
| **NomadNamespace** | namespace | A Nomad namespace (quota, capabilities) | Managed: you own CRUD, operator applies |
| **NomadJob** | job | A Nomad job, authored as the native job spec in YAML | Managed: register on apply, deregister (purge) on delete |

Full design records for each live under
[`docs/development/designs/`](development/designs/).

## The control plane (NomadCluster)

A `NomadCluster` reconciles to:

- **A StatefulSet of servers** (`spec.servers` ∈ {1, 3, 5}). An init container
  renders each server's `advertise` stanza at boot — HTTP and RPC advertise the
  external address, Serf advertises the pod IP (server-to-server gossip stays
  in-cluster).
- **mTLS**, from a cert-manager Secret you supply via `spec.tls.certSecretRef`
  (keys `tls.crt`, `tls.key`, `ca.crt`). The operator consumes this Secret; it
  does not issue certificates. Servers run with `verify_server_hostname` and
  `verify_https_client` enabled. Required SANs: `server.<region>.nomad`,
  `client.<region>.nomad`, the Gateway `httpHostname` (Gateway mode),
  `localhost`, `127.0.0.1`.
- **Gossip encryption** — the operator generates a 32-byte Serf key once and
  stores it in Secret `<name>-nomad-gossip-key` (key `key`). It is never
  regenerated (that would split-brain Serf) and is retained on delete.
- **ACLs**, bootstrapped automatically. The management token lands in Secret
  `<name>-nomad-bootstrap-token` (key `token`), also retained on delete.
- **An external-access surface** (`spec.externalAccess.mode`):
  - **Gateway** (default; 1/3/5 servers) — a Gateway API `Gateway` with an HTTP
    TLS-passthrough listener on 4646 and one TCP listener per server for RPC
    4647 (RPC is a raw multiplexed mTLS stream and is not SNI-routable, so each
    server needs its own port — you supply `gateway.rpcPorts`).
  - **LoadBalancer** (single server only) — one `type: LoadBalancer` Service
    exposing RPC 4647 and HTTP 4646.

Once the external address resolves, it is published to
`status.externalAddress`; the cluster reports `status.phase` (e.g.
`Bootstrapping` → `Ready`), `status.quorum` (voters/total), and
`status.members`.

### Ports

| Port | Purpose | Exposed externally? |
|------|---------|---------------------|
| 4646 | HTTP API / UI | Yes |
| 4647 | RPC (client ↔ server, server ↔ server Raft) | Yes (per-server in Gateway mode) |
| 4648 | Serf gossip (server ↔ server only) | No — in-cluster |

## Reconcile model

- **NomadCluster** runs a phase machine: provision storage and the StatefulSet,
  wait for mTLS material and the external address, bootstrap ACLs, then report
  `Ready`. A drift guard watches the advertised RPC address so a single-server
  cluster whose external address changes surfaces a warning rather than silently
  wedging Raft.
- **NomadNode** is reflected, not provisioned. Its reconciler is keyed on the
  `NomadCluster` (only that view can mint the first CR), lists the cluster's
  registered nodes each pass, and creates/updates/prunes one `NomadNode` per
  node. Users edit `spec.eligible` and `spec.drain`; the operator drives those
  onto Nomad and mirrors observed status back. Creating or deleting the CR does
  not create or destroy the machine.
- **NomadPool / NomadNamespace / NomadJob** are managed-lifecycle: you own CRUD,
  the operator applies via the Nomad API and uses finalizers to block deletion
  until the object is safe to remove (or to short-circuit cleanly when the
  cluster itself is going away).

## Security model

- **mTLS everywhere** on the RPC and HTTP planes. The CA is **yours** — the
  operator only reads `ca.crt` from the cert Secret. Both servers and clients
  present certificates whose embedded role/region SAN (`server.<region>.nomad` /
  `client.<region>.nomad`) is what Nomad verifies, not the dialed address.
- **Gossip is encrypted** and server-only; clients never hold the gossip key.
- **ACLs are enabled.** Client *agents* register using their mTLS certificate
  and do not need an ACL token; human API/UI access uses the management token
  (or a token derived from it).

## Where to go next

- [Getting started](../README.md#getting-started) — deploy the operator and
  create a cluster.
- [Edge agents](agents/README.md) — attach Nomad clients from outside the
  cluster.
- [Runbooks](runbooks/) — per-resource operational guides.
- [`docs/development/`](development/) — design records, implementation plans,
  and issue history.
