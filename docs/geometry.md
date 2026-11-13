# Geometry limits

## QEMU limits

Checked in `block/qcow2.h` and `block/qcow2.c` of QEMU 8.2.0 and 10.0.0 (git
tags `v8.2.0`, `v10.0.0`). They are enforced in `geometry.go`.

| Limit | Value | Source |
|---|---|---|
| Cluster size | 512 B to 2 MiB | `MIN_CLUSTER_BITS`, `MAX_CLUSTER_BITS` |
| L1 table | 32 MiB, so 4 Mi entries | `QCOW_MAX_L1_SIZE` |
| Refcount table | 8 MiB | `QCOW_MAX_REFTABLE_SIZE` |
| Host offsets | below 2^56 | `L2E_OFFSET_MASK` |
| Backing file name | 1023 bytes, inside cluster 0 | `qcow2_do_open` |
| L1 coverage | `l1_size` maps the whole virtual size | `qcow2_do_open` |

## What the limits mean

One L2 table maps `cluster_size² / 8` bytes, and the L1 has at most 4 Mi
entries.

| Cluster size | One L2 table maps | Largest disk (L1) |
|---|---|---|
| 512 B | 32 KiB | 128 GiB |
| 4 KiB | 2 MiB | 8 TiB |
| 64 KiB | 512 MiB | 2 PiB |
| 2 MiB | 512 GiB | host offset limit, 64 PiB |

A full image reaches the refcount table limit at about the same size as the
L1 limit. With 2 MiB clusters, the 2^56-byte host offset limit binds first.

## Incremental images

With standard L2 tables, a cluster is either in the increment or inherited
whole from the parent. So every changed cluster must be fully covered by the
changed regions: reading the rest of it would read unchanged bytes, and
zero-filling it would corrupt it.

- Regions are merged first, so adjacent regions split by an NBD reply
  boundary count as one.
- Each merged region must start and end on a cluster boundary. An end at the
  virtual size is allowed, since the tail past it is padding.
- The largest compatible cluster size is the largest power of two dividing
  every boundary. One walk finds it.

`ClusterSizeAuto`, the default, picks:

| Mode | Cluster size |
|---|---|
| Full | 2 MiB |
| Incremental | min(2 MiB, alignment of the regions) |

An explicit cluster size is used as given, or refused with
`ErrUnsupportedGranularity`. The error gives the alignment found and the
smallest cluster size the disk size allows.

| Case | Result |
|---|---|
| Proxmox, 4 MiB bitmap | 2 MiB clusters |
| 64 KiB bitmap (QEMU default for raw, LVM, ZFS, RBD) | 64 KiB clusters, up to 2 PiB disks |
| 100 TiB, 64 KiB-aligned regions | 64 KiB clusters |
| 100 TiB, 4 KiB-aligned regions | refused: 4 KiB clusters stop at 8 TiB, larger ones would read unchanged bytes |

The last case needs Extended L2 subclusters (32 per cluster, so 128 KiB
clusters with 4 KiB subclusters), which v1 does not write, or a source with
coarser tracking.

## Mixed chains

Each image in a chain has its own cluster size. A 2 MiB full image and 64 KiB
increments chain fine; the integration tests check such a chain.
