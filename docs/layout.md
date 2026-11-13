# Binary layout

Every image is a standard QCOW2 version 3 file with 16-bit refcounts and
standard L2 tables. Structures are contiguous and cluster-aligned, in this
order. Writing only moves forward: gaps are zero bytes that are written, not
seeks.

| Clusters | Content |
|---|---|
| 0 | Header, extensions, backing file name, zero padding |
| 1 … | Refcount table |
| then | Refcount blocks |
| then | L1 table, sized for the whole virtual disk |
| then | L2 tables of the L1 entries that have one, in L1 order |
| then | Data clusters, in guest order |

## Header (cluster 0)

| Offset | Field | Value |
|---|---|---|
| 0 | magic | `QFI\xfb` |
| 4 | version | 3 |
| 8 | backing_file_offset | offset of the name in cluster 0, or 0 for a full image |
| 16 | backing_file_size | name length, at most 1023 |
| 20 | cluster_bits | 9 to 21 |
| 24 | size | virtual size in bytes |
| 32 | crypt_method | 0 |
| 36 | l1_size | L1 entries covering the virtual size |
| 40 | l1_table_offset | |
| 48 | refcount_table_offset | cluster 1 |
| 56 | refcount_table_clusters | |
| 60 | nb_snapshots, snapshots_offset | 0 |
| 72 | incompatible, compatible, autoclear features | 0: no dirty bit, no corrupt bit, no lazy refcounts |
| 96 | refcount_order | 4 (16 bits) |
| 100 | header_length | 104: no compression type field, read by every QEMU version targeted |

Header extensions follow at byte 104:
- an incremental image has a backing format extension (`0xe2792aca`, value
  `qcow2`), padded to 8 bytes
- every image has the end-of-extensions marker (type 0, length 0)

The backing file name comes next, inside cluster 0. QEMU reads extensions up
to the backing file offset.

## Tables

- **Refcount table entries** point to the refcount blocks. Unused entries are
  0.
- **Refcount blocks** hold 1 for every cluster of the file, refcount
  structures included, and 0 past the end. Every cluster is used exactly once,
  so no other value occurs.
- **L1 entries** point to the L2 tables, with bit 63 (COPIED) set. Entries
  with no L2 table are 0.
- **L2 entries** are one of:
  - *data:* the host offset with COPIED.
  - *zero:* `0x1`, the "reads as zeros" flag, with no host offset. Incremental
    images only, for clusters the source reports as zero; QEMU never reads the
    parent for them.
  - *absent:* 0. The cluster is read from the parent in an incremental image,
    and reads as zeros in a full one.
- **Entries past the virtual end** are 0. The last data cluster is padded with
  zeros up to the cluster size.

## Refcount layout

The refcount blocks count every cluster, themselves included, so their number
is a fixed point:

```text
total = 1 + refcount_table + refcount_blocks + l1 + l2 + data
refcount_blocks = ceil(total / (cluster_size / 2))
refcount_table  = ceil(refcount_blocks * 8 / cluster_size)
```

Iterating from zero only grows and converges in a few steps. The result is the
smallest layout that fits. The image size is exactly `total × cluster_size`.

## Data placement

Data clusters are numbered in guest order. The k-th data cluster sits at
`data_offset + k × cluster_size`, so L2 entries are computed while walking the
regions, with no map per cluster. The data walk emits clusters in the same
order. Both walks check they see the regions the plan saw.
