# Measurements

Produced by `go run ./internal/cmd/qcowbench`. Every scenario runs in its own
process, with deterministic sources: virtual disks computed from the offset,
procedural region maps, and a 2 GiB random raw file served by `qemu-nbd`. The
destination is `io.Discard`, so the figures exclude destination cost.

## Results

Measured on 2026-11-13, Apple M-series laptop, QEMU 10.2.2 `qemu-nbd`, over a
Unix socket with the file in the page cache.

| Scenario | Walks | Plan | Write | Throughput | Estimate | Allocated | Peak RSS |
|---|---:|---:|---:|---:|---:|---:|---:|
| plan full 100 TiB, 64 KiB clusters | 0 | 0.3 ms | - | - | 4.1 MiB | 0 KiB | 10.1 MiB |
| plan full 100 TiB, auto (2 MiB) clusters | 0 | 0.3 ms | - | - | 6.1 MiB | 0 KiB | 10.4 MiB |
| plan incremental 100 TiB, 1 Mi alternating 64 KiB extents | 2 | 51.8 ms | - | - | 4.1 MiB | 28 KiB | 10.4 MiB |
| plan incremental 100 TiB, 12.8 M alternating 4 MiB extents | 2 | 632.7 ms | - | - | 6.1 MiB | 2 KiB | 10.3 MiB |
| write incremental 100 TiB, 100 x 4 MiB regions | 4 | 0.0 ms | 0.18 s | 2255 MiB/s | 6.1 MiB | 6.0 MiB | 16.8 MiB |
| write full 16 GiB virtual disk, concurrency 1 | 0 | 0.0 ms | 7.14 s | 2294 MiB/s | 6.1 MiB | 6.0 MiB | 16.7 MiB |
| write full 16 GiB virtual disk, concurrency 4 | 0 | 0.0 ms | 1.79 s | 9138 MiB/s | 18.1 MiB | 18.0 MiB | 28.7 MiB |
| write full 2 GiB over NBD (Go test server), concurrency 1 | 0 | 0.0 ms | 2.31 s | 885 MiB/s | 6.1 MiB | 6.2 MiB | 17.1 MiB |
| write full 2 GiB over NBD (Go test server), concurrency 4 | 0 | 0.0 ms | 3.81 s | 538 MiB/s | 18.1 MiB | 18.3 MiB | 29.7 MiB |
| write full 2 GiB over NBD (qemu-nbd), concurrency 1 | 0 | 0.0 ms | 0.66 s | 3086 MiB/s | 6.1 MiB | 6.2 MiB | 16.9 MiB |
| write full 2 GiB over NBD (qemu-nbd), concurrency 4 | 0 | 0.0 ms | 0.29 s | 7039 MiB/s | 18.1 MiB | 18.3 MiB | 30.1 MiB |
| write full 2 GiB over NBD (qemu-nbd), concurrency 8 | 0 | 0.0 ms | 0.28 s | 7250 MiB/s | 34.1 MiB | 34.3 MiB | 45.8 MiB |

Measured on darwin/arm64, 10 CPUs, 1.26.3.

## Columns

| Column | Kind | Meaning |
|---|---|---|
| Walks | measured | Passes over the region sources: Plan's, plus Write's for write rows. 0 means no region source to walk. |
| Plan, Write | measured | Wall-clock time. |
| Throughput | measured | Source bytes read per second of Write. |
| Estimate | estimated | `PlanInfo.MemoryEstimate`, the working memory the plan budgets for: fixed overhead, one metadata cluster, `Concurrency × BufferSize`, and the touched-L1 bitset. |
| Allocated | measured | Heap bytes allocated by Plan and Write together (`runtime.MemStats.TotalAlloc`), including the NBD client's scratch buffers. |
| Peak RSS | measured | Peak resident memory of the whole scenario process, Go runtime included (about 10 MiB at rest). |

## Reading them

- Planning a 100 TiB disk allocates nothing per cluster or per extent.
  - A full image has no region source, so its plan is pure arithmetic.
  - An incremental plan walks its regions twice and keeps only the touched-L1
    bitset. That is 25 KiB at 64 KiB clusters and under 1 KiB at 2 MiB.
- Allocations track the estimate within 0.3 MiB in every write scenario. RSS
  adds the Go runtime on top.
- Over a real NBD server, concurrency pays: 3.0 GiB/s with one connection,
  7 GiB/s with four on a local socket. Over a network, latency makes the gap
  wider.
- The Go test server does worse with more connections because it allocates
  and copies every request in one process. It exists to check protocol
  behaviour, not to measure speed.
- The virtual-disk rows mostly measure generating the source pattern.
