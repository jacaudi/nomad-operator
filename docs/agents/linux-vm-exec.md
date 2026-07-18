# Edge agent — Linux VM (Isolated Fork/Exec)

Run a Nomad client in a Linux VM and launch workloads with the built-in
**Isolated Fork/Exec (`exec`)** driver — no plugin to install. `exec` runs a host
binary in an isolated environment (chroot, cgroups, PID/IPC namespaces).

**Prerequisites:** complete the [join backbone](README.md#join-backbone-do-this-first-every-platform)
first — you need `ca.crt`, `client.crt`, `client.key`, and the base `client.hcl`.

> **Compatibility.** `exec` is **built into** the Nomad 2.0.4 binary, so there is
> no plugin version risk here. It is **Linux-only** and the Nomad client must run
> **as root**, on a host with cgroups mounted.

## 1. Host prerequisites

- Linux VM with systemd and cgroups (standard on modern distros).
- The Nomad 2.0.4 binary in `PATH`; run the client as **root**.
- The binaries your tasks call must exist inside the driver's chroot (populated
  by default from host dirs like `/bin`, `/lib`, `/usr`, `/etc`); add to the
  chroot in client config if a task needs more.

No plugin download is required — `exec` ships in the Nomad binary.

## 2. Client configuration

The base `client.hcl` from the join backbone is already enough (the `exec`
driver is enabled by default). Add a `plugin "exec"` block **only** to change
defaults:

```hcl
plugin "exec" {
  config {
    # default_pid_mode = "private"   # or "host"
    # default_ipc_mode = "private"   # or "host"
    # no_pivot_root    = false       # chroot without pivot_root when true
    # allow_caps       = ["chown", "dac_override", "net_raw", ...]  # default = Docker's standard set
    # denied_host_uids = "0,1-10"    # block host UID ranges from being requested
    # denied_host_gids = "0,1-10"
  }
}
```

## 3. Run the agent

```bash
sudo nomad agent -config=/etc/nomad.d     # root required for exec isolation
```

Verify per the [join backbone](README.md#5-verify-the-join).

## 4. Sample job

`command` is an absolute path (resolved inside the chroot); `args` is a list.
Author the job as a `NomadJob` custom resource and apply it to your
**Kubernetes** cluster — the operator registers it with Nomad:

```yaml
apiVersion: nomad.operator.io/v1alpha1
kind: NomadJob
metadata:
  name: hello-exec
spec:
  clusterRef:
    name: nomad                    # the NomadCluster in this namespace
  jobID: hello-exec
  job:                             # the native Nomad jobspec, as YAML
    datacenters:
      - dc1
    taskGroups:
      - name: app
        tasks:
          - name: server
            driver: exec
            config:
              command: /bin/sh
              args:
                - -c
                - echo hello from exec && sleep 3600
            resources:
              cpu: 200
              memoryMB: 64
```

```bash
kubectl apply -f hello-exec.yaml
```

## Gotchas

- **Root is mandatory** — a non-root client silently loses the `exec` driver.
- On **cgroup v2** hosts, non-root operation is not supported (Nomad
  [#17816](https://github.com/hashicorp/nomad/issues/17816)).
- Missing libraries/binaries in the chroot are the most common cause of task
  startup failures — stage them, or use a self-contained static binary.

## Consider `exec2` on modern kernels

HashiCorp's newer
[`nomad-driver-exec2`](https://github.com/hashicorp/nomad-driver-exec2)
(Landlock LSM + cgroups v2 + `unshare`) is the forward-looking sandbox driver
with a tighter, more modern security model. Unlike `exec`, it is a **separate
plugin install**. For a fresh Linux VM on Nomad 2.0.4 it is worth evaluating as
the successor to `exec`.
