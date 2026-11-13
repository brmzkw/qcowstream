# qcowstream — specification and implementation plan

`qcowstream` is a standalone Go project, usable as a library and as a program.
It produces full and incremental QCOW2 images directly into a sequential
stream: no temporary file, no access to the parent image, bounded memory. A
separate NBD adapter reads Proxmox backup exports so that a Plakar importer can
consume them.

The deliverable is the implemented, documented and tested project. Part I gives
the context, Part II the requirements, Part III the decisions taken during
design review, Part IV the implementation plan. Proposed Go signatures may be
improved as long as their guarantees hold.

---

# Part I — Context

## Proxmox integration in progress

Plakar has a Proxmox integration under development in
[plakar-pve-provider](https://github.com/gillesdubois/plakar-pve-provider). It
uses a Proxmox storage plugin of type `plakar` paired with a Perl backup
provider called by VZDump. Plakar drives the backup with:

```sh
PLAKAR_SESSION=<session> vzdump <vmid> --storage plakar
```

For QEMU VMs, Proxmox prepares a point-in-time view of each disk and exposes it
over NBD. The Perl provider hands Plakar a manifest with the VM configuration,
the size of each disk and the path of its NBD socket, then waits for Plakar to
finish reading. The plugin gives access to blocks; it does not carry their
content. Reading the blocks is the job of the Go importer. This path does not
require Proxmox Backup Server.
([technical notes](https://github.com/gillesdubois/plakar-pve-provider/blob/main/TECHNICAL.md))

In the version reviewed, `backup_get_mechanism` returns `nbd` for VMs and
`backup_vm_query_incremental` requests no incremental tracking. The manifest has
sizes and NBD paths but no bitmap information yet. Incremental backups are a
future step.
([current Perl provider](https://github.com/gillesdubois/plakar-pve-provider/blob/main/src/PVE/BackupProvider/Plugin/Plakar.pm))

## Plakar PITR model

Plakar's planned PITR feature creates a snapshot from an existing base snapshot.
The new snapshot inherits the base content and may add new files or replace a
whole file at the same path. It cannot modify part of an inherited file at an
offset.

Composition is logical: referencing the base does not copy its files. But added
content must be whole files. Plakar stores the set; the application-specific
restore component knows how to use it.

## PostgreSQL example

The first snapshot holds a `pg_basebackup` and the WAL needed to recover it.
Later snapshots inherit it and add newly archived WAL segments:

```text
S0 : base backup + initial WAL
S1 : S0 + new WAL up to T1
S2 : S1 + new WAL up to T2
```

On restore, Plakar restores files and PostgreSQL replays WAL up to the target.
WAL segments are already separate files the engine consumes: no offset-level
modification is needed in Plakar.
([PostgreSQL PITR](https://www.postgresql.org/docs/current/continuous-archiving.html))

## The Proxmox problem

For a VM, changed-block tracking tells which disk regions changed since the
previous capture. Those are replacements at offsets of the virtual disk, not a
journal the VM replays. They must be expressed as new files that fit Plakar's
composition model.

The answer: one full QCOW2 image, then one new QCOW2 per increment holding the
changed regions and a reference to its parent. Inherited images never change:

```text
S0 : base.qcow2
S1 : base.qcow2 + inc-1.qcow2
S2 : base.qcow2 + inc-1.qcow2 + inc-2.qcow2

Disk chain at T2:
inc-2.qcow2 -> inc-1.qcow2 -> base.qcow2
```

On restore, QEMU interprets the chain and exposes the resulting disk, which can
be converted or handed to Proxmox's restore. The QCOW2 files carry the change
semantics; Plakar keeps storing immutable files.

The hard part is generating each QCOW2 as a stream straight into Plakar: no
temporary file, no access to the parent, bounded memory, including for very
large disks. A normal QCOW2 writer updates metadata it already wrote. This
project instead computes the full layout up front and then emits the file
sequentially, once.

`qcowstream` provides a generic Go library, a separate NBD adapter and a CLI.
Evolving the Perl provider, enabling incremental tracking, publishing snapshots
and orchestrating restore belong to future integrations. Periodic block
captures restore the captured instants only; they do not give recovery to an
arbitrary intermediate instant like a continuous journal would.

---

# Part II — Requirements

## 1 Goals and limits

Three mandatory components: a generic QCOW2 generation library, an NBD source
adapter, and a CLI combining both that also offers a raw file source.

The core library uses only the Go standard library: no QEMU at runtime, no
external process, no CGO, no Plakar or Proxmox dependency. NBD dependencies stay
in a separate package. QEMU is a validation oracle in integration tests, never a
production writer.

The output is a real, standard QCOW2, not a proprietary archive or a patch
series. QEMU can use it once fully received. Output goes to an `io.Writer`;
never require `Seek`, `WriteAt`, `Truncate`, a local file or a block device on
the destination side.

A planning phase and several passes over source metadata are allowed. Reading
all data before emitting is forbidden. No on-disk staging, no whole QCOW2 in
RAM, no hidden temporary file, no destination NBD server, no `qemu-img` to
produce the file.

v1 produces QCOW2 v3 with standard L2 tables, the v3 "reads as zeros" L2 flag
and 16-bit refcounts. No compression or encryption: Plakar provides both. No
internal snapshots (the chain uses backing files), persistent bitmaps (change
tracking lives on the live disk), external data file (it splits the stream) or
lazy refcounts (refcounts are final when written). Extended L2 is out of v1; its
absence is an explicit geometry limit, never an excuse to read forbidden source
blocks.

## 2 Image semantics

**Full**: no backing file. The image represents every byte of the virtual
space. Without zero information, every cluster is stored, zeros included. The
full plan is implicit and holds no per-cluster entry.

**Incremental**: only selected regions are materialised. Absent regions inherit
from a parent named by a relative path. The parent is absent during generation:
never open, read or download it. Changed blocks that contain zeros must never
fall through to the parent: they are stored as allocated data, or as zero-flagged
L2 entries when the source reports them as zero (below).

**Zero detection from metadata**: a source may provide a replayable enumeration
of regions it guarantees read as zeros (NBD `base:allocation` with the ZERO bit,
`SEEK_HOLE`/`SEEK_DATA` on a raw file). Payload content is never inspected for
this. A cluster entirely inside known-zero regions is not read:

- full: left unallocated, which reads as zeros without a backing file. An L2
  table whose clusters are all unallocated is not written.
- incremental: its L2 entry carries the v3 "reads as zeros" flag (bit 0) with no
  host offset, so it reads as zeros and never consults the parent.

A cluster only partly known to be zero is read normally. Zero detection is on by
default and can be disabled.

The backing filename and the `qcow2` backing format are in the header from the
first byte. No rebase is needed after generation. The caller guarantees the
identity and compatibility of the parent; the writer cannot check it.

An incremental with no change is valid: an image with a parent, no allocated
data, minimal consistent metadata. v1 requires the same virtual size as the
parent, per caller-supplied metadata. A resize requires a new full.

## 3 Sources and stability

A source provides a virtual size, positioned reads and, for an incremental, an
ordered enumeration of selected regions. It is an immutable view for the whole
of planning and emission. A raw file must not change during the operation; the
tool does not take a consistent capture of a file being written.

Regions are half-open intervals `[offset, offset+length)`, non-empty, ordered,
non-overlapping and within the virtual size. Adjacent regions may be merged in
constant memory. Out-of-order entries, overflows and out-of-disk regions are
rejected. An empty list means no change, never an implicit full.

```go
type Extent struct {
    Offset uint64
    Length uint64
}

type Reader interface {
    ReadAtContext(ctx context.Context, p []byte, off int64) (int, error)
}

type ExtentSource interface {
    // Each call restarts from zero and yields the same intervals.
    Walk(ctx context.Context, yield func(Extent) error) error
}
```

Provide an `io.ReaderAt` wrapper for files and tests; document that the context
cannot interrupt a blocked `ReadAt`. The NBD adapter must truly interrupt
network waits.

Do not require `[]Extent` or keep one element per cluster. An in-memory adapter
for small lists may exist. v1 requires replayable enumeration; a single-pass
source is rejected, without hidden spooling.

Compute a fingerprint of the canonical (merged) enumeration and its counters to
detect divergence between passes. It does not replace the immutability
contract. A divergence during emission makes the stream incomplete and the
operation fails; success is never reported.

## 4 Geometry and permitted reads

The virtual size is positive, a multiple of 512, and representable as a Go
`int64` offset. Every size, alignment, ceiling and product is overflow-checked.

The cluster size is a power of two allowed by QCOW2 and by the targeted QEMU
readers. Default: the documented automatic choice, which prefers 2 MiB (D4). The
caller may ask for an explicit size.

In incremental mode with standard L2, every allocated guest cluster must be
fully covered by readable regions, except the part past the end of the virtual
disk, which is zero-filled. Coverage is evaluated after merging adjacent
regions; an NBD reply boundary is not a hole boundary.

Never round a region outward and read unchanged bytes. If regions cannot fill
the selected clusters, pick a compatible geometry or return
`ErrUnsupportedGranularity` before any output. Never turn missing sectors of a
cluster into zeros.

Check L1 size, refcount table size and physical offset limits of the targeted
QEMU versions. Required case: 100 TiB with 64 KiB clusters and 64 KiB-aligned
regions. 100 TiB with 4 KiB tracking may be refused in v1; the error explains
that Extended L2 or another readable source would be needed.

## 5 Plan then write

Public phases `Plan` and `Write` are separate; `Generate` is an optional
shortcut.

```go
type Options struct {
    Mode          Mode
    VirtualSize   uint64
    ClusterSize   uint64 // ClusterSizeAuto (the default in the CLI) or an explicit power of two
    BackingFile   string
    BackingFormat string
    MemoryLimit   uint64
    BufferSize    uint64
    Concurrency   int
}

type Extents struct {
    Changed ExtentSource // incremental: required, even for zero changes; full: must be nil
    Zeros   ExtentSource // optional: regions guaranteed to read as zeros
}

func Plan(ctx context.Context, ext Extents, opts Options) (*ImagePlan, error)
func Write(ctx context.Context, dst io.Writer, src Reader,
    ext Extents, plan *ImagePlan) (Stats, error)
```

In full mode `Changed` is nil and the plan covers the whole disk. In
incremental mode it is mandatory, even for zero changes. `Zeros` follows the
same validation, replay and fingerprint rules as `Changed`. Write must receive
the same sources the plan was built from. No ambiguous or silently ignored
options. The plan is immutable, knows the exact output size, and exposes a
memory estimate, the number of data clusters, the number of source bytes to read
and the chosen geometry.

Required steps:

1. Validate options, parent name, coverage and geometry.
2. Count data clusters and needed L2 tables without reading payload.
3. Compute the final physical layout of every structure and of the data.
4. Resolve the dependency between total clusters, refcount blocks and refcount
   table with a convergent, bounded computation.
5. Emit final metadata in physical offset order.
6. Read and emit data clusters in increasing guest order, with explicit final
   padding.

Large tables are generated by chunk, one table at a time. No array of all L2
entries or all refcounts. Contiguous physical placement lets offsets be
computed instead of stored per cluster.

Document the number of passes over regions and their complexity. Each pass is
linear; never rescan the whole bitmap per L2 entry. A small per-L2 summary is
allowed within the budget.

## 6 QCOW2 conformance

Follow the values and masks of the official spec: big-endian encoding, aligned
offsets, zero reserved fields, header length, extensions and their terminator,
backing filename and its format extension.

L1/L2 tables reference only structures that exist. Absent entries keep
inheritance semantics. COPIED flags and refcounts are consistent. Every physical
cluster in use, refcount structures included, is counted; unused entries are
zero. No corrupt flag, dirty bit or lazy refcounts left to repair.

The file is final at its last byte. No header patch at the end, no QEMU repair,
rebase or rewrite pass. Same source, regions and options produce exactly the
same bytes, whatever the concurrency.

## 7 Memory and backpressure

Default working budget: 128 MiB for allocations the project controls: plan,
optional indexes, metadata buffers, payload buffers and the reorder window of
concurrent reads. It is not an RSS limit of the Go runtime or of dependencies;
measure and publish both.

All queues and buffers are bounded. A slow destination blocks source reads
without unbounded accumulation. Never one goroutine per region.

For 100 TiB at 64 KiB: full bitmap 200 MiB, L1 1.5625 MiB, all L2 tables
12.5 GiB. These are geometry figures, not required allocations. v1 must plan
this volume without a full bitmap or all L2 tables in RAM.

The budget is configurable. If the options need more than the budget, return
`ErrMemoryBudget` before emitting, stating the estimated minimum. Adapters and
dependencies also bound their replies and allocations. Caller buffers and final
storage are outside the writer's budget; this boundary is documented.

## 8 NBD adapter

Separate package, Unix socket and explicit export name. No TCP/TLS in v1.

Full mode: check the size and read the whole virtual space within the server's
request constraints. Incremental mode: negotiate `qemu:dirty-bitmap:<name>`, get
regions with `NBD_CMD_BLOCK_STATUS`, and keep extents whose DIRTY flag is set.
`base:allocation` is not a substitute for the change bitmap.

Zero detection uses `base:allocation` and only its ZERO bit (HOLE alone does not
guarantee zeros). In incremental mode it is queried only inside dirty extents.
If the server does not offer `base:allocation`, the adapter provides no zero
information and the library reads every allocated cluster.

Paginate replies; handle partial replies, multiple fragments, invalid zero
lengths, maximum request sizes and a missing context. Context IDs and flags are
interpreted per the protocol, not by assumed order. Never read or query
allocation outside the permitted regions of an incremental Proxmox export.

The adapter exposes positioned reads and a replayable enumeration. It requires a
stable view and bitmap for all passes. A plain NBD URI does not guarantee a
snapshot; that is the caller's responsibility, and it is documented.

No WRITE, WRITE_ZEROES or TRIM in v1. The library never modifies or resets the
bitmap. Without discard, Proxmox may need more fleecing storage: that is a
separate resource from the generator's RAM and local temp files.

## 9 CLI

```sh
qcowstream full --source-file disk.raw --output - > base.qcow2

qcowstream full --nbd-socket /run/source.sock --export scsi0 \
  --cluster-size 64KiB --memory-limit 128MiB --output - > base.qcow2

qcowstream incremental --nbd-socket /run/source.sock --export scsi0 \
  --bitmap backup-bitmap --backing-file base.qcow2 --backing-format qcow2 \
  --cluster-size 64KiB --memory-limit 128MiB --output - > inc-1.qcow2

qcowstream plan incremental --nbd-socket /run/source.sock --export scsi0 \
  --bitmap backup-bitmap --backing-file base.qcow2 --cluster-size 64KiB
```

`--cluster-size` defaults to `auto`. `--no-zero-detection` disables zero
detection.

Files in the examples are the explicit destination, not internal staging.
`--output -` writes only QCOW2 bytes to stdout; logs, progress and diagnostics go
to stderr. `plan` prints JSON metadata only and reads no disk data: final QCOW2
size, counters, geometry and estimated budget. Units are explicit, SI or IEC,
never mixing TB and TiB.

File source: full is required. To test an increment from a raw file, accept an
explicit, size-limited, validated JSON regions file; it is not the internal
representation of NBD. Reject incompatible flags, and increments without a
parent or without a bitmap/regions file.

Handle SIGINT/SIGTERM, broken pipe and cancellation with a non-zero exit and
released resources. Never overwrite an existing destination file without an
explicit flag. No atomic publication promise on stdout. No root, FUSE or
`/dev/nbd*` needed.

## 10 Errors and transactional contract

Typed errors for invalid options, unsupported geometry, unstable source, invalid
region, insufficient budget, overflow, missing NBD context, incomplete read,
incomplete write and cancellation. Causes are preserved by wrapping.

A short read without error is not end of data: loop until all bytes or a real
error. A short write is completed or reported without losing bytes. Context
cancellation and errors release connections and stop goroutines.

Every error detectable while planning happens before the first QCOW2 byte. A
network or destination error after emission starts leaves a partial stream: the
caller must abandon the object and not publish the snapshot. Success means every
byte was handed to the writer, not that Plakar committed it durably. The caller
handles commit, retention, parent relationship and Proxmox acknowledgement.

## 11 Proxmox and Plakar integration to prepare

No Perl or Plakar code here. Provide an example message with disk, socket,
export, size, effective bitmap mode, bitmap name and parent reference. It is a
future interface, not an official Proxmox JSON API.

The provider will ask for the NBD mechanism. In the Proxmox API reviewed, the
tracking request is `new` or `use` and the effective result `none`, `new` or
`reuse`. Only `reuse` with a known parent allows the incremental chain; `new`
requires a new full. The callback stays alive until the operation and the
external commit end. An interruption must never report success for a backup that
does not exist.

Plakar receives each image as a new immutable file: S0 holds base; S1 inherits
S0 and adds inc-1; S2 inherits S1 and adds inc-2. An external manifest maps disks
to parents. The relative name recorded in each image must match the restored
layout. v1 does no garbage collection of parents and no restore orchestration.

Document that the Proxmox restore reviewed expects a raw view: a future
integration must expose the QCOW2 chain as a disk over NBD or convert it.

## 11.1 Facts from the Proxmox sources

Read on 2026-11-13 from pve-qemu `2dc0af5` (QEMU 11.1.3), qemu-server
`7f1c6d7`, pve-storage `76a5d4012` and mirror_qemu `005c61c`.

- The backup-access bitmap is created with `PROXMOX_BACKUP_DEFAULT_CHUNK_SIZE`
  (4 MiB), not configurable (pve-qemu patch
  `0042-PVE-backup-implement-backup-access-setup-and-teardow.patch`). Merged
  dirty extents are 4 MiB-aligned, so any cluster size up to 2 MiB covers them.
  Extended L2 is not needed for Proxmox.
- The bitmap name is the target ID, `snapshot-access:<storage>`.
- The export is a plain QEMU NBD export of the snapshot-access node, on a Unix
  socket `<root>/<vmid>-nbd.backup-access`. It offers `base:allocation`.
- Reading or querying allocation outside dirty parts returns an error
  (`-EACCES`). Parts discarded by the provider also become unreadable. A fully
  clean bitmap makes the image inaccessible: the integration must produce an
  empty incremental without NBD in that case.
- Inside dirty extents, `base:allocation` reflects the original disk for areas
  not yet copied, and reports copied areas (on the fleecing image) as allocated.
  Zero detection is therefore exact when it answers ZERO, and may miss zeros.
- The NBD server advertises `CAN_MULTI_CONN` (no `max-connections` set) and a
  maximum request size of `min(max_transfer, 32 MiB)`.

## 12 Conformance and data tests

Unit tests and fuzzing: region validation, coalescing, limit sizes, arithmetic
overflow, budget, short writes, short reads, modified source, cancellation and
determinism. Tests check invariants and errors, not formulas copied from code.

Integration tests with documented QEMU versions:

- Full from a deterministic raw, `qemu-img check -f qcow2`, logical comparison
  with the raw.
- A base and at least two increments, with a block changed twice, a block that
  became zero, disjoint regions, writes on cluster and L2-table boundaries, and
  a partial last cluster.
- Each increment generated with its parent really absent. Parents restored at
  the expected paths, then every state of the chain checked.
- An empty increment, a zero-only increment and a zero full. Zeros must replace
  non-zero parent data.
- Conversion of the chain top to raw and byte equality with the expected
  reference; `qemu-img check` alone does not prove data.
- Pipe to a separate process to show no seek and no staging. Slow destination
  and mid-stream errors injected.
- NBD export with a known bitmap. The test server refuses any read outside dirty
  regions, like the Proxmox source.
- Missing bitmap, refused context and fragmented BLOCK_STATUS replies.

A compatibility matrix lists the QEMU versions actually tested. Tests without
QEMU stay runnable; integration tests are mandatory in a dedicated CI job, not
silently skipped.

## 13 Large volume validation

Deterministic virtual sources and replayable region maps for 100 TiB: no real
100 TiB file, no full transfer in CI.

Test full planning at that size, highly fragmented increments, the compatible
64 KiB geometry, refusal of incompatible geometries and table limits. A small
increment on a 100 TiB virtual disk is emitted and checked with QEMU without
writing 100 TiB of payload.

Measure controlled allocations and RSS separately. For 100 TiB/64 KiB the
default budget holds without per-cluster arrays. Publish planning time, number
of passes, memory and throughput on reproducible datasets, separating measured
results from estimates.

## 14 Deliverables

Go module with separate packages, CLI, API docs, README, full / incremental /
pipe / NBD examples, binary layout description, geometry limits guide, tests,
fuzzing and CI. A maintained Go version; dependencies and their licenses
documented. The project's own publication license is the owner's choice and is
not picked here.

Before declaring done, demonstrate: valid full, increment without parent access,
correct chain despite zeroed blocks, pipe output without seek, no forbidden NBD
read, budget respected on documented scenarios, and errors that prevent any
success report. List remaining limits precisely; do not call a test against a
generic NBD server "Proxmox support".

## 15 References

Check bits and limits in these sources before coding and record the version or
commit used:

- [QCOW2 specification](https://www.qemu.org/docs/master/interop/qcow2.html)
- [QEMU NBD metadata](https://www.qemu.org/docs/master/interop/nbd.html)
- [NBD protocol](https://github.com/NetworkBlockDevice/nbd/blob/master/doc/proto.md)
- [Proxmox backup provider API](https://github.com/proxmox/pve-storage/blob/master/src/PVE/BackupProvider/Plugin/Base.pm)
- [qemu-img](https://www.qemu.org/docs/master/tools/qemu-img.html)

---

# Part III — Design decisions

Settled during design review on 2026-11-13.

| # | Topic | Decision |
|---|---|---|
| D1 | Delivery | Whole project in one go, reviewed at the end. Several commits, each with tests proving that step. |
| D2 | Module | `github.com/brmzkw/qcowstream`. Core package at the module root (`qcowstream`), NBD adapter in `nbdsource`, CLI in `cmd/qcowstream`. |
| D3 | Concurrency | Supported, default `1`. See below. |
| D4 | Cluster size | Explicit sizes are honoured or refused; `auto` picks the largest compatible size. See below and D14. |
| D5 | QEMU targets | QEMU 8.2 (`ubuntu:24.04`) and QEMU 10.0 (`debian:trixie`), run with testcontainers. Header v3 with `header_length` 104, which both read. |
| D6 | CLI | Standard library `flag`, hand-written subcommand dispatch. Sizes accept IEC (`KiB`, `MiB`, …) and SI (`KB`, `MB`, …) suffixes, never confused. |
| D7 | Regions file | `{"virtual_size": N, "extents": [{"offset": N, "length": N}, …]}`, max 16 MiB, decoded with a streaming decoder on every walk. `--force` allows overwriting `--output`. |
| D8 | Language | Code, comments and docs in English. |
| D9 | NBD client | `github.com/digitalocean/go-nbd` (Apache-2.0, pure Go, only dependency `go-cmp`), wrapped in `nbdsource`. Compatible with ISC, MIT, BSD, Apache-2.0 and GPLv3 publication; not with GPLv2-only. |
| D10 | NBD tests | An in-process Go test server that refuses forbidden reads, plus real `qemu-nbd -B` in containers. |
| D11 | Test isolation | testcontainers lives in a separate module `integration/` so the library module does not depend on it. |
| D12 | Zero detection | On by default, from source metadata only: NBD `base:allocation` ZERO bit (inside dirty extents in incremental mode), `SEEK_HOLE`/`SEEK_DATA` for raw files. Full: zero clusters unallocated, all-zero L2 tables omitted. Incremental: zero-flagged L2 entries. `--no-zero-detection` disables it. |
| D13 | Extended L2 | Out of v1. Not needed for Proxmox (4 MiB bitmap). `ErrUnsupportedGranularity` mentions it for finer-grained sources. |
| D14 | Default cluster size | `auto`, preferring 2 MiB. Supersedes the explicit 64 KiB default first agreed for D4. |

## D3 Concurrency

The output is one ordered stream, so concurrency only applies to source reads.

- Contiguous allocated clusters are read in chunks of `BufferSize` (default
  `max(4 MiB, cluster size)`, a multiple of the cluster size). This is the main
  throughput lever on NBD, independently of concurrency.
- With `Concurrency = N`, up to N chunk reads are in flight, each into its own
  buffer from a ring of N slots. The emitter waits for slot `i mod N`, writes
  it, then releases it for job `i + N`. Memory is `N × BufferSize`, counted in
  the budget. A slow destination stops releasing slots, which stops reads.
  Output is identical for any N because order is fixed by guest offset.
- `go-nbd` serializes requests on one connection. The NBD adapter therefore
  opens a pool of N read connections to the same read-only export, plus one for
  BLOCK_STATUS. `--concurrency` is a CLI flag; docs recommend 4–8 for NBD.
- Exactly N worker goroutines plus the emitter, never one per region. All exit
  on error or cancellation.

## D4 Automatic cluster size

Coverage needs each merged region's start and end aligned to the cluster size,
except an end equal to the virtual size. The largest compatible size is the
largest power of two dividing every such boundary, computed in one pass.

- Explicit size: used as-is or fails with `ErrUnsupportedGranularity`.
- `auto` is the default (D14).
- `auto` in full mode: 2 MiB. For 100 TiB, L2 tables shrink from 12.5 GiB at
  64 KiB to 400 MiB.
- `auto` in incremental mode: `min(2 MiB, alignment)`, if it is ≥ 512 and the
  disk fits the L1, refcount table and offset limits at that size. Otherwise
  `ErrUnsupportedGranularity`, naming the alignment found and the minimum
  cluster size the disk size requires. On Proxmox (4 MiB bitmap) this gives
  2 MiB.
- Trade-off: zero detection works per cluster, so a 2 MiB cluster is skipped
  only if all of it is known to be zero.
- Each image in a chain has its own cluster size. QEMU handles mixed chains, so a
  full at 2 MiB and an incremental at 64 KiB coexist.

## Further decisions made while writing this plan

- **Plan walks twice** whenever it has a source (`Changed` and/or `Zeros`).
  Walk 1 validates, merges, computes the alignment, counts clusters for every
  candidate cluster size, and fingerprints. Walk 2 recomputes the fingerprint
  and builds the touched-L1 bitset for the chosen size. A walk reads `Changed`
  and `Zeros` in lockstep: two ordered streams merged with `iter.Pull`, which
  costs one goroutine per stream. Comparing fingerprints is what rejects
  single-pass sources before the first byte, as §3 and §10 require. The bitset
  costs at most 512 KiB (4 Mi L1 entries, QEMU's limit).
- **Write walks twice** in the same cases: once to emit L2 tables, once to read
  and emit data. Each walk recomputes the fingerprints; a mismatch fails the
  operation. Data-walk overruns (more clusters than planned) fail immediately.
  Total: 4 linear walks when there is a source, 0 for a full without zero
  information.
- **Cluster classes**: per guest cluster, one of
  - *data*: allocated and read;
  - *zero*: incremental only, zero-flagged;
  - *absent*: unallocated (inherit in an incremental, zeros in a full).

  A cluster is *zero* only if it is entirely inside merged `Zeros`. In an
  incremental, `Zeros` outside `Changed` is ignored. The NBD adapter does not
  produce such regions anyway, because it queries `base:allocation` only inside
  dirty extents.
- **`BackingFormat`**: empty means `qcow2`; any other value is rejected. Backing
  file: non-empty, relative, at most 1023 bytes, no NUL, must fit the header
  cluster with the extensions.
- **Virtual size**: always explicit in `Options`. Adapters expose `Size()` and the
  CLI fills it. `Write` checks it against the source when the source implements
  `Size()`.
- **File output in the CLI**: created with `O_EXCL` unless `--force`, removed on
  failure so no partial file looks like a valid image.
- **SIGPIPE**: the CLI handles it through `signal.Notify`, so a broken stdout
  pipe returns `EPIPE` as an error instead of killing the process silently.
- **Repository**: new repo with `git init`, branch `main`, one commit per step
  below.

---

# Part IV — Implementation plan

## Package layout

```text
go.mod                       github.com/brmzkw/qcowstream (go 1.26)
qcowstream.go                Options, Mode, Plan, Write, Generate, Stats
errors.go                    sentinel errors and detailed error types
extent.go                    Extent, ExtentSource, validation, merging, fingerprint
geometry.go                  cluster size, limits, layout and refcount convergence
budget.go                    memory estimate
emit.go                      header, refcounts, L1, L2 emitters
data.go                      bounded concurrent reader and ordered emitter
source.go                    ReaderAtSource, SliceExtents, Sizer
internal/qcow2test/          test-only QCOW2 parser and invariant checker
internal/virtual/            deterministic virtual sources and region maps
nbdsource/                   go-nbd wrapper: dial, pool, reads, BLOCK_STATUS walk
nbdsource/internal/nbdtest/  in-process NBD test server
cmd/qcowstream/              CLI
integration/                 separate module: testcontainers + QEMU tests
docs/                        layout.md, geometry.md, proxmox.md, measurements.md,
                             compatibility.md
.github/workflows/ci.yml     unit job and integration job
```

## Binary layout

All offsets are cluster-aligned. Writing is strictly increasing; gaps are
explicit zero bytes.

| Cluster range | Content |
|---|---|
| 0 | Header (104 bytes), backing-format extension `0xe2792aca` (incremental), end-of-extensions marker, backing filename, zero padding |
| 1 … | Refcount table (`refcount_table_clusters`) |
| then | Refcount blocks, all of them. Entries `0 … total-1` are 1, the rest 0 |
| then | L1 table, sized for the whole virtual disk, padded to a cluster |
| then | L2 tables of touched L1 indices (holding at least one data or zero cluster), in L1 order |
| then | Data clusters in guest order; the last one zero-padded past the virtual end |

- Host offset of the k-th data cluster: `data_start + k × cs`, where k is the
  running count of data clusters during the walk. For a full image with no
  zero information, k is the guest cluster index and no walk is needed.
- L1 entry j: `l2_start + rank(j) × cs` with bit 63 (COPIED), where rank comes
  from the touched-L1 bitset.
- L2 entry:
  - data cluster: host offset with bit 63 (COPIED)
  - zero cluster: `0x1`, the "reads as zeros" flag with no host offset
  - absent: 0, which means inherit from the backing file, or zeros in a full
- Refcount convergence: `total = 1 + rt + rb + l1 + l2 + data`,
  `rb = ceil(total / (cs/2))`, `rt = ceil(rb × 8 / cs)`. Iterate from `rt = rb = 0`.
  The sequence is monotone and bounded; cap the iterations and fail with
  `ErrOverflow` if it does not converge.
- Output size is exactly `total × cs`.

Limits to confirm against `block/qcow2.h` at the tested versions and record in
`docs/geometry.md`:
- `cluster_bits` 9…21
- L1 ≤ 32 MiB
- refcount table ≤ 8 MiB
- host offsets < 2^56
- backing filename ≤ 1023 bytes, inside cluster 0
- `l1_size` must cover the virtual size

## Commit sequence

Each commit builds, passes `go vet` and `go test ./...`, and carries its own
tests. Integration commits also pass `go test ./...` in `integration/` with
Docker.

1. **`docs: rewrite spec in english with decisions and plan`**
   This file.

2. **`qcowstream: validate, merge and fingerprint extents`**
   `Extent`, `ExtentSource`, `SliceExtents`, a streaming merger, SHA-256
   fingerprint and counters, `ErrInvalidRegion`, `ErrOverflow`.
   Tests: ordering, overlap, zero length, end overflow, out-of-disk, adjacent
   merge, empty list. A fuzz test checks that merged output covers exactly the
   union of the input, stays ordered and never has adjacent extents.

3. **`qcowstream: plan full images`**
   `Options`, validation, geometry limits, refcount convergence, memory
   estimate, `ErrMemoryBudget`, `ErrInvalidOptions`, an immutable `ImagePlan`
   with exact size and counters.
   Tests:
   - 100 TiB at 64 KiB and at 2 MiB plans without per-cluster allocation
     (allocation counter asserted).
   - Limits on both sides of L1 and refcount-table maximums.
   - A cluster size outside 512 B–2 MiB, or not a power of two, is rejected.
   - Virtual size not a multiple of 512, or zero, is rejected.
   - The budget error reports the needed minimum.
   - A fuzz test checks that convergence always reaches a fixed point whose
     refcount blocks cover `total` clusters.

4. **`qcowstream: write full images`**
   Header, refcount, L1 and L2 emitters by chunk; sequential data emission;
   `ReaderAtSource`; short-read looping; short-write handling; context checks.
   Also adds `internal/qcow2test`, which parses an image from bytes and checks:
   - the header fields
   - every cluster referenced by L1/L2 and refcount structures has refcount
     exactly 1, and every other cluster has 0
   - COPIED flags are set
   - data read through L1/L2 equals the source
   Tests:
   - Several sizes, including a partial last cluster and multiple L2 tables.
   - Bytes written equal `plan.Size`.
   - Output is identical across runs.
   - Readers returning short counts or `(n, io.EOF)` early →
     `ErrIncompleteRead`.
   - Writers returning short writes without an error → `io.ErrShortWrite` as
     `ErrIncompleteWrite`.
   - Cancellation mid-stream → `ErrCanceled`.
   - A writer that is only `io.Writer` (no Seek) is used throughout.

5. **`integration: check full images with qemu-img in containers`**
   `integration/` module with testcontainers.
   - Images `ubuntu:24.04` and `debian:trixie` with `qemu-utils`.
   - The CLI is cross-compiled for Linux and copied in. Until commit 15, a small
     test-only driver binary built from the library stands in for it.
   - Tests:
     - deterministic raw → full → `qemu-img check` → `qemu-img convert` to raw →
       byte compare
     - zero full
     - output piped to `cat` and to `sha256sum`, compared with direct output

6. **`ci: run unit and integration jobs`**
   - Unit job: `go vet`, `go test -race ./...`, and each fuzz target for 30 s.
   - Integration job: Docker on `ubuntu-latest`, `go test ./...` in
     `integration/`. The job fails if Docker is unavailable.

7. **`qcowstream: plan incremental images`**
   Two-walk plan, alignment detection, auto cluster size,
   `ErrUnsupportedGranularity` with diagnostics, touched-L1 bitset, backing
   name validation, rejection of single-pass sources (`ErrUnstableSource`).
   Tests:
   - Covered and uncovered clusters.
   - An end at the virtual size that is not aligned.
   - An adjacent split across reply boundaries still counts as covered.
   - Auto choice: 2 MiB for 4 MiB-aligned regions, 64 KiB for 64 KiB-aligned
     ones, and refusal below 512 bytes.
   - 100 TiB with 4 KiB-aligned regions is refused with an explanation that
     mentions Extended L2.
   - A one-shot source is rejected before any output.
   - An empty list gives a valid plan with 0 data clusters.

8. **`qcowstream: write incremental images`**
   Backing extension, L2 walk, data walk, fingerprint verification, overrun
   detection.
   Tests, all with `internal/qcow2test`:
   - allocated clusters equal exactly the covered clusters
   - unallocated entries are zero
   - zero-filled regions are allocated, not inherited
   - the empty incremental
   - a source whose second walk differs fails with `ErrUnstableSource`
   - the source reader records every read, and the test asserts that no byte
     outside the regions was read

9. **`integration: check incremental chains with absent parents`**
   - raw0 → base. Mutate to raw1 → inc-1, generated in a directory without the
     base. Mutate to raw2 → inc-2.
   - Mutations: a block changed twice, a block zeroed, disjoint regions, writes
     on L2-table boundaries, a partial last cluster.
   - Each image is checked alone with backing disabled (`json:` filename with
     `"backing": null`). The parents are then put in place, every chain state
     is converted and byte-compared.
   - Empty increment, zero-only increment.

10. **`qcowstream: skip clusters known to be zero`**
    `Extents.Zeros`, walking it in lockstep with `Changed` through `iter.Pull`,
    the three cluster classes, omitted all-zero L2 tables in a full image,
    zero-flagged entries in an incremental. Also a raw-file zero source built on
    `SEEK_HOLE`/`SEEK_DATA`.
    Unit tests:
    - A source reader that fails on any read inside a known-zero cluster.
    - Partly zero clusters are read.
    - In an incremental, `Zeros` outside `Changed` changes nothing.
    - Zero clusters in an incremental carry the flag and no host offset.
    - A full image with an all-zero L2 range omits that table.
    - Plan size and counters match the emitted bytes.
    - A zero source whose second walk differs fails.

    Integration (`integration/`):
    - a sparse raw file gives a full whose `qemu-img map` shows the holes, and
      converts back to the same bytes
    - a zero-flagged incremental over a non-zero parent reads as zeros through
      the chain

11. **`qcowstream: read sources with bounded concurrency`**
    Slot-ring reader, `BufferSize`, `Concurrency` validation, budget accounting.
    Tests:
    - Byte-identical output for N = 1, 4 and 16.
    - A slow writer: the reader never has more than N chunks in flight, checked
      by an instrumented source.
    - A goroutine leak check after cancellation and after a mid-stream source
      error.
    - The budget rejects N × BufferSize above the limit.

12. **`nbdsource: read full exports over NBD`**
    Dial with export name, block-size constraints, request splitting, connection
    pool, cancellation through deadlines (the connection is closed after an
    interrupted request), `Size()`. Also adds the in-process test server
    `nbdsource/internal/nbdtest`.
    Tests:
    - reads honour the server's min and max block sizes
    - a server that stalls is interrupted by context cancellation
    - pool size equals concurrency
    - full generation over NBD equals generation from the same bytes as a file

13. **`nbdsource: enumerate dirty regions with BLOCK_STATUS`**
    Meta context negotiation for `qemu:dirty-bitmap:<name>`, ID check, windowed
    and paginated BLOCK_STATUS, clipping of a last extent that overruns the
    request, zero-length rejection, DIRTY selection, `ErrMissingContext`.
    Tests against the test server:
    - fragmented and short replies
    - a refused context
    - an unknown bitmap
    - extents with a wrong context ID
    - the server refuses any READ outside dirty regions and the test fails on
      any such attempt

14. **`nbdsource: report zero regions from base:allocation`**
    Negotiates `base:allocation` next to the bitmap context and keeps only the
    ZERO bit. In incremental mode it queries only inside dirty extents. If the
    server does not offer the context, there is no zero information.
    Tests against the test server:
    - the server refuses, and the test fails on, any `base:allocation` query
      outside dirty extents
    - HOLE without ZERO is not treated as zero
    - a server without `base:allocation` still produces a correct image
    - fragmented allocation replies

15. **`cmd/qcowstream: add full, incremental and plan commands`**
    Flags per §9 and D6/D7, regions file, `--concurrency`, `--buffer-size`,
    `--cluster-size auto|<size>`, `--no-zero-detection`, `--force`, signals,
    SIGPIPE, stderr diagnostics, plan JSON with IEC/SI labelled sizes.
    Tests (the binary is built and run with `os/exec`):
    - stdout carries only image bytes
    - incompatible flags are rejected
    - an existing output is refused without `--force`
    - a partial file is removed on failure
    - SIGINT gives a non-zero exit
    - a broken pipe gives a non-zero exit
    - `plan` reads no data (the file source is opened, but reads are counted
      through a test hook)

16. **`integration: generate images from qemu-nbd dirty bitmaps`**
    In the container:
    - write data with `qemu-io` on a sparse source disk, run a full over
      `qemu-nbd` (zero detection on and off, same converted bytes)
    - `qemu-img bitmap --add -g 4M`, the Proxmox granularity, write more
      (including `write -z`), run an incremental over `qemu-nbd -B`
    - check the chain against `qemu-img convert` of the source disk
    Also a missing bitmap name gives a clean error.

17. **`qcowstream: validate 100 TiB volumes`**
    `internal/virtual` deterministic sources (data derived from offset) and
    procedural region maps.
    Tests:
    - full plan at 100 TiB
    - highly fragmented incremental plan
    - geometry refusal
    - emission of a small incremental on a 100 TiB disk, checked by
      `internal/qcow2test` and, in `integration/`, by `qemu-img check` with
      backing disabled plus `qemu-io -c "read -P …"` on the written regions
    - the budget holds (allocation counter)

18. **`bench: measure planning, memory and throughput`**
    Benchmarks plus a small `internal/cmd/qcowbench` reporting:
    - plan time
    - walks
    - controlled bytes vs `runtime.MemStats` / RSS
    - MiB/s for file and NBD at several concurrencies

    Results go in `docs/measurements.md`, separating what was measured from
    what was estimated.

19. **`docs: add README, layout, geometry, Proxmox notes and examples`**
    README; `docs/layout.md`, `geometry.md` and `compatibility.md` (QEMU
    versions actually tested); `proxmox.md` with the example integration
    message and bitmap modes; `DEPENDENCIES.md` with licenses; `Example*`
    functions for full, incremental, pipe and NBD.
    Also lists the remaining limits:
    - no Extended L2
    - no resize
    - no TCP/TLS
    - the stability of the NBD view is the caller's responsibility
    - not "Proxmox support" until tested against a real Proxmox export

## Definition of done

- Every item of §14 is demonstrated by a named test.
- Unit and integration suites are green locally with Docker.
- `docs/compatibility.md` lists the exact QEMU versions run.
- Remaining limits are listed in the README.
