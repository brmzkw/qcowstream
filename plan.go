package qcowstream

import (
	"context"
	"fmt"
)

// ImagePlan is the complete, immutable layout of one image. It is computed
// without reading payload and fixes every byte of the metadata.
type ImagePlan struct {
	opts   Options
	geo    geometry
	layout layout

	sourceBytes uint64
	memory      uint64

	// touchedL1 marks L1 entries with an L2 table; nil means all of them.
	touchedL1    bitset
	prints       walkPrints
	zeroClusters uint64
	hasZeros     bool // planned with a zero source
}

// PlanInfo describes a plan. All sizes are in bytes.
type PlanInfo struct {
	Mode          string `json:"mode"`
	VirtualSize   uint64 `json:"virtual_size"`
	ClusterSize   uint64 `json:"cluster_size"`
	ImageSize     uint64 `json:"image_size"`
	BackingFile   string `json:"backing_file,omitempty"`
	BackingFormat string `json:"backing_format,omitempty"`

	GuestClusters         uint64 `json:"guest_clusters"`
	DataClusters          uint64 `json:"data_clusters"`
	ZeroClusters          uint64 `json:"zero_clusters"` // known zero: unallocated in a full image, zero-flagged in an incremental
	L1Entries             uint64 `json:"l1_entries"`
	L2Tables              uint64 `json:"l2_tables"`
	RefcountBlocks        uint64 `json:"refcount_blocks"`
	RefcountTableClusters uint64 `json:"refcount_table_clusters"`
	TotalClusters         uint64 `json:"total_clusters"`
	SourceBytes           uint64 `json:"source_bytes"`

	MemoryEstimate uint64 `json:"memory_estimate"`
	MemoryLimit    uint64 `json:"memory_limit"`
	BufferSize     uint64 `json:"buffer_size"`
	Concurrency    int    `json:"concurrency"`

	PlanWalks  int `json:"plan_walks"`  // walks over the region sources done by Plan
	WriteWalks int `json:"write_walks"` // walks Write will do
}

// Info returns a description of the plan.
func (p *ImagePlan) Info() PlanInfo {
	return PlanInfo{
		Mode:                  p.opts.Mode.String(),
		VirtualSize:           p.geo.virtualSize,
		ClusterSize:           p.geo.clusterSize,
		ImageSize:             p.layout.imageSize,
		BackingFile:           p.opts.BackingFile,
		BackingFormat:         p.opts.BackingFormat,
		GuestClusters:         p.geo.guestClusters,
		DataClusters:          p.layout.dataClusters,
		ZeroClusters:          p.zeroClusters,
		L1Entries:             p.geo.l1Entries,
		L2Tables:              p.layout.l2Clusters,
		RefcountBlocks:        p.layout.rbClusters,
		RefcountTableClusters: p.layout.rtClusters,
		TotalClusters:         p.layout.total,
		SourceBytes:           p.sourceBytes,
		MemoryEstimate:        p.memory,
		MemoryLimit:           p.opts.MemoryLimit,
		BufferSize:            p.opts.BufferSize,
		Concurrency:           p.opts.Concurrency,
		PlanWalks:             p.walks(),
		WriteWalks:            p.walks(),
	}
}

// Size returns the exact number of bytes Write emits.
func (p *ImagePlan) Size() uint64 { return p.layout.imageSize }

// walks returns the walks over the region sources Plan did, which is also
// the number Write does: Plan validates then counts, Write emits L2 tables
// then data. Without region sources, no walk touches anything.
func (p *ImagePlan) walks() int {
	if p.touchedL1 == nil {
		return 0
	}
	return 2
}

// touched reports whether L1 entry j has an L2 table.
func (p *ImagePlan) touched(j uint64) bool {
	return p.touchedL1 == nil || p.touchedL1.has(j)
}

// Plan validates the options, chooses the geometry and lays out the image. It
// reads no payload. Every error it can detect is reported here, before any
// byte is written.
func Plan(ctx context.Context, ext Extents, opts Options) (p *ImagePlan, err error) {
	// Sources report cancellation as plain context errors; classify here.
	defer func() { err = ctxErr(ctx, err) }()
	if err := validateOptions(&opts, ext); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, canceled(ctx)
	}
	if opts.Mode == Full && ext.Zeros == nil {
		return planFull(opts)
	}
	return planWalked(ctx, ext, opts)
}

// planFull plans a full image without zero information: every guest cluster
// is data, so no walk is needed.
func planFull(opts Options) (*ImagePlan, error) {
	geo, err := geometryFor(opts, 0)
	if err != nil {
		return nil, err
	}
	lay, err := newLayout(geo, geo.l1Entries, geo.guestClusters)
	if err != nil {
		return nil, err
	}
	p := &ImagePlan{geo: geo, layout: lay, sourceBytes: opts.VirtualSize}
	return p.finish(opts)
}

