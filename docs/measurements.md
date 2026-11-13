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
| plan full 100 TiB, 64 KiB clusters | 0 | 0.2 ms | - | - | 8.1 MiB | 0 KiB | 10.1 MiB |
| plan full 100 TiB, auto (2 MiB) clusters | 0 | 0.3 ms | - | - | 10.1 MiB | 0 KiB | 10.2 MiB |
| plan incremental 100 TiB, 1 Mi alternating 64 KiB extents | 2 | 51.9 ms | - | - | 8.1 MiB | 28 KiB | 10.4 MiB |
| plan incremental 100 TiB, 12.8 M alternating 4 MiB extents | 2 | 630.3 ms | - | - | 10.1 MiB | 2 KiB | 10.3 MiB |
| write incremental 100 TiB, 100 x 4 MiB regions | 4 | 0.0 ms | 0.18 s | 2256 MiB/s | 10.1 MiB | 10.0 MiB | 20.8 MiB |
| write full 16 GiB virtual disk, concurrency 1 | 0 | 0.0 ms | 7.19 s | 2279 MiB/s | 10.1 MiB | 10.0 MiB | 21.0 MiB |
| write full 16 GiB virtual disk, concurrency 4 | 0 | 0.0 ms | 1.79 s | 9144 MiB/s | 22.1 MiB | 22.0 MiB | 32.7 MiB |
| write full 2 GiB over NBD (Go test server), concurrency 1 | 0 | 0.0 ms | 2.44 s | 839 MiB/s | 10.1 MiB | 10.2 MiB | 21.3 MiB |
| write full 2 GiB over NBD (Go test server), concurrency 4 | 0 | 0.0 ms | 4.03 s | 508 MiB/s | 22.1 MiB | 22.3 MiB | 33.9 MiB |
| write full 2 GiB over NBD (qemu-nbd), concurrency 1 | 0 | 0.0 ms | 0.40 s | 5081 MiB/s | 10.1 MiB | 10.2 MiB | 21.1 MiB |
| write full 2 GiB over NBD (qemu-nbd), concurrency 4 | 0 | 0.0 ms | 0.27 s | 7453 MiB/s | 22.1 MiB | 22.2 MiB | 33.4 MiB |
| write full 2 GiB over NBD (qemu-nbd), concurrency 8 | 0 | 0.0 ms | 0.28 s | 7220 MiB/s | 38.1 MiB | 38.3 MiB | 49.7 MiB |

Measured on darwin/arm64, 10 CPUs, 1.26.3.

## Columns

| Column | Kind | Meaning |
|---|---|---|
| Walks | measured | Passes over the region sources: Plan's, plus Write's for write rows. 0 means no region source to walk. |
| Plan, Write | measured | Wall-clock time. |
| Throughput | measured | Source bytes read per second of Write. |
| Estimate | estimated | `PlanInfo.MemoryEstimate`, the working memory the plan budgets for: fixed overhead, one metadata cluster, `(Concurrency+1) × BufferSize`, and the touched-L1 bitset. |
| Allocated | measured | Heap bytes allocated by Plan and Write together (`runtime.MemStats.TotalAlloc`), including the NBD client's scratch buffers. |
| Peak RSS | measured | Peak resident memory of the whole scenario process, Go runtime included (about 10 MiB at rest). |

## Reading them

- Planning a 100 TiB disk allocates nothing per cluster or per extent.
  - A full image has no region source, so its plan is pure arithmetic.
  - An incremental plan walks its regions twice and keeps only the touched-L1
    bitset. That is 25 KiB at 64 KiB clusters and under 1 KiB at 2 MiB.
- Allocations track the estimate within 0.3 MiB in every write scenario. RSS
  adds the Go runtime on top.
- Over a real NBD server, concurrency pays: 5.0 GiB/s with one connection,
  where reading already overlaps writing, and 7.3 GiB/s with four on a local
  socket. Over a network, latency makes the gap
  wider.
- The Go test server does worse with more connections because it allocates
  and copies every request in one process. It exists to check protocol
  behaviour, not to measure speed.
- The virtual-disk rows mostly measure generating the source pattern.
