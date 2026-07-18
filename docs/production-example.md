# Production example — cert-manager, External Secrets Operator, and a workload

An end-to-end walkthrough that wires together the pieces a real deployment uses:

- your **CA** lives in an external secrets manager and is synced into Kubernetes
  by **External Secrets Operator (ESO)**;
- **cert-manager** issues the Nomad **server** and **client** leaf certificates
  from that CA;
- **nomad-operator** provisions an HA control plane consuming the server cert;
- a Nomad **client on a TrueNAS box** joins over mTLS; and
- a **`NomadJob`** runs nginx on that client via the Docker driver.

Everything Kubernetes-side lives in the `nomad` namespace.

> [!IMPORTANT]
> The server certificate and **every** client certificate must be signed by the
> **same CA**. The operator publishes that CA's `ca.crt`, and every client
> verifies the servers against it (and vice-versa). This example keeps a single
> CA — held in your secrets manager, issued from by cert-manager — so mTLS works
> end to end. Mixing CAs (e.g. a self-signed server cert and a Vault-issued
> client cert) silently breaks the RPC handshake.

## Topology

```
external secrets manager (Vault, holds CA cert+key)
        │  (1) ESO ExternalSecret
        ▼
   Secret: nomad-ca (tls.crt + tls.key)
        │  (2) cert-manager CA Issuer
        ├──────────────► Secret: nomad-server-tls  ──► (3) NomadCluster (servers in k8s)
        └──────────────► Secret: nomad-client-truenas-tls
                                   │  (4) extracted to the TrueNAS host
                                   ▼
                         Nomad client on TrueNAS (Docker driver)  ◄── mTLS RPC
                                   ▲
                                   │  (5) NomadJob → operator registers with Nomad
                              nginx workload
```

## Prerequisites