// planWalked walks the region sources twice. The first walk validates them
// and finds the alignment of the changed regions, which decides the cluster
// size. The second counts clusters and marks L1 entries at that size.
// Comparing the fingerprints of both walks rejects sources that cannot
// replay, before any output.
func planWalked(ctx context.Context, ext Extents, opts Options) (*ImagePlan, error) {
	var first walkPrints
	var align uint64 // 0: no constraint
	var err error
	if ext.Changed != nil {
		first.changed, err = MergeExtents(ctx, ext.Changed, opts.VirtualSize, func(e Extent) error {
			if a := boundaryAlignment(e, opts.VirtualSize); a != 0 && (align == 0 || a < align) {
				align = a
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	if ext.Zeros != nil {
		if first.zeros, err = MergeExtents(ctx, ext.Zeros, opts.VirtualSize, nil); err != nil {
			return nil, err
		}
	}
	geo, err := geometryFor(opts, align)
	if err != nil {
		return nil, err
	}

	touched := newBitset(geo.l1Entries)
	var data, zero, source uint64
	prints, err := geo.walkRuns(ctx, ext, func(r clusterRun) error {
		if r.zero {
			zero += r.count
		} else {
			data += r.count
			source += geo.runBytes(r)
		}
		touched.setRange(r.first/geo.l2Entries, (r.first+r.count-1)/geo.l2Entries+1)
		return nil
	})
	if err != nil {
		return nil, err
	}
	if ext.Changed == nil {
		zero = geo.guestClusters - data // unallocated in a full image
	}
	if prints != first {
		return nil, fmt.Errorf("%w: the region sources differ between two walks; they must be replayable and stable",
			ErrUnstableSource)
	}
	lay, err := newLayout(geo, touched.count(), data)
	if err != nil {
		return nil, err
	}
	p := &ImagePlan{
		geo: geo, layout: lay, sourceBytes: source, zeroClusters: zero,
		touchedL1: touched, prints: prints, hasZeros: ext.Zeros != nil,
	}
	return p.finish(opts)
}

// geometryFor chooses the cluster size for regions whose boundaries are
// multiples of align (0 for no constraint) and checks every limit that
// depends on it.
func geometryFor(opts Options, align uint64) (geometry, error) {
	cs, err := chooseClusterSize(opts, align)
	if err != nil {
		return geometry{}, err
	}
	geo, err := newGeometry(opts.VirtualSize, cs, align)
	if err != nil {
		return geometry{}, err
	}
	if n := headerBytes(opts.BackingFile, opts.BackingFormat); n > cs {
		return geometry{}, invalid("header and backing file name need %d bytes, more than one %d-byte cluster", n, cs)
	}
	return geo, nil
}

// chooseClusterSize picks the cluster size for regions whose boundaries are
// multiples of align (0 for no constraint).
func chooseClusterSize(opts Options, align uint64) (uint64, error) {
	if cs := opts.ClusterSize; cs != ClusterSizeAuto {
		if align != 0 && align < cs {
			return 0, &GranularityError{
				ClusterSize: cs, Alignment: align, MinClusterSize: minClusterSizeFor(opts.VirtualSize),
				Reason: fmt.Sprintf("changed regions are aligned to %d bytes, finer than the %d-byte clusters; "+
					"filling the clusters would read unchanged bytes. Use a cluster size of at most %d, "+
					"or a source with coarser tracking (Extended L2 subclusters are not supported)", align, cs, align),
			}
		}
		return cs, nil
	}
	if opts.Mode == Full {
		return autoClusterSize, nil
	}
	cs := uint64(autoClusterSize)
	if align != 0 {
		cs = min(cs, align)
	}
	if cs < MinClusterSize {
		return 0, &GranularityError{
			Alignment: align, MinClusterSize: minClusterSizeFor(opts.VirtualSize),
			Reason: fmt.Sprintf("changed regions are aligned to %d bytes, below the %d-byte QCOW2 minimum cluster size",
				align, MinClusterSize),
		}
	}
	if min := minClusterSizeFor(opts.VirtualSize); min == 0 || cs < min {
		return 0, &GranularityError{
			ClusterSize: cs, Alignment: align, MinClusterSize: min,
			Reason: fmt.Sprintf("changed regions are aligned to %d bytes, but a %d-byte disk needs clusters of at least %d bytes "+
				"for its L1 table. Standard L2 tables cannot represent this; it needs Extended L2 subclusters "+
				"(not supported) or a source with coarser tracking", align, opts.VirtualSize, min),
		}
	}
	return cs, nil
}

// finish records the options with their defaults and checks the memory
// budget.
func (p *ImagePlan) finish(opts Options) (*ImagePlan, error) {
	cs := p.geo.clusterSize
	opts.ClusterSize = cs
	if opts.MemoryLimit == 0 {
		opts.MemoryLimit = DefaultMemoryLimit
	}
	if opts.Concurrency == 0 {
		opts.Concurrency = 1
	}
	if opts.BufferSize == 0 {
		opts.BufferSize = max(DefaultBufferSize, cs)
	} else if opts.BufferSize%cs != 0 {
		return nil, invalid("buffer size %d must be a multiple of the cluster size %d", opts.BufferSize, cs)
	}
	// One buffer per read in flight, plus the one being written.
	payload, err := mul64(opts.BufferSize, uint64(opts.Concurrency)+1)
	if err != nil {
		return nil, err
	}
	// Fixed overhead covers the plan and the walkers; one cluster buffers
	// metadata while it is emitted.
	const overhead = 64 << 10
	p.memory, err = sum(overhead, cs, payload, p.touchedL1.bytes())
	if err != nil {
		return nil, err
	}
	if p.memory > opts.MemoryLimit {
		return nil, &MemoryBudgetError{Required: p.memory, Limit: opts.MemoryLimit}
	}
	p.opts = opts
	return p, nil
}
