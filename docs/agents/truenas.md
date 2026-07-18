# Edge agent — TrueNAS SCALE (Docker driver)

Run a Nomad client **as a container on TrueNAS SCALE** that itself spawns
workload containers via the built-in **Docker** task driver. TrueNAS SCALE
(Electric Eel 24.10+) uses Docker for apps, so the Nomad client just drives the
NAS's existing Docker daemon.

**Prerequisites:** complete the [join backbone](README.md#join-backbone-do-this-first-every-platform)
first — you need `ca.crt`, `client.crt`, `client.key`, and the base `client.hcl`.
Put them on a TrueNAS dataset (e.g. `/mnt/pool/nomad/config`).

> **Compatibility.** The `docker` driver is **built into** Nomad 2.0.4 — no
> plugin version risk. The risk here is **privilege** (see Security below).

## Deployment pattern

The Nomad client runs in a container; the Docker *driver* inside it needs to
reach a Docker daemon. Two options:

1. **Mount the host Docker socket (recommended on TrueNAS).** TrueNAS is already
   the Docker host, so the driver drives the NAS daemon directly. Workload
   containers become **siblings** of the Nomad container and show up in TrueNAS's
   own Docker.
2. **docker-in-docker sidecar.** A privileged `docker:dind` sidecar isolates
   workload containers from the host daemon, at the cost of a nested privileged
   daemon and extra storage layering. Use only if you specifically need workloads
   kept out of the TrueNAS-managed Docker.

This guide uses pattern (1).

## 1. Deploy the client container

In the TrueNAS UI: **Apps → Discover Apps → Custom App**, which accepts a Docker
Compose spec (or run `docker compose` on the host):

```yaml
services:
  nomad-client:
    image: hashicorp/nomad:2.0.4
    command:
      - agent
      - -config=/etc/nomad.d
    network_mode: host            # Nomad needs real host networking for ports/fingerprinting
    pid: host
    privileged: true              # required for cgroup/fingerprinting (see Security)
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock       # driver → host Docker daemon
      - /mnt/pool/nomad/config:/etc/nomad.d:ro           # ca.crt, client.crt, client.key, client.hcl
      - /mnt/pool/nomad/data:/opt/nomad/data             # persist allocation state
    restart: unless-stopped
```

## 2. Client configuration

Add the Docker driver block to the base `client.hcl` (already on the dataset at
`/mnt/pool/nomad/config/client.hcl`):

```hcl
plugin "docker" {
  config {
    endpoint         = "unix:///var/run/docker.sock"   # the mounted host socket
    allow_privileged = false                           # keep false — see Security
    allow_caps       = ["chown", "net_raw"]            # extend only as needed
    volumes { enabled = true }
    gc { image = true; image_delay = "3m"; container = true }
  }
}
```

## 3. Verify

From a TrueNAS shell (or `docker exec` into the client):

```bash
docker exec -it nomad-client nomad node status -self    # → ready
kubectl get nomadnode                                   # a NomadNode CR appears
```

## 4. Sample job

Author the job as a `NomadJob` custom resource and apply it to your
**Kubernetes** cluster — the operator registers it with Nomad:

```yaml
apiVersion: nomad.operator.io/v1alpha1
kind: NomadJob
metadata:
  name: hello-docker
spec:
  clusterRef:
    name: nomad                    # the NomadCluster in this namespace
  jobID: hello-docker
  job:                             # the native Nomad jobspec, as YAML
    datacenters:
      - dc1
    taskGroups:
      - name: web
        tasks:
          - name: web
            driver: docker
            config:
              image: nginx:latest
            resources:
              cpu: 200
              memoryMB: 128
```

```bash
kubectl apply -f hello-docker.yaml
```

## Security — read this

**Mounting `/var/run/docker.sock` grants root-equivalent control of the TrueNAS
host.** Any job Nomad runs — and anyone who can submit jobs — can start a
privileged container, bind-mount `/`, and take over the NAS. This is the single
biggest risk in this design. Mitigate:

- Lock down the Nomad API with **ACLs + mTLS**; never expose this client's API to
  untrusted job submitters.
- Keep `allow_privileged = false` and a **minimal `allow_caps`**.
- Restrict host-path mounts (`volumes { enabled = ... }`).
- Persist `data_dir` on a dataset so allocation state survives restarts.

The docker-in-docker pattern narrows the blast radius to the DinD container, but
DinD still runs `--privileged` — it is a different containment boundary, not a
safe one.
