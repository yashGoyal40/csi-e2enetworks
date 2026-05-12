# E2E Networks Block Storage API — Probe Findings

Manual API exploration to inform CSI driver design. All operations confirmed
end-to-end against the live API in location `Delhi`.

## Auth

Two credentials, both required on every call:

```
Header:       Authorization: Bearer <JWT-AUTH-TOKEN>
Query param:  apikey=<API-KEY>
```

Example:
```
GET /myaccount/api/v1/block_storage/?apikey=<KEY>&location=Delhi&project_id=<PROJECT_ID>
```

JWT issuer: `gateway.e2enetworks.com/auth/realms/apiman`.
This token expires in ~2 years (`exp` claim). API key UUID is long-lived
until manually revoked in the dashboard.

## Base URL

```
https://api.e2enetworks.com/myaccount/api/v1
```

Every block-storage call carries `?apikey=&location=Delhi&project_id=`.

## Block-storage API surface

| Op | Method | Path | Body | Notes |
|----|--------|------|------|-------|
| List | GET | `/block_storage/` | — | Returns `{data: [], total_count}` |
| Get | GET | `/block_storage/{id}/` | — | Returns `{status, vm_detail, template}` |
| Create | POST | `/block_storage/` | `{name, size, iops}` | Returns `{id}`. Status starts `Available`. |
| Delete | DELETE | `/block_storage/{id}/` | — | **Requires status=`Available`** (must detach first) |
| Plans | GET | `/block_storage/plans/` | — | Returns sizes/IOPS tiers. Min 100 GB. |
| Eligible VMs | GET | `/block_storage/{id}/vm/attach/` | — | List of VMs in project that the volume can attach to. **This is where you find the right `vm_id`.** |
| Attach | **PUT** | `/block_storage/{id}/vm/attach/` | `{"vm_id": <bs-vm-id>}` | NOT POST. POST returns 405. |
| Detach | **PUT** | `/block_storage/{id}/vm/detach/` | `{"vm_id": <bs-vm-id>}` | Async; status flips back to `Available` in ~10s. |

## Critical gotcha: VM ID namespace mismatch

The numeric VM identifiers from `/nodes/` are **different** from the ones the
block-storage API expects.

```
GET /nodes/ →
  node-A → id 30xxxx
  node-B → id 30xxxy

GET /block_storage/{id}/vm/attach/ →
  node-A → vm_id 31xxxx    ← THIS is what attach/detach use
  node-B → vm_id 31xxxy    ← THIS is what attach/detach use
```

Both are present in the GET-volume response under `vm_detail`:
```json
"vm_detail": {
  "node_id": 30xxxx,    // == /nodes/ id
  "vm_id": 31xxxx       // == block-storage API id
}
```

