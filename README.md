# qcowstream

qcowstream writes QCOW2 images as one sequential stream: a full image of a
disk, or an incremental image of its changed regions that names its parent. It
needs no seek, no temporary file and no access to the parent, and works in
bounded memory, including for disks of 100 TiB.

It exists for Plakar's Proxmox backups. Plakar snapshots can inherit a base
and add whole files, but cannot patch a file at an offset. Changed blocks of a
VM disk are patches at offsets. qcowstream turns each set of changes into a
new immutable QCOW2 file whose backing file is the previous one. QEMU replays
the chain on restore:

```text
S0: base.qcow2
S1: base.qcow2 + inc-1.qcow2
S2: base.qcow2 + inc-1.qcow2 + inc-2.qcow2      inc-2 -> inc-1 -> base
```

The design and its decisions are in [spec.md](spec.md).

## Packages

| Package | |
|---|---|
| `github.com/brmzkw/qcowstream` | The generator. Standard library only. |
| `github.com/brmzkw/qcowstream/nbdsource` | Reads an NBD export on a Unix socket: data, QEMU dirty bitmap regions, `base:allocation` zeros. |
| `cmd/qcowstream` | The CLI, over a raw file or NBD. |

## CLI

```sh
go install github.com/brmzkw/qcowstream/cmd/qcowstream@latest

# Full image of a raw file or block device. Holes are not read.
qcowstream full --source-file disk.raw --output base.qcow2

# Full and incremental images over NBD.
qcowstream full --nbd-socket /run/source.sock --export scsi0 --concurrency 4 --output - > base.qcow2
qcowstream incremental --nbd-socket /run/source.sock --export scsi0 \
  --bitmap backup-bitmap --backing-file base.qcow2 --output - > inc-1.qcow2

# The layout as JSON, without reading disk data.
qcowstream plan incremental --nbd-socket /run/source.sock --export scsi0 \
  --bitmap backup-bitmap --backing-file base.qcow2

# An incremental image from a raw file needs an explicit regions file:
# {"virtual_size": N, "extents": [{"offset": N, "length": N}, ...]}
qcowstream incremental --source-file disk.raw --regions changes.json \
  --backing-file base.qcow2 --output inc-1.qcow2
```

| Flag | Default | |
|---|---|---|
| `--cluster-size` | `auto` | 2 MiB, or the largest the changed regions allow |
| `--memory-limit` | `128MiB` | |
| `--buffer-size` | `max(4MiB, cluster size)` | size of one source read |
| `--concurrency` | `1` | reads in flight, and NBD connections; 4 to 8 suits NBD |
| `--no-zero-detection` | off | read clusters the source reports as zeros instead of skipping them |
| `--force` | off | overwrite an existing `--output` file |

- **Units:** sizes take IEC (`KiB`, `MiB`, `GiB`, `TiB`) or SI (`KB`, `MB`,
  `GB`, `TB`) suffixes. A bare `K` or `M` is refused as ambiguous.
- **Output:** with `--output -`, stdout carries only image bytes, and
  diagnostics go to stderr.
- **Failure:** on failure, or SIGINT or SIGTERM, the exit status is non-zero
  and an output file is removed. A stream sent to stdout is just incomplete:
  the reader must discard it.

## Library

```go
ext := qcowstream.Extents{Changed: changed, Zeros: zeros}
plan, err := qcowstream.Plan(ctx, ext, qcowstream.Options{
    Mode:        qcowstream.Incremental,
    VirtualSize: size,
    BackingFile: "base.qcow2",
})
// plan.Info() has the exact output size, counters, geometry and memory estimate.
stats, err := qcowstream.Write(ctx, w, src, ext, plan)
```

See the package examples for full, incremental, pipe and NBD use.

- **Sources:**
  - `Reader` reads the disk at an offset.
  - `ExtentSource` enumerates ordered, non-overlapping regions, and must give
    the same regions on every walk.
  - Adapters are `ReaderAtSource` for files, `FileHoles` for `SEEK_HOLE`
    zeros, `SliceExtents` for small lists, and `nbdsource` for NBD.
- **Errors:** every error `Plan` can detect comes before the first byte.
  `Write` errors leave a partial stream; a nil error means every byte reached
  the writer.
- **Error types:** `ErrInvalidOptions`, `ErrUnsupportedGranularity`,
  `ErrUnstableSource`, `ErrInvalidRegion`, `ErrMemoryBudget`, `ErrOverflow`,
  `ErrIncompleteRead`, `ErrIncompleteWrite`, `ErrCanceled`, and
  `nbdsource.ErrMissingContext`.

## Guarantees

- **Output:** a standard QCOW2 v3 file, checked by `qemu-img check` and byte
  comparisons on QEMU 8.2 and 10.0 ([compatibility](docs/compatibility.md)).
- **Single pass:** written once, front to back. No header patch, no repair,
  no rebase.
- **Deterministic:** the same bytes for the same inputs, whatever the
  concurrency.
- **Incrementals:**
  - Only changed clusters are read.
  - Changed zeros are stored, or flagged as zeros, never inherited from the
    parent.
  - An empty change list is a valid, empty increment.
- **Memory:** the working memory is estimated before writing and bounded by
  `--memory-limit` ([measurements](docs/measurements.md)).

## Limits

- **Geometry:** standard L2 tables only. Changed regions must cover whole
  clusters. 100 TiB with 4 KiB change tracking is refused, as it would need
  Extended L2 ([geometry](docs/geometry.md)).
- **Resize:** an increment must have its parent's virtual size. A resize
  needs a new full image.
- **Formats and transport:** QCOW2 parents only, NBD over Unix sockets only
  (no TCP or TLS), no compression or encryption (Plakar does both).
- **Stability:** sources must stay unchanged across walks. qcowstream detects
  region changes, but not data changes under a region.
- **Proxmox:** not tested against a real Proxmox export. The integration
  notes are in [docs/proxmox.md](docs/proxmox.md).
- **Not included:** no parent garbage collection, no restore orchestration.

## Development

```sh
go test ./...                      # unit tests, no QEMU needed
(cd integration && go test ./...)  # QEMU 8.2 and 10.0 in Docker; fails without Docker
go run ./internal/cmd/qcowbench    # measurements
```

Dependencies and their licenses are listed in
[DEPENDENCIES.md](DEPENDENCIES.md). The project's license is not chosen yet.
