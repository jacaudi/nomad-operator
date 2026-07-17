# NomadNamespace runbook

`NomadNamespace` is a **managed lifecycle** object — you author the CR, the
operator `Register`s the declared Nomad namespace onto the cluster and `Delete`s
it when the CR is removed. It mirrors the `NomadPool` create/update/delete
model. A *Nomad* namespace is a Nomad-internal tenancy partition, **not** a
Kubernetes namespace — the CR itself lives in a Kubernetes namespace
(`nomad-system` below), but `spec.namespaceName` names the partition inside
Nomad.

## Create a namespace
    kubectl apply -f - <<EOF
    apiVersion: nomad.operator.io/v1alpha1
    kind: NomadNamespace
    metadata:
      name: team-a
      namespace: nomad-system
    spec:
      clusterRef:
        name: prod
      namespaceName: team-a   # exact Nomad namespace name; may differ from metadata.name
      description: "Team A workloads"
      meta:
        team: a
        tier: standard
    EOF

- **`spec.clusterRef.name`** names the `NomadCluster` (in the *same* Kubernetes
  namespace) this namespace lives on. It is **immutable** — a namespace belongs
  to one cluster for its lifetime. Cross-namespace references are not supported
  (see [Deferred](#deferred-not-built-in-v1)).
- **`spec.namespaceName`** is the exact Nomad namespace name and is kept
  separate from `metadata.name` because Nomad namespace names allow characters
  (underscores, uppercase) that a Kubernetes object name does not. It is
  **immutable** (CEL-enforced at admission) and constrained to
  `^[a-zA-Z0-9-_]{1,128}$`. The built-in `default` namespace is rejected at
  admission (see below).
- **`spec.description`** and **`spec.meta`** are optional and map straight to
  `Namespace.Description` / `Namespace.Meta`.
- **`spec.meta` is fully managed.** The map you declare owns the namespace's
  `Meta` entirely — any key set out-of-band (CLI, another tool) is overwritten
  on the next reconcile. There is no merge.

## Why `default` is rejected
`default` is Nomad's **built-in** namespace — it always exists and cannot be
created or deleted. It is therefore not representable as a managed CR:
`spec.namespaceName` is CEL-validated to reject `default` at admission, before
the operator ever calls Nomad. (If a reconcile ever reaches the apply path with
a reserved name anyway — e.g. a pre-existing CR — the reconciler defensively
sets `Ready=False, reason=ReservedNamespace` and skips the `Register`.)

## Conditions and their reasons
Two condition types report state; watch them with
`kubectl describe nomadnamespace <name>`.

**`Ready`** — steady-state health of the managed namespace:

| Status | Reason | Meaning |
|--------|--------|---------|
| `True`  | `Registered`            | The namespace is registered onto Nomad and matches spec. |
| `False` | `ClusterNotFound`       | The referenced `NomadCluster` does not exist in this Kubernetes namespace. |
| `False` | `ClusterNotReady`       | The referenced `NomadCluster` exists but is not `Ready` (control plane unreachable/unconfigured). |
| `False` | `ReservedNamespace`     | `namespaceName` is a built-in (`default`) and cannot be managed. |
| `False` | `NamespaceNameConflict` | Another `NomadNamespace` CR targets the same `namespaceName` on the same cluster; the `Register` is skipped (see below). |

**`DeleteBlocked`** — only appears while a CR is `Terminating` and the finalizer
cannot yet complete:

| Status | Reason | Meaning |
|--------|--------|---------|
| `True` | `ClusterNotReady`   | The namespace's `NomadCluster` is unreachable/not `Ready`, so deletion cannot be confirmed. |
| `True` | `NamespaceNotEmpty` | Nomad refused the delete because the namespace still has non-terminal jobs. |
| `True` | `DeleteFailed`      | The Nomad `Delete` call failed for some other reason (see the condition message). |

## Read-modify-write preserves unmanaged config
This CRD models only `Description` and `Meta`. Nomad namespaces also carry
`Quota`, `Capabilities`, and other server-side configuration that this CRD does
**not** own (out of scope in v1 — no present consumer). Rather than blindly
overwriting the namespace on every `Register` — which would silently wipe a
`Quota` or `Capabilities` block set out-of-band the first time the operator
adopts an existing namespace — the reconciler does a **read-modify-write**: it
fetches the current namespace, overlays only the managed fields
(`Description`, `Meta`) from `spec`, and preserves whatever
`Quota` / `Capabilities` / other config is already set. `Register` is only
called when a managed field actually differs from what's live, so a
steady-state resync does not churn Raft.

## Deletion semantics — the finalizer blocks until the namespace is empty
`kubectl delete nomadnamespace <name>` does **not** delete instantly. The
operator holds a finalizer and calls Nomad's `Delete` on the namespace; Nomad
itself refuses to delete a namespace that still has **non-terminal jobs** in it.
If that happens:

- The CR stays `Terminating`.
- A `DeleteBlocked=True, reason=NamespaceNotEmpty` condition appears, with
  `status.jobCount` populated so you can see the job total without going to the
  Nomad CLI.

To unblock it: stop or move the jobs still running in the namespace (delete
their `NomadJob` CRs, or `nomad job stop -purge` out-of-band jobs), then let the
operator's requeue retry the delete — no need to re-run `kubectl delete`.

If the namespace's `NomadCluster` is unreachable or not `Ready` when you delete
the CR, the finalizer also stays and `DeleteBlocked=True, reason=ClusterNotReady`
is set until the cluster recovers. Any other `Delete` failure surfaces as
`DeleteBlocked=True, reason=DeleteFailed` with the underlying error in the
condition message. In every case the finalizer is retained on **any** Delete
error — the CR is never removed while the Nomad namespace might still exist.

> `status.jobCount` is a raw total from `Jobs().List` (it includes terminal
> jobs). It is informational — the delete **gate** is Nomad's Delete refusal,
> not this count.

## Duplicate `namespaceName` — `NamespaceNameConflict`
Two `NomadNamespace` CRs that declare the same `namespaceName` on the same
`clusterRef` would otherwise fight over one Nomad namespace, each re-`Register`ing
it every resync. Instead, the reconciler detects the collision (a namespaced
`List` filtered for the same cluster + namespaceName), sets
`Ready=False, reason=NamespaceNameConflict` on **every** colliding CR, emits a
Warning event, and skips the `Register` call entirely — so the namespace's
last-written state is left alone rather than flapping. There is no automatic
winner election: resolve it by renaming or deleting one of the conflicting CRs.

## Placing jobs into a managed namespace — `NomadJob.spec.nomadNamespace`
A `NomadJob` selects its Nomad namespace by name via
**`spec.nomadNamespace`** (default `"default"`, immutable). The linkage is
**by-name only** — there is no hard CR-to-CR reference and no `NamespaceNotReady`
wait state:

- The named namespace must **already exist** when the job registers — created by
  a `NomadNamespace` CR, out-of-band, or the always-present `default`. The
  operator injects `spec.nomadNamespace` as the authoritative `job.Namespace`.
- A `namespace` embedded inside `spec.job` that **disagrees** with
  `spec.nomadNamespace` is rejected: the NomadJob reports
  `Ready=False, reason=NamespaceMismatch` and is not registered, rather than
  silently landing in the wrong partition.

## Deferred (not built in v1)
Per the design (§5), the following are intentionally **not** modeled yet; each is
additive later and nothing here forecloses it:

- **Cross-namespace `clusterRef`** — a `NomadNamespace` can only reference a
  `NomadCluster` in its own Kubernetes namespace. Cross-namespace tenancy
  passthrough has RBAC implications and is a larger change.
- **Managing `Quota` / `Capabilities`** (and `NodePoolConfiguration`,
  Vault/Consul config) — these are *preserved* via read-modify-write but not
  *owned*. They would arrive as new `spec` fields feeding the same `Register`.
