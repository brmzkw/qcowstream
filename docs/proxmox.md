# Proxmox integration notes

qcowstream contains no Proxmox or Plakar code. This page records what a future
integration needs, from the Proxmox sources read on 2026-11-13:
- pve-qemu `2dc0af5` (QEMU 11.1.3)
- qemu-server `7f1c6d7`
- pve-storage `76a5d4012`

None of it was run against a real Proxmox host. Tests against `qemu-nbd` and
the Go test server are not "Proxmox support".

## What the backup export looks like

- **Mechanism:** a backup provider asks for `nbd`. Proxmox exposes each disk's
  backup-access snapshot as a QEMU NBD export, on a Unix socket
  `<root>/<vmid>-nbd.backup-access`.
- **Bitmap:** the dirty bitmap is named after the target,
  `snapshot-access:<storage>`, and exported as
  `qemu:dirty-bitmap:snapshot-access:<storage>`. Its granularity is fixed at
  4 MiB (`PROXMOX_BACKUP_DEFAULT_CHUNK_SIZE`), so increments use 2 MiB clusters.
- **Access control:** reading or querying allocation outside the dirty parts
  fails with `EACCES`. `nbdsource` reads only inside dirty extents, and
  queries `base:allocation` only there.
- **Clean bitmap:** a fully clean bitmap makes the image inaccessible. The
  integration must then produce the empty increment without NBD:
  `Plan` with `Changed: qcowstream.SliceExtents{}`.
- **Zero answers:** `base:allocation` reflects the original disk for areas not
  yet copied by copy-before-write. Copied areas report as allocated. So ZERO
  answers are exact, but some zeros are missed.
- **Server limits:** the server advertises `CAN_MULTI_CONN` and a maximum
  request of `min(max_transfer, 32 MiB)`. `--concurrency` opens that many read
  connections.

## Bitmap modes

The provider requests tracking with `new` or `use`. Proxmox reports the
effective mode as `none`, `new` or `reuse`.

| Effective mode | Image to produce |
|---|---|
| `none` | full |
| `new` | full: the bitmap starts now, there is no valid parent |
| `reuse` with a known parent | incremental against that parent |

## Example message

An illustrative message from the Perl provider to the Plakar importer. It is
not an official Proxmox API.

```json
{
  "vmid": 100,
  "disks": [
    {
      "device": "scsi0",
      "size": 34359738368,
      "nbd_socket": "/run/qemu-server/100-nbd.backup-access",
      "export": "drive-scsi0",
      "bitmap_mode": "reuse",
      "bitmap": "snapshot-access:plakar",
      "parent": { "snapshot": "4f1c…", "image": "scsi0/inc-1.qcow2" },
      "image": "scsi0/inc-2.qcow2"
    }
  ]
}
```

The importer then runs, per disk:

```sh
qcowstream incremental --nbd-socket /run/qemu-server/100-nbd.backup-access \
  --export drive-scsi0 --bitmap snapshot-access:plakar \
  --backing-file inc-1.qcow2 --concurrency 4 --output -
```

`--backing-file` is relative to the image's own directory at restore time:
`inc-2.qcow2` names `inc-1.qcow2`, which names `base.qcow2`. The integration
must restore the files with that layout.

## Contract with the caller

- **Before acknowledging:** the provider's callback stays alive until the
  image is written and Plakar has committed the snapshot. A non-zero exit or a
  `Write` error means no backup exists, and must never be acknowledged as one.
- **Stable view:** the export must not change between walks. Proxmox's
  snapshot access gives that; a plain NBD URI does not.
- **Discards:** qcowstream sends no WRITE, TRIM or WRITE_ZEROES and never
  resets the bitmap. Without discards, Proxmox may need more fleecing storage
  for the duration. That is disk on the Proxmox side, not RAM or temporary
  files of the generator.
- **Resizes:** a disk resize needs a new full image. v1 requires an increment
  to have its parent's virtual size, which the caller must ensure.
- **Restore:** the Proxmox restore reviewed expects a raw view. A future
  integration has to expose the QCOW2 chain as a disk over NBD (`qemu-nbd` on
  the chain top) or convert it, rather than handing the QCOW2 path to the
  restore callback.
