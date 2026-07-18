# Edge agent — Mac mini (Apple `container` runtime)

Run a Nomad client on an Apple-Silicon Mac mini and launch Linux OCI containers
in lightweight per-container VMs via Apple's native `container` framework, using
[anultravioletaurora/nomad-driver-container](https://github.com/anultravioletaurora/nomad-driver-container).

**Prerequisites:** complete the [join backbone](README.md#join-backbone-do-this-first-every-platform)
first — you need `ca.crt`, `client.crt`, `client.key`, and the base `client.hcl`.

> **Compatibility caveat — read before relying on this.** This driver is an
> early, single-author, **dev-tagged** project (latest `v0.3.0-dev`, 2026-06;
> no stable release). Its README targets **Nomad ≥ 1.8.0** — **not tested on
> 2.x**. It requires **Apple Silicon**, **macOS 26**, and **Apple's `container`
> CLI ≥ 0.11.0**. Treat it as experimental: pin to a specific `-dev` build and
> re-test on every upgrade. (The README says "macOS 26 (Sequoia)", which is
> self-contradictory — macOS 26 is *Tahoe*; the binding requirement is the
> version number **26**, i.e. Apple's current `container` framework.)

## 1. Host prerequisites

```bash
brew install container       # Apple's container runtime (or from github.com/apple/container/releases)
container system start       # start the container system service
```

Install the Nomad 2.0.4 `darwin_arm64` binary from
[releases.hashicorp.com](https://releases.hashicorp.com/nomad/) into your `PATH`.

## 2. Install the driver plugin

```bash
git clone https://github.com/anultravioletaurora/nomad-driver-container
cd nomad-driver-container && make dev
sudo mkdir -p /opt/nomad/data/plugins
sudo cp ./build/nomad-driver-container /opt/nomad/data/plugins/
```

## 3. Client configuration

Point the client at the plugin directory and add the driver block to the base
`client.hcl`:

```hcl
client {
  enabled    = true
  plugin_dir = "/opt/nomad/data/plugins"
  servers    = ["<address>:14647", "<address>:24647", "<address>:34647"]
}

plugin "nomad-driver-container" {          # NOTE: the binary/registered name...
  config {
    container_path         = "/opt/homebrew/bin/container"   # Apple-Silicon Homebrew path
    gc { container = true }
    volumes { enabled = true }
    image_pull_timeout     = "5m"
    disable_log_collection = false
  }
}
```

The **plugin block is named `nomad-driver-container`** (the binary name) but the
**task `driver` is `container`** — don't confuse the two.

## 4. Run the agent

```bash
sudo nomad agent -config=/etc/nomad.d     # container system service must be running
```

Verify per the [join backbone](README.md#5-verify-the-join).

## 5. Sample job

Author the job as a `NomadJob` custom resource and apply it to your
**Kubernetes** cluster — the operator registers it with Nomad:

```yaml
apiVersion: nomad.operator.io/v1alpha1
kind: NomadJob
metadata:
  name: hello-container
spec:
  clusterRef:
    name: nomad                    # the NomadCluster in this namespace
  jobID: hello-container
  job:                             # the native Nomad jobspec, as YAML
    datacenters:
      - dc1
    taskGroups:
      - name: web
        tasks:
          - name: nginx
            driver: container      # <- task driver name
            config:
              image: nginx:latest
              init: true
              # rosetta: true      # run x86 images via Rosetta
            resources:
              cpu: 1000
              memoryMB: 128
```

```bash
kubectl apply -f hello-container.yaml
```

## Gotchas

- **Apple Silicon + macOS 26 only.** No Intel-Mac support; the `container`
  framework requires a current macOS.
- Each container is a micro-VM, so cold-start and image-pull latency differ from
  Docker (hence the 5m default `image_pull_timeout`).
- Dev-only tags mean the config surface can change between builds — pin and
  re-test.
