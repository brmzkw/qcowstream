package qcowstream

import (
	"context"
	"fmt"
	"math/bits"
)

// clusterRun is a run of consecutive guest clusters of one class.
type clusterRun struct {
	first, count uint64
}

// walkPrints are the fingerprints of one walk over the region sources.
type walkPrints struct {
	changed Fingerprint
}

// walkRuns walks the region sources once and calls fn with the runs of data
// clusters in increasing guest order. Every merged changed extent must cover
// whole clusters, except for an end at the virtual size.
func (g geometry) walkRuns(ctx context.Context, ext Extents, fn func(clusterRun) error) (walkPrints, error) {
	var wp walkPrints
	var err error
	cs := g.clusterSize
	wp.changed, err = MergeExtents(ctx, ext.Changed, g.virtualSize, func(e Extent) error {
		if !g.aligned(e) {
			return fmt.Errorf("%w: extent [%d, %d) is not aligned to the %d-byte clusters",
				ErrUnstableSource, e.Offset, e.End(), cs)
		}
		first := e.Offset / cs
		return fn(clusterRun{first: first, count: ceilDiv(e.End(), cs) - first})
	})
	return wp, err
}

// aligned reports whether e covers whole clusters.
func (g geometry) aligned(e Extent) bool {
	cs := g.clusterSize
	return e.Offset%cs == 0 && (e.End()%cs == 0 || e.End() == g.virtualSize)
}

// boundaryAlignment returns the largest power of two dividing the boundaries
// of e, ignoring an end at the virtual size. It returns 0 for no constraint.
func boundaryAlignment(e Extent, virtualSize uint64) uint64 {
	m := e.Offset
	if e.End() != virtualSize {
		m |= e.End()
	}
	if m == 0 {
		return 0
	}
	return 1 << bits.TrailingZeros64(m)
}

// bitset is a fixed-size set of L1 indices.
type bitset []uint64

func newBitset(n uint64) bitset    { return make(bitset, ceilDiv(n, 64)) }
func (b bitset) set(i uint64)      { b[i/64] |= 1 << (i % 64) }
func (b bitset) has(i uint64) bool { return b[i/64]&(1<<(i%64)) != 0 }
func (b bitset) bytes() uint64     { return uint64(len(b)) * 8 }
func (b bitset) setRange(lo, hi uint64) {
	for i := lo; i < hi; i++ {
		b.set(i)
	}
}
func (b bitset) count() uint64 {
	var n int
	for _, w := range b {
		n += bits.OnesCount64(w)
	}
	return uint64(n)
}
