package qcowstream

import (
	"context"
)

// ImagePlan is the complete, immutable layout of one image. It is computed
// without reading payload and fixes every byte of the metadata.
type ImagePlan struct {
	opts   Options
	geo    geometry
	layout layout

	backingFormat string
	sourceBytes   uint64
	memory        uint64
	walks         int
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
		BackingFormat:         p.backingFormat,
		GuestClusters:         p.geo.guestClusters,
		DataClusters:          p.layout.dataClusters,
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
		PlanWalks:             p.walks,
		WriteWalks:            p.writeWalks(),
	}
}

// Size returns the exact number of bytes Write emits.
func (p *ImagePlan) Size() uint64 { return p.layout.imageSize }

func (p *ImagePlan) writeWalks() int { return 0 }

// Plan validates the options, chooses the geometry and lays out the image. It
// reads no payload. Every error it can detect is reported here, before any
// byte is written.
func Plan(ctx context.Context, ext Extents, opts Options) (*ImagePlan, error) {
	if err := validateOptions(&opts, ext); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, canceled(ctx)
	}
	if opts.Mode != Full {
		return nil, invalid("incremental images are not supported yet")
	}
	cs := opts.ClusterSize
	if cs == ClusterSizeAuto {
		cs = autoClusterSize
	}
	geo, err := newGeometry(opts.VirtualSize, cs)
	if err != nil {
		return nil, err
	}
	lay, err := newLayout(geo, geo.l1Entries, geo.guestClusters)
	if err != nil {
		return nil, err
	}
	p := &ImagePlan{geo: geo, layout: lay, sourceBytes: opts.VirtualSize}
	opts.ClusterSize = cs
	if err := p.setResources(&opts); err != nil {
		return nil, err
	}
	p.opts = opts
	return p, nil
}

// setResources fills the resource defaults and checks the memory budget.
func (p *ImagePlan) setResources(opts *Options) error {
	cs := p.geo.clusterSize
	if opts.MemoryLimit == 0 {
		opts.MemoryLimit = DefaultMemoryLimit
	}
	if opts.Concurrency == 0 {
		opts.Concurrency = 1
	}
	if opts.BufferSize == 0 {
		opts.BufferSize = max(DefaultBufferSize, cs)
	} else if opts.BufferSize%cs != 0 {
		return invalid("buffer size %d must be a multiple of the cluster size %d", opts.BufferSize, cs)
	}
	payload, err := mul64(opts.BufferSize, uint64(opts.Concurrency))
	if err != nil {
		return err
	}
	// Fixed overhead covers the plan and the walkers; one cluster buffers
	// metadata while it is emitted.
	const overhead = 64 << 10
	p.memory, err = sum(overhead, cs, payload)
	if err != nil {
		return err
	}
	if p.memory > opts.MemoryLimit {
		return &MemoryBudgetError{Required: p.memory, Limit: opts.MemoryLimit}
	}
	return nil
}
