# NomadJob runbook

`NomadJob` is a **managed lifecycle** object — you author the CR, the operator
strict-decodes `spec.job` into Nomad's job structure, `Register`s it onto the
referenced cluster, and `Deregister`s it (with `purge=true`) when the CR is
removed. The CR is the **source of truth**: the operator re-registers the
declared jobspec whenever it drifts from what Nomad holds. It mirrors the
`NomadCluster`/`NomadPool` create/update/delete model, not the reflector model
`NomadNode` uses.

## Create a job
    kubectl apply -f - <<EOF
    apiVersion: nomad.operator.io/v1alpha1
    kind: NomadJob
    metadata:
      name: web
      namespace: nomad-system
    spec:
      clusterRef:
        name: prod
      jobID: web            # exact Nomad job ID; may differ from metadata.name
      job:
        datacenters: ["dc1"]
        type: service
        taskGroups:
          - name: web
            count: 2
            tasks:
              - name: server
                driver: docker
                config:
                  image: nginx:1.27
            update:
              minHealthyTime: 10000000000   # 10s expressed as INTEGER NANOSECONDS
    EOF

## Authoring `spec.job`
`spec.job` is the Nomad jobspec expressed as the **`api.Job` structure** (the
Go API shape), not HCL. It is **schemaless** — the CRD does not model its
fields, so there is no per-field OpenAPI validation at admission. Instead the
operator **strict-decodes** it: keys are matched to `api.Job` fields
case-insensitively, so **camelCase (`taskGroups`) or PascalCase (`TaskGroups`)
both work**, and any key that does not map to a real field is **rejected**
(see "How a typo surfaces" below).

### The duration-nanoseconds caveat
Nomad's HCL accepts duration strings like `"10s"`, but the `api.Job` structure
does **not** — its `time.Duration` fields are decoded from **integer
nanoseconds**. Anywhere HCL would take a duration string, `spec.job` takes a
raw nanosecond integer:

- `minHealthyTime: 10000000000` — **not** `"10s"`
- `healthyDeadline: 300000000000` — 5m as nanoseconds
- `interval`, `timeout`, `stagger`, `progressDeadline`, etc. — same rule.

A duration written as a string (`"10s"`) is a decode error, and surfaces
exactly like any other bad key (below). Convert: seconds × 1_000_000_000.

## Why `spec.jobID` is separate from `metadata.name`
`spec.jobID` is the exact Nomad job ID and is a **separate top-level field**
rather than being read out of `spec.job`. Two reasons:

- **Kubernetes-name vs Nomad-ID.** Nomad job IDs allow characters
  (underscores, dots, uppercase) that a Kubernetes object name does not.
- **CEL cannot see inside `spec.job`.** `spec.job` is a schemaless
  `RawExtension`, and CEL validation rules cannot reach into it. Identity and
  immutability therefore have to live in a typed field the API server can
  validate.

The operator injects `spec.jobID` as the **authoritative** `job.ID` before
registering. If `spec.job` *also* carries an `id`/`ID` and it differs from
`spec.jobID`, the job is rejected with `Ready=False, reason=JobIDMismatch` —
the two identities must agree (or omit it from `spec.job` entirely and let the
operator inject it).

**`spec.jobID` and `spec.clusterRef.name` are immutable** (CEL-enforced at
admission): a job's identity and its home cluster are fixed for the CR's
lifetime. To move a job, delete and re-create the CR.

## How a typo surfaces — `InvalidJobSpec`
Because `spec.job` is strict-decoded, a mistyped key (`taskGropus`), a wrong
type (`count: "2"` where an int is expected), or a duration string
(`minHealthyTime: "10s"`) does not silently drop — it fails the decode. The
CR gets:

- `Ready=False, reason=InvalidJobSpec`, and the **decoder error itself** in the
  condition message (e.g. `unknown field "taskGropus"`), so you can see exactly
  which key is wrong without leaving `kubectl`.

Nothing is registered onto Nomad while the spec is invalid. Fix the key and
re-apply.

## Update semantics — edit `spec.job`, re-register only on drift
To change a running job, edit `spec.job` (or any managed field) and re-apply.
On each reconcile the operator runs Nomad's **`Plan`** (a server-side dry-run)
and re-`Register`s **only when `Plan` reports a diff** (`Diff.Type != "None"`).
A steady-state resync of an unchanged job therefore does **not** churn Raft or
bump the job version. Register is Nomad's upsert, so an update is the same call
as a create — gated by the plan.

Register warnings from Nomad (e.g. deprecation notices) are surfaced as a
Warning event on the CR; they do not fail the reconcile.

## Deletion semantics — the finalizer deregisters with `purge=true`
`kubectl delete nomadjob <name>` does **not** delete instantly. The operator
holds a finalizer (`nomad.operator.io/nomadjob-cleanup`) and calls Nomad's
`Deregister` with **`purge=true`** — the CR going away means the job should not
exist, and purge fully removes the job record rather than leaving a queryable
dead record that would collide with a later re-create. Unlike `NomadPool`,
a job is **never refused** deletion on the Nomad side — there is no
"not empty" analog, so a job always deregisters cleanly.

If the deregister call itself **fails** (Nomad unreachable, transient error):

- The CR stays `Terminating`.
- A `DeleteBlocked=True, reason=DeregisterFailed` condition appears with the
  underlying error in its message. The operator requeues and retries — no need
  to re-run `kubectl delete`.

If the job's `NomadCluster` is not `Ready` when you delete the CR, the
finalizer also stays and `DeleteBlocked=True, reason=ClusterNotReady` is set
until the cluster recovers (the operator cannot confirm deregistration against
a control plane it cannot reach).

## Status fields
- **`status.jobStatus`** — Nomad's job status from `Info`
  (`running`/`pending`/`dead`). Shown in the `Status` print column.
- **`status.jobVersion`** — the server-observed job version from `Info`
  (distinct from `status.observedGeneration`, which tracks the **CR**
  generation the operator last reconciled).
- **`status.groups`** — per-task-group allocation counts, mapping each group
  name to its `running`/`desired` counts, so you can see how the job is
  actually placed without going to the Nomad CLI.
- **`status.conditions`** — `Ready` (with `reason=Registered` on success, or
  `InvalidJobSpec`/`JobIDMismatch`/`ClusterNotFound`/`ClusterNotReady` when
  not) and, on a stuck delete, `DeleteBlocked`.

## Behavior notes
- **Cascade.** Deleting the `NomadCluster` cascades and removes all its
  `NomadJob` CRs (an ownerReference is set on create). This is safe under both
  background and foreground `--cascade` modes: if the cluster CR is gone *or*
  itself being deleted, the job's finalizer drops **without** calling
  `Deregister` — the Nomad control plane (and the job with it) is going away
  regardless.
- **Orphan risk on `--cascade=orphan`.** If you orphan the `NomadCluster`
  while its Nomad control plane keeps running, the job CRs are removed without
  a Nomad-side `Deregister` — the live job is left running (non-destructive;
  re-`apply` the CR to resume managing it). Same premise the other managed CRs
  rely on for cascade — not a new risk.
