# nomad-operator

**Run a HashiCorp Nomad control plane on Kubernetes, and manage it declaratively
as Kubernetes resources.**

nomad-operator provisions a production-shaped Nomad **server** cluster inside
Kubernetes — StatefulSet, mTLS, encrypted gossip, ACLs, persistent storage, and
an external-access surface — from a single `NomadCluster` resource. Once it is
running, you manage the cluster's day-2 objects (nodes, node pools, namespaces,
jobs) as native Kubernetes custom resources: write YAML, and the operator
reconciles Nomad to match.

Your Nomad **clients** (the workers) run wherever you need capacity — a Linux
box, a VM, a Mac mini, a NAS at the edge — and join the operator-managed servers
over mTLS. See [Edge agents](docs/agents/README.md).

> **Status:** all five custom resources are implemented and tested against
> **Nomad v2.0.4**. The API group is `nomad.operator.io/v1alpha1`.

> [!WARNING]
> **Agentically generated.** This codebase was produced through agentic,
> spec-driven development: each feature began as a written design and
> implementation spec, then a coding agent executed the plan under human
> review. Tests, code review, and CI gates apply as they would for any
> project, but the authorship pattern is not a single human contributor —
> keep that in mind when evaluating fit for your environment.

## Table of contents

- [Why](#why)
- [How it works](#how-it-works)
- [Getting started](#getting-started)
  - [Prerequisites](#prerequisites)
  - [Install the operator](#install-the-operator)
  - [Create a cluster](#create-a-cluster)
  - [Attach a client](#attach-a-client)
  - [Schedule work](#schedule-work)
- [Custom resources](#custom-resources)
- [Project layout](#project-layout)
- [Documentation](#documentation)
- [Development](#development)
- [License](#license)

## Why

Nomad is a great scheduler, but running its control plane by hand means managing
StatefulSets, TLS material, gossip keys, ACL bootstrap, and network exposure
yourself, then keeping day-2 objects in sync out-of-band. nomad-operator folds
all of that into the Kubernetes control loop:

- **One resource for the control plane.** `NomadCluster` owns the servers, mTLS,
  gossip, ACLs, storage, and external access. Scale (1/3/5 servers),
  certificates, and exposure are spec fields, not runbooks.
- **Day-2 objects as CRs.** Node pools, namespaces, and jobs are declarative and
  reconciled; nodes are reflected into Kubernetes so you can drain or cordon them
  with `kubectl`.
- **GitOps-friendly.** Everything is YAML under your existing Kubernetes
  workflow — no separate Nomad provisioning pipeline.

## How it works

The operator reconciles five resources. See
[Architecture](docs/architecture.md) for the full picture.

- **NomadCluster** — the server control plane: a StatefulSet of Nomad servers
  with mTLS (from a cert-manager Secret you supply), an operator-generated gossip
  key, automatic ACL bootstrap, persistent storage, and an external-access
  surface (Gateway API per-server RPC listeners, or a LoadBalancer for a
  single-server cluster). It reports `status.phase`, `status.externalAddress`,
  and `status.quorum`.
- **NomadNode** — reflects each registered client into a CR so you can manage its
  `eligible`/`drain` state from Kubernetes. It does not create or destroy
  machines.
- **NomadPool / NomadNamespace / NomadJob** — managed-lifecycle resources: you
  own CRUD, the operator applies them through the Nomad API and cleans up on
  delete via finalizers.

## Getting started

### Prerequisites

- A Kubernetes cluster (v1.30+) and `kubectl`.
- **Gateway API CRDs** — the manager watches `Gateway`/`TCPRoute`/`TLSRoute`, so
  these must be installed even if you only use LoadBalancer mode. Bundled copies
  are in [`config/crd/gateway-api/`](config/crd/gateway-api/).
- An mTLS certificate **Secret** for the servers (`tls.crt`, `tls.key`,
  `ca.crt`) — see the note below on how to produce it.
- A default `StorageClass` for the servers' persistent volumes.
- To build the image yourself: Go 1.26+ and Docker.

> [!NOTE]
> The operator **consumes** the certificate Secret; it does not mint
> certificates. Two common ways to produce and keep it in sync:
>
> - **[cert-manager](https://cert-manager.io/)** — issue the cert in-cluster from
>   an `Issuer`/CA (Kubernetes-native PKI).
> - **[External Secrets Operator](https://external-secrets.io/)** — sync a cert
>   minted and stored in an external system (e.g. Vault PKI, a cloud secrets
>   manager) into the Kubernetes Secret.
>
> Either way, the Secret needs `tls.crt`, `tls.key`, `ca.crt`, and its SANs must
> include `server.<region>.nomad`, `client.<region>.nomad`, the gateway
> `httpHostname` (Gateway mode), `localhost`, and `127.0.0.1`.

### Install the operator

```bash
# 1. Gateway API CRDs (required)
kubectl apply -f config/crd/gateway-api/

# 2. The operator's CRDs
make install

# 3. The controller (set IMG to your built/pushed image)
make deploy IMG=<registry>/nomad-operator:latest
```

`make install` applies the `NomadCluster`, `NomadNode`, `NomadPool`,
`NomadNamespace`, and `NomadJob` CRDs; `make deploy` installs the controller,
RBAC, and namespace. To build and push the image first: `make docker-build
docker-push IMG=<registry>/nomad-operator:latest`.

### Create a cluster

First, a server certificate. The SANs are mandatory — Nomad verifies the
embedded role/region, not the address:

```yaml
apiVersion: cert-manager.io/v1
kind: Certificate
metadata:
  name: nomad-server-tls
spec:
  secretName: nomad-server-tls               # ← referenced by the cluster below
  issuerRef:
    name: nomad-ca-issuer
    kind: Issuer
  commonName: server.global.nomad
  dnsNames:                                  # region defaults to "global"
    - server.global.nomad
    - client.global.nomad
    - nomad.example.com                      # = gateway.httpHostname
    - localhost
  ipAddresses:
    - 127.0.0.1
  usages:
    - server auth
    - client auth
```

Then the cluster — a 3-server HA control plane exposed through the Gateway API:

```yaml
apiVersion: nomad.operator.io/v1alpha1
kind: NomadCluster
metadata:
  name: nomad
spec:
  image: hashicorp/nomad:2.0.4
  servers: 3                       # 1, 3, or 5
  region: global
  datacenters:
    - dc1
  storage:
    size: 10Gi
    # storageClassName: fast-ssd   # omit to use the default
  tls:
    certSecretRef: nomad-server-tls
  externalAccess:
    mode: Gateway
    gateway:
      mode: Managed                # operator creates & owns the Gateway
      httpHostname: nomad.example.com
      rpcPorts:                         # one RPC listener per server
        - 14647
        - 24647
        - 34647
```

```bash
kubectl apply -f cluster.yaml
kubectl get nomadcluster nomad -w        # wait for phase: Ready
kubectl get nomadcluster nomad -o jsonpath='{.status.externalAddress}'; echo
```

For a single-server cluster you can use `externalAccess.mode: LoadBalancer`
instead (one VIP for RPC 4647 + HTTP 4646). See the
[NomadCluster runbook](docs/runbooks/nomadcluster.md).

### Attach a client

Clients run outside Kubernetes and self-register over mTLS. The
[Edge agents guide](docs/agents/README.md) covers the shared join procedure plus
per-platform setup:

- [Linux bare metal (systemd-nspawn)](docs/agents/bare-metal-nspawn.md)
- [Mac mini (Apple `container`)](docs/agents/mac-mini-container.md)
- [Linux VM (Isolated Fork/Exec)](docs/agents/linux-vm-exec.md)
- [TrueNAS SCALE (Docker)](docs/agents/truenas.md)

### Schedule work

With a client registered, apply namespaces, pools, and jobs as CRs:

```yaml
apiVersion: nomad.operator.io/v1alpha1
kind: NomadJob
metadata:
  name: hello
spec:
  clusterRef:
    name: nomad                    # the NomadCluster in this namespace
  jobID: hello
  job:                             # the native Nomad job spec, in YAML
    datacenters:
      - dc1
    taskGroups:
      - name: web
        tasks:
          - name: server
            driver: docker
            config:
              image: nginx:latest
```

See the per-resource [runbooks](docs/runbooks/) for the full field reference.

## Custom resources

| Kind | Manages | You control | Operator does |
|------|---------|-------------|---------------|
| `NomadCluster` | The server control plane | scale, TLS, storage, exposure | provisions & owns everything |
| `NomadNode` | A registered client | `eligible`, `drain` | reflects status, drives eligibility/drain |
| `NomadPool` | A node pool | full CRUD | applies via the Nomad API |
| `NomadNamespace` | A namespace | full CRUD | applies via the Nomad API |
| `NomadJob` | A job | full CRUD | registers / deregisters (purge on delete) |

## Project layout

```
cmd/main.go              Manager entry point (registers the controllers)
api/v1alpha1/            CRD types (+kubebuilder markers) and generated code
internal/controller/     Reconcilers for the five resources
internal/nomad/          Typed Nomad API client (pinned to v2.0.4)
config/                  CRDs, RBAC, manager manifests, samples, Gateway API CRDs
docs/                    User docs (this README's TOC) + docs/development history
```

`AGENTS.md` documents the codebase conventions for contributors and AI agents.

## Documentation

- **[Architecture](docs/architecture.md)** — resources, control-plane internals,
  reconcile model, security model.
- **[Production example](docs/production-example.md)** — end-to-end: a CA synced
  by External Secrets Operator, certs issued by cert-manager, an HA cluster, a
  TrueNAS client, and a workload.
- **[Edge agents](docs/agents/README.md)** — attach Nomad clients from outside
  the cluster ([bare metal](docs/agents/bare-metal-nspawn.md) ·
  [Mac mini](docs/agents/mac-mini-container.md) ·
  [Linux VM](docs/agents/linux-vm-exec.md) ·
  [TrueNAS](docs/agents/truenas.md)).
- **Runbooks** — operational guides per resource:
  [NomadCluster](docs/runbooks/nomadcluster.md) ·
  [NomadNode](docs/runbooks/nomadnode.md) ·
  [NomadPool](docs/runbooks/nomadpool.md) ·
  [NomadNamespace](docs/runbooks/nomadnamespace.md) ·
  [NomadJob](docs/runbooks/nomadjob.md).
- **[docs/development/](docs/development/)** — design records, implementation
  plans, and issue history (contributor-facing).

## Development

```bash
make manifests generate fmt vet   # regenerate CRDs/DeepCopy, format, vet
make test                         # envtest-backed controller + unit tests
make test-integration             # hermetic tests against a real nomad v2.0.4 binary
make run                          # run the controller against your kubeconfig
```

Contributor conventions and the codebase map are in
[`AGENTS.md`](AGENTS.md); design and planning history lives under
[`docs/development/`](docs/development/).

## License

Apache License 2.0 (`SPDX-License-Identifier: Apache-2.0`).

> A top-level `LICENSE` file has not yet been committed — add the canonical
> Apache-2.0 text to complete the declaration.
