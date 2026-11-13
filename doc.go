// Package qcowstream writes QCOW2 images as one sequential stream: a full
// image of a disk, or an incremental image holding changed regions and
// naming its parent. It needs no seek, no temporary file and no access to
// the parent, and works in bounded memory for any disk size QCOW2 allows.
//
// # Plan, then write
//
// Plan validates the options, chooses the geometry and fixes the layout of
// every structure without reading payload. Every error it can detect is
// reported before any byte is written. Write then emits the header, refcount
// table and blocks, L1 table, L2 tables and data clusters, in that order,
// each exactly once. The output is final at its last byte: there is no
// header patch and no repair pass. The same source, regions and options
// produce the same bytes whatever the concurrency.
//
// # Walks
//
// Region sources are walked, never stored. With region sources, Plan walks
// them twice: once to validate them and find their alignment, which decides
// the cluster size, and once to count clusters and mark L1 entries. Comparing
// both walks' fingerprints rejects sources that cannot replay. Write walks
// twice more: once for the L2 tables, once for the data. Each walk is linear
// in the number of extents; Changed and Zeros are read in lockstep. A full
// image without zero information needs no walk at all.
//
// # Memory
//
// The working memory Write controls is the plan, the touched-L1 bitset (at
// most 512 KiB), one metadata cluster and Concurrency × BufferSize of read
// buffers. Plan estimates it and returns ErrMemoryBudget beyond
// Options.MemoryLimit. The destination's buffers, the caller's source and the
// Go runtime are outside this budget.
//
// # Failures
//
// An error from Write leaves a partial stream that must be discarded. A nil
// error means every byte was handed to the writer, not that it is stored
// durably.
package qcowstream
