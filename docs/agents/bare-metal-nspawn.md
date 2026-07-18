# Edge agent — Linux bare metal (systemd-nspawn)

Run a Nomad client on a bare-metal Linux host and launch workloads as
`systemd-nspawn` OS containers, via [JanMa/nomad-driver-nspawn](https://github.com/JanMa/nomad-driver-nspawn).

**Prerequisites:** complete the [join backbone](README.md#join-backbone-do-this-first-every-platform)
first — you need `ca.crt`, `client.crt`, `client.key`, and the base `client.hcl`
in `/etc/nomad.d/`.

> **Compatibility caveat.** `nomad-driver-nspawn`'s latest release is **v0.10.0
> (2023)** and its README targets **Nomad 1.4.0+** — it is **not tested against
> Nomad 2.x**. The plugin ABI has been stable, so it will likely load, but
> validate on a throwaway client and be prepared to rebuild against a current Go
> toolchain and the Nomad 2.0 plugin SDK. This driver requires the Nomad client
> to run **as root**.

## 1. Host prerequisites

```bash
# Debian/Ubuntu: provides systemd-nspawn + machinectl
sudo apt-get install -y systemd-container
# Fedora/RHEL: sudo dnf install -y systemd-container
```

The host needs systemd and cgroups (any modern distro). Install the Nomad 2.0.4
binary (`/usr/local/bin/nomad`) from
[releases.hashicorp.com](https://releases.hashicorp.com/nomad/).

## 2. Install the driver plugin

```bash
git clone https://github.com/JanMa/nomad-driver-nspawn.git
cd nomad-driver-nspawn && make        # builds the plugin binary
sudo mkdir -p /opt/nomad/data/plugins
sudo cp nomad-driver-nspawn /opt/nomad/data/plugins/
```

Nomad loads plugins from `<data_dir>/plugins` by default (here
`/opt/nomad/data/plugins`).

## 3. Client configuration

Append the driver block to the base `client.hcl` from the join backbone:

```hcl
plugin "nspawn" {
  config {
    enabled = true
    volumes = true    # allow host bind-mounts into machines
  }
}
```

## 4. Run the agent

```bash
sudo nomad agent -config=/etc/nomad.d       # must be root for nspawn
```

Then verify per the [join backbone](README.md#5-verify-the-join):
`nomad node status -self` should report `ready`, and a `NomadNode` CR appears.

## 5. Sample job

The `nspawn` driver runs **machine images**, not OCI/Docker images — provision
those on the host (e.g. `debootstrap`, `machinectl pull-tar`). A single-command
task:

```hcl
job "hello-nspawn" {
  group "g" {
    task "debian" {
      driver = "nspawn"
      config {
        image       = "/var/lib/machines/debian"   # an nspawn machine image
        resolv_conf = "copy-host"
        command     = ["/bin/sh", "-c", "echo hello from nspawn && sleep 3600"]
      }
      resources { cpu = 200; memory = 128 }
    }
  }
}
```

Boot a full init system instead of a single command with `boot = true` (mutually
exclusive with `command`).

## Gotchas

- **Root only** — a rootless/unprivileged client silently loses the driver.
- `image` is an nspawn machine image directory/template, **not** a container
  registry image. Pre-stage images on the host.
- `boot = true` (full `systemd` init) and `command`/`process` mode are mutually
  exclusive — pick one per task.