- A Kubernetes cluster + `kubectl`, with the **Gateway API CRDs** and
  **nomad-operator** installed (see the [README](../README.md#getting-started)).
- **[cert-manager](https://cert-manager.io/)** and
  **[External Secrets Operator](https://external-secrets.io/)** installed.
- An external secrets store holding your Nomad CA certificate and key. This
  example uses **HashiCorp Vault**; swap the `SecretStore` provider for AWS
  Secrets Manager, GCP, etc.
- A default `StorageClass`.
- A **TrueNAS SCALE** box (Electric Eel 24.10+) that can reach the cluster's
  external RPC address.

## 0. Namespace

```yaml
apiVersion: v1
kind: Namespace
metadata:
  name: nomad
```

## 1. Sync the CA from your secrets manager (ESO)

Point ESO at your store, then sync the CA cert+key into a Kubernetes Secret that
cert-manager can use as a CA issuer.

```yaml
apiVersion: external-secrets.io/v1
kind: SecretStore
metadata:
  name: vault-backend
  namespace: nomad
spec:
  provider:
    vault:
      server: https://vault.example.com:8200
      path: secret
      version: v2
      auth:
        appRole:
          path: approle
          roleId: <your-vault-role-id>
          secretRef:
            name: vault-approle          # a Secret holding the AppRole secret-id
            key: secret-id
```

```yaml
apiVersion: external-secrets.io/v1
kind: ExternalSecret
metadata:
  name: nomad-ca
  namespace: nomad
spec:
  refreshInterval: 1h
  secretStoreRef:
    name: vault-backend
    kind: SecretStore
  target:
    name: nomad-ca                        # the k8s Secret ESO creates
    creationPolicy: Owner
    template:
      type: kubernetes.io/tls             # cert-manager CA issuer wants tls.crt + tls.key
  data:
    - secretKey: tls.crt
      remoteRef:
        key: nomad/pki/ca
        property: tls.crt
    - secretKey: tls.key
      remoteRef:
        key: nomad/pki/ca
        property: tls.key
```

> [!NOTE]
> Older ESO installs use `external-secrets.io/v1beta1`. If your `SecretStore`
> refers to a `ClusterSecretStore` instead, set `secretStoreRef.kind:
> ClusterSecretStore`.

## 2. Issue the server and client certificates (cert-manager)

Use the ESO-synced CA as a cert-manager `Issuer`, then mint two leaf certs from
it — one for the servers, one for the TrueNAS client — so both share the CA.

```yaml
apiVersion: cert-manager.io/v1
kind: Issuer
metadata:
  name: nomad-ca-issuer
  namespace: nomad
spec:
  ca:
    secretName: nomad-ca                  # from step 1
```

```yaml
apiVersion: cert-manager.io/v1
kind: Certificate
metadata:
  name: nomad-server-tls
  namespace: nomad
spec:
  secretName: nomad-server-tls            # consumed by the NomadCluster
  issuerRef:
    name: nomad-ca-issuer
    kind: Issuer
  commonName: server.global.nomad
  dnsNames:                               # region defaults to "global"
    - server.global.nomad
    - client.global.nomad
    - nomad.example.com                   # = gateway.httpHostname
    - localhost
  ipAddresses:
    - 127.0.0.1
  usages:
    - server auth
    - client auth
```

```yaml
apiVersion: cert-manager.io/v1
kind: Certificate
metadata:
  name: nomad-client-truenas-tls
  namespace: nomad
spec:
  secretName: nomad-client-truenas-tls    # extracted onto the TrueNAS host (step 4)
  issuerRef:
    name: nomad-ca-issuer
    kind: Issuer
  commonName: client.global.nomad
  dnsNames:
    - client.global.nomad
  usages:
    - client auth
    - server auth
```

## 3. Provision the control plane

Three servers, HA, exposed through the Gateway API.

```yaml
apiVersion: nomad.operator.io/v1alpha1
kind: NomadCluster
metadata:
  name: nomad
  namespace: nomad
spec:
  image: hashicorp/nomad:2.0.4
  servers: 3
  region: global
  datacenters:
    - dc1
  storage:
    size: 10Gi
    storageClassName: fast-ssd
  tls:
    certSecretRef: nomad-server-tls
  externalAccess:
    mode: Gateway
    gateway:
      mode: Managed
      httpHostname: nomad.example.com
      rpcPorts:
        - 14647
        - 24647
        - 34647
```

```bash
kubectl apply -f cluster.yaml
kubectl -n nomad get nomadcluster nomad -w                                   # wait for phase: Ready
ADDR=$(kubectl -n nomad get nomadcluster nomad -o jsonpath='{.status.externalAddress}')
echo "$ADDR"
```

## 4. Join the TrueNAS client

Extract the client bundle and the CA onto the TrueNAS host (into a dataset such
as `/mnt/pool/nomad/config`):

```bash
kubectl -n nomad get secret nomad-client-truenas-tls -o jsonpath='{.data.tls\.crt}' | base64 -d > client.crt
kubectl -n nomad get secret nomad-client-truenas-tls -o jsonpath='{.data.tls\.key}' | base64 -d > client.key
kubectl -n nomad get secret nomad-client-truenas-tls -o jsonpath='{.data.ca\.crt}' | base64 -d > ca.crt
```

Write the client config (`/mnt/pool/nomad/config/client.hcl`), pointing at the
external RPC listeners from step 3:

```hcl
data_dir   = "/opt/nomad/data"
region     = "global"
datacenter = "dc1"

client {
  enabled = true
  servers = ["<ADDR>:14647", "<ADDR>:24647", "<ADDR>:34647"]
}

tls {
  http = true
  rpc  = true
  ca_file   = "/etc/nomad.d/ca.crt"
  cert_file = "/etc/nomad.d/client.crt"
  key_file  = "/etc/nomad.d/client.key"
  verify_server_hostname = true
  verify_https_client    = true
}

plugin "docker" {
  config {
    endpoint         = "unix:///var/run/docker.sock"
    allow_privileged = false
    volumes { enabled = true }
    gc { image = true; container = true }
  }
}
```

Run the Nomad client as a container on TrueNAS (**Apps → Discover Apps → Custom
App**, Docker Compose):

```yaml
services:
  nomad-client:
    image: hashicorp/nomad:2.0.4
    command:
      - agent
      - -config=/etc/nomad.d
    network_mode: host
    pid: host
    privileged: true
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock
      - /mnt/pool/nomad/config:/etc/nomad.d:ro     # client.hcl, ca.crt, client.crt, client.key
      - /mnt/pool/nomad/data:/opt/nomad/data
    restart: unless-stopped
```

Verify the client registered:

```bash
docker exec -it nomad-client nomad node status -self     # → ready
kubectl -n nomad get nomadnode                           # a NomadNode CR appears
```

See [Edge agents → TrueNAS](agents/truenas.md) for the driver details and the
host-socket security discussion.

## 5. Run the workload

An nginx job, scheduled onto the TrueNAS client via the Docker driver:

```yaml
apiVersion: nomad.operator.io/v1alpha1
kind: NomadJob
metadata:
  name: nginx
  namespace: nomad
spec:
  clusterRef:
    name: nomad                     # the NomadCluster above
  jobID: nginx
  nomadNamespace: default
  job:                              # the native Nomad jobspec, as YAML
    datacenters:
      - dc1
    taskGroups:
      - name: web
        count: 1
        networks:
          - mode: host
        tasks:
          - name: nginx
            driver: docker
            config:
              image: nginx:1.27
            resources:
              cpu: 200
              memoryMB: 128
```

```bash
kubectl apply -f nginx-job.yaml
```

## 6. Verify end to end

```bash
kubectl -n nomad get nomadjob nginx -o wide          # Reconciled=True, running
docker exec -it nomad-client nomad job status nginx  # allocation running on the TrueNAS client
```

## Security notes

- **Host Docker socket.** Mounting `/var/run/docker.sock` gives the Nomad client
  (and anyone who can submit jobs) root-equivalent control of the TrueNAS host.
  Lock the Nomad API down with ACLs + mTLS, keep `allow_privileged = false`, and
  never expose the client's API to untrusted submitters. See
  [agents/truenas.md](agents/truenas.md#security--read-this).
- **CA key.** The CA private key is the root of trust for the whole cluster.
  Keep it in your secrets manager, scope ESO's read access tightly, and prefer a
  short `refreshInterval` with rotation handled in the store rather than copying
  the key around.
- **Certificate rotation.** cert-manager renews the leaf certs automatically;
  restart the servers (rolling) and re-extract the client bundle when the CA
  rotates.