**CSI driver implication:** at NodeStageVolume time, the driver knows the
node's hostname. It must:
1. Map hostname → public/private IP (via the Node object's `status.addresses`)
2. Call `GET /block_storage/{id}/vm/attach/` to get the candidate list
3. Match by IP, harvest the `vm_id`
4. Pass that `vm_id` to the attach/detach calls

Caching this mapping per-node is fine. It won't change unless you destroy and
recreate the VM.

## Lifecycle semantics

- **Create** is synchronous. The volume is `Available` immediately on response.
- **Attach** is synchronous (status flips to `Attached` in the response window).
- **Detach** is async. The PUT returns 200 with `"Block Storage Detach Process
  is Started."`, but status remains `Attached` for ~5–10s. Poll `GET /block_storage/{id}/` for `status=Available` before issuing `Delete`.
- **Delete** is rejected with HTTP 412 if status isn't `Available`.

CSI driver implication: `ControllerPublishVolume` (= attach) can return after
the API call resolves. `ControllerUnpublishVolume` (= detach) must wait/poll
to give clean handoff to a subsequent attach on a different node.

## Device discovery on the host (the hard part)

After attach, the kernel does **not** automatically see the new device. Linux
needs an explicit PCI rescan:

```bash
echo 1 > /sys/bus/pci/rescan
```

After the rescan, the device appears as a virtio-blk device:

```
$ lsblk -o NAME,SIZE,TYPE,SERIAL,WWN,MODEL
sr0      368K rom  QM00001     QEMU DVD-ROM
vda    232.8G disk                          ← root disk
vdb     93.1G disk                          ← our 100 GB volume (93.1 GiB usable)
```

**Crucial: there is no SCSI serial, no WWN, no model** that maps the device
back to the volume id. `lsblk` shows blanks. `/sys/block/vdb/serial` is empty.
`/dev/disk/by-id/` has no entry for the new disk. The only stable
post-attach symlinks are by PCI address:

```
/dev/disk/by-path/pci-0000:00:0a.0           -> ../../vdb
/dev/disk/by-path/virtio-pci-0000:00:0a.0    -> ../../vdb
```

The PCI slot is allocated by the hypervisor at attach time. It is not
predictable from outside. That means the driver has to figure out *which*
device is "ours" via diff:

1. Snapshot existing block devices before the attach API call.
2. Issue attach API call.
3. Trigger PCI rescan.
4. Poll `lsblk` (or `/sys/block`) until a new device appears.
5. The new entry is our device.

The `diskseq` attribute in sysfs is monotonically increasing — picking the
device with the highest `diskseq` after rescan is a reliable signal. Volume
attaches are serialized per-node by the CSI controller plugin (single
ControllerPublishVolume call per (node, volume) at a time), so the diff
approach is race-free in practice.

Alternative: if E2E ever exposes the attach point in the API response (PCI
slot, virtio bus, or a serial they program at create time), use that
instead. As of this probe they do not.

## Plans / sizing

```
100 GB  →  bs_size 0.1   IOPS  1500   Rs. 0.69/hour
200 GB  →  bs_size 0.2   IOPS  3000   ...
```

Minimum size is 100 GB. The CSI driver should round all PVC requests up to
100 GB and warn (or error) if the user asked for less.

## Reference: full GET volume response after attach

```json
{
  "code": 200,
  "data": {
    "block_id": <volume-id>,
    "name": "<volume-name>",
    "size": 95368,
    "status": "Attached",
    "template": {
      "DEV_PREFIX": "vd",
      "DRIVER": "raw",
      "TOTAL_IOPS_SEC": "1500"
    },
    "vm_detail": {
      "node_id": <node-id-from-nodes-api>,
      "vm_id":   <vm-id-from-attach-list>,
      "vm_name": "<plan-derived-name>"
    },
    "size_string": "100 GB",
    "bs_size": 0.1,
    "isEncryptionEnbaled": false
  }
}
```

## Resize (`/vm/upgrade/`)

E2E enabled the upgrade endpoint for this account in May 2026. Probed against
the live API and wired through CSI as ControllerExpandVolume + NodeExpandVolume.

```
PUT /myaccount/api/v1/block_storage/{id}/vm/upgrade/?apikey=&project_id=&location=Delhi
{
  "vm_id":              <int>,    // current vm_id from vm_detail.vm_id
  "block_storage_size": <int_GB>, // new size (decimal GB, in 100 GB steps)
  "name":               "<str>"   // must be the volume's current name
}
```

Findings:

- **`vm_id` is an int**, despite the support doc showing it quoted as a string.
  Mirrors `/vm/attach/` payload shape.
- **Volume must be attached**; no offline-resize path. The driver returns
  FailedPrecondition if `status != Attached`.
- **`bs_size` flips immediately** to the new GB value on the GET response.
  **`size` (MiB) lags ~15-20s** while the OpenNebula resize actually runs.
  Poll on `size > pre_upgrade_size` for the "settled" signal.
- **HOTPLUG transient**: a PUT issued in the first ~30-60s after attach
  returns HTTP 500 with `data: "[one.vm.diskresize] Could not resize disk for
  VM <id>, wrong state HOTPLUG."` — the underlying VM hasn't transitioned out
  of HOTPLUG. The client retries that error for up to 90s. Real-world resize
  on a long-running PVC never hits this.
- **Error envelope quirk**: on 500 from the OpenNebula-backed paths the
  meaningful error string lives in `data` (envelope `errors={}`, `message`
  is just "Internal Server Error"). The client now surfaces all three.
- **Decimal-GB math**: a 200 GB allocation reports `size=190735` MiB
  (= 200 × 10⁹ / 2²⁰), i.e. ~186.3 GiB. CSI capacity comparisons therefore
  must be in decimal GB, not bytes — a 200 Gi PVC request maps to a 200 GB
  decimal E2E allocation.

## Open questions / TODO

- [ ] Test snapshot create/list/delete — not yet probed
- [ ] Confirm whether the API returns the PCI slot anywhere. If not, the
      lsblk-diff approach is the only path.
- [ ] Confirm whether attach across regions (Delhi vs others) needs a
      different path; current probe is Delhi-only.
