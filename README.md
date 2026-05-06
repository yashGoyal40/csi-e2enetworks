# csi-e2enetworks

Container Storage Interface (CSI) driver for **E2E Networks block storage**.
Lets you create `PersistentVolumeClaim`s in self-managed Kubernetes clusters
running on E2E Networks VMs and have them backed by real E2E block volumes
(provisioned, attached, formatted, and mounted automatically).

> **Status: 0.1 alpha.** API client is end-to-end verified against the live
> E2E API. Full in-cluster CSI flow has been built and Helm-packaged but the
> first cluster install of the published chart is still outstanding.

## What it does

| When you do this in K8s | What the driver does on E2E |
|---|---|
| `kubectl apply -f pvc.yaml`     | Calls `POST /block_storage/` → creates a 100 GB+ block volume |
| Pod scheduled to a node         | Resolves the node's E2E `vm_id`, calls `PUT /block_storage/{id}/vm/attach/`, triggers PCI rescan, formats ext4/xfs, mounts into the pod |
| Pod moves to another node       | Detaches from the old node, re-attaches on the new one |
| `kubectl delete pvc`            | Detaches, waits for `Available`, calls `DELETE /block_storage/{id}/` |

`ReadWriteOnce` only (E2E PDs cannot be shared between VMs at once).
Minimum size 100 GB (E2E's smallest tier).

---

## Installation (single command)

```bash
helm repo add csi-e2enetworks https://yashgoyal40.github.io/csi-e2enetworks
helm repo update

helm install csi-e2enetworks csi-e2enetworks/csi-e2enetworks \
  --namespace csi-e2enetworks --create-namespace \
  --set e2e.apiKey=<UUID-API-KEY> \
  --set e2e.authToken=<JWT-AUTH-TOKEN> \
  --set e2e.projectID=<numeric-project-id>
```

Or, point at a values file:

```yaml
# my-values.yaml
e2e:
  apiKey: <UUID-API-KEY>
  authToken: <JWT-AUTH-TOKEN>
  projectID: "<numeric-project-id>"
  location: Delhi      # E2E region

storageClass:
  name: e2e-block      # name your apps will reference
  default: false       # set true to make it the cluster default
  parameters:
    fsType: ext4       # or xfs
```

```bash
helm install csi-e2enetworks csi-e2enetworks/csi-e2enetworks \
  --namespace csi-e2enetworks --create-namespace \
  -f my-values.yaml
```

The chart will create:

- `Secret/csi-e2enetworks-creds` holding your API credentials (or use `existingSecret` to point at one you manage out-of-band).
- `CSIDriver/e2e.csi.speakx.in`.
- `Deployment/csi-e2enetworks-controller` with the controller plugin + sidecars (`csi-provisioner`, `csi-attacher`, `csi-resizer`).
- `DaemonSet/csi-e2enetworks-node` running on every node (privileged, with `node-driver-registrar`).
- `StorageClass/e2e-block`.

## Where the credentials come from

| What | Where |
|---|---|
| **API Key + Auth Token** | MyAccount → API → "Create new Token". The token must have **Write** capability for the driver to attach/detach/delete. |
| **Project ID** | Open https://myaccount.e2enetworks.com/, log in, then in the browser console run `localStorage.getItem('currentProject')` — it's the numeric ID (e.g. `50198`). |

## Use it

```yaml
# pvc.yaml
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: data
spec:
  accessModes: [ReadWriteOnce]
  storageClassName: e2e-block
  resources:
    requests:
      storage: 100Gi
---
apiVersion: v1
kind: Pod
metadata: {name: writer}
spec:
  containers:
    - name: app
      image: alpine
      command: ["sh", "-c", "echo hello >> /data/log.txt && sleep 3600"]
      volumeMounts:
        - {name: data, mountPath: /data}
  volumes:
    - name: data
      persistentVolumeClaim: {claimName: data}
```

```bash
kubectl apply -f pvc.yaml
kubectl wait --for=condition=Bound pvc/data --timeout=2m
kubectl exec writer -- cat /data/log.txt
```

---

## Architecture

```
controller plugin (Deployment, 1 replica)             node plugin (DaemonSet, 1/node)
┌──────────────────────────────────────┐              ┌────────────────────────────────┐
│ csi-driver --mode=controller         │              │ csi-driver --mode=node         │
│   CreateVolume    →  POST .../       │              │   NodeStageVolume   → diff     │
│   DeleteVolume    →  DELETE          │              │     /sys/block, format,        │
│   ControllerPublishVolume → PUT      │              │     mount to staging dir       │
│   ControllerUnpublishVolume → PUT    │              │   NodePublishVolume → bind     │
│                                      │              │     mount staging → pod        │
│ csi-provisioner (sidecar)            │              │ node-driver-registrar (sidecar)│
│ csi-attacher    (sidecar)            │              │                                │
│ csi-resizer     (sidecar)            │              │ privileged + hostPath /dev/sys │
└──────────────────────────────────────┘              └────────────────────────────────┘
        ▲                                                              ▲
        │ PersistentVolumeClaims, VolumeAttachments                    │ kubelet socket
        │                                                              │ (CSI registry)
        │              kube-apiserver  ───────────────────────────────┘
        ▼
  E2E Networks API   https://api.e2enetworks.com/myaccount/api/v1/...
```

### How the Node plugin identifies "which device just appeared"

E2E doesn't expose a serial / WWN inside the guest — `lsblk` shows attached
volumes with empty SERIAL / WWN columns and `/dev/disk/by-id/` is empty.
The node plugin works around this by:

1. Snapshotting `/sys/block` (names + `diskseq`) **before** the attach.
2. The controller does the API attach (synchronous).
3. `echo 1 > /sys/bus/pci/rescan` to make the kernel re-enumerate.
4. Polling `/sys/block` for ~30 s until a new device appears.
5. Picking the entry with the highest `diskseq`.

CSI serializes `ControllerPublishVolume` per (node, volume), so the diff is
race-free in practice.

See [`API_NOTES.md`](./API_NOTES.md) for the full set of API quirks
encountered during the live probe (vm_id namespace mismatch, HTML rate-limit
pages on rapid PUT, status eventual-consistency on detach).

---

## Configuration reference

| Value                              | Default                              | Notes |
|-----------------------------------|--------------------------------------|-------|
| `e2e.apiKey`                      | *(required)*                         | UUID-format API key |
| `e2e.authToken`                   | *(required)*                         | JWT token, expires in ~2 years |
| `e2e.projectID`                   | *(required)*                         | Numeric project id |
| `e2e.location`                    | `Delhi`                              | E2E region |
| `existingSecret`                  | *(empty)*                            | Use a Secret you manage; needs keys `apiKey`, `authToken`, `projectID` |
| `image.repository`                | `yashgoyal04/csi-e2enetworks`        | Docker Hub public image |
| `image.tag`                       | `0.1.0`                              | |
| `storageClass.create`             | `true`                               | |
| `storageClass.name`               | `e2e-block`                          | |
| `storageClass.default`            | `false`                              | Make this the cluster default |
| `storageClass.reclaimPolicy`      | `Delete`                             | or `Retain` |
| `storageClass.parameters.fsType`  | `ext4`                               | or `xfs` |
| `storageClass.parameters.iops`    | `"1500"`                             | E2E IOPS tier (1500 for 100 GB, 3000 for 200 GB) |
| `storageClass.allowVolumeExpansion` | `true`                             | |
| `nodeSelector`                    | `{}`                                 | Restrict the node DaemonSet (defaults to every node) |
| `controllerPlugin.replicaCount`   | `1`                                  | |

Sidecar images are pinned to recent stable releases — see [`chart/values.yaml`](./chart/values.yaml).

---

## Multiple environments (dev + stage with different E2E projects)

Install the chart twice with different project IDs and different StorageClass names:

```bash
helm install csi-dev csi-e2enetworks/csi-e2enetworks \
  --namespace csi-e2enetworks-dev --create-namespace \
  --set e2e.projectID=<dev-project-id> \
  --set storageClass.name=e2e-block-dev \
  -f dev-values.yaml

helm install csi-stage csi-e2enetworks/csi-e2enetworks \
  --namespace csi-e2enetworks-stage --create-namespace \
  --set e2e.projectID=<stage-project-id> \
  --set storageClass.name=e2e-block-stage \
  -f stage-values.yaml
```

Apps then pick the right environment via `storageClassName: e2e-block-dev` or `e2e-block-stage`.

---

## Develop / build from source

```
.
├── chart/                 Helm chart (published to GitHub Pages by .github/workflows/release.yml)
├── cmd/
│   ├── csi-e2enetworks/   Single-binary driver (controller + node)
│   └── api-smoketest/     Standalone API exerciser
├── pkg/
│   ├── e2e/               E2E Networks API client
│   └── driver/            CSI Identity + Controller + Node services
├── Dockerfile             Multi-stage Alpine, ~13 MB compressed
├── API_NOTES.md           Probe findings (read this if extending the API client)
└── README.md
```

### Smoke-test the API client (no cluster needed)

Create `.env.local`:

```bash
export E2E_API_KEY="<UUID>"
export E2E_AUTH_TOKEN="<JWT>"
export E2E_PROJECT_ID="<numeric>"
export E2E_LOCATION="Delhi"
```

Then:

```bash
source .env.local
go run ./cmd/api-smoketest -nodeip <private-ip-of-any-node-in-project>
```

Walks: create → wait Available → attach → detach → wait Available → delete.

### Build the Go binary

```bash
go build -o bin/csi-e2enetworks ./cmd/csi-e2enetworks
```

### Build the image

```bash
docker build -t yashgoyal04/csi-e2enetworks:0.1.0 .
docker push  yashgoyal04/csi-e2enetworks:0.1.0
```

### Helm

```bash
helm lint     chart --set e2e.apiKey=x --set e2e.authToken=x --set e2e.projectID=1
helm template test  chart --set e2e.apiKey=x --set e2e.authToken=x --set e2e.projectID=1
helm package  chart --destination ./bin
```

The GitHub Pages release pipeline (`.github/workflows/release.yml`) packages
the chart on every push to `main` that touches `chart/**` or `README.md`,
publishes the `.tgz` + `index.yaml` to `gh-pages`, and renders this README
as the repo landing page.

---

## Known gaps / TODO

- [ ] **First cluster install** of the published chart — image is on Docker Hub, chart packages cleanly, but the live `helm install` against a real cluster has not yet been done.
- [ ] **Resize** (`/upgrade/`) — driver advertises the capability but the API endpoint is not yet wired through.
- [ ] **Snapshots** — not implemented.
- [ ] **JWT auto-refresh** — current setup uses a long-lived JWT (≈2 years). Replace with a refresh-on-401 flow before that expires.
- [ ] **ArtifactHub listing** — repo metadata is in place (`chart/artifacthub-repo.yml`); listing it on artifacthub.io is a one-time manual step (add a "Helm" repo pointing at the gh-pages URL).

---

## License

Apache-2.0 — see [`LICENSE`](./LICENSE).
