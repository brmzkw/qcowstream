package qcowstream

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"math/bits"
)

// clusterRun is a run of consecutive guest clusters of one class. Clusters
// not covered by any run are absent.
type clusterRun struct {
	first, count uint64
	zero         bool // known to read as zeros; otherwise data to read
}

// walkPrints are the fingerprints of one walk over the region sources.
type walkPrints struct {
	changed Fingerprint
	zeros   Fingerprint
}

var errStopPull = errors.New("qcowstream: stop pulling zeros")

// walkRuns walks the region sources once and calls fn with data and zero
// runs in increasing guest order.
//
// The domain is the whole disk for a full image, or the changed regions for
// an incremental one; each merged changed extent must cover whole clusters,
// except for an end at the virtual size. Within the domain, a cluster lying
// entirely inside the merged zero regions is a zero run, the rest is data.
// The zero source is pulled in lockstep, so both walks stay linear.
func (g geometry) walkRuns(ctx context.Context, ext Extents, fn func(clusterRun) error) (walkPrints, error) {
	var wp walkPrints
	z := g.zeroCursor(ctx, ext.Zeros)
	defer z.stop()

	split := func(first, end uint64) error {
		for pos := first; pos < end; {
			z.skipBefore(pos)
			r := clusterRun{first: pos, count: end - pos}
			switch {
			case z.ok && z.lo <= pos:
				r.count, r.zero = min(end, z.hi)-pos, true
			case z.ok && z.lo < end:
				r.count = z.lo - pos
			}
			if err := fn(r); err != nil {
				return err
			}
			pos += r.count
		}
		return nil
	}

	var err error
	if ext.Changed == nil {
		err = split(0, g.guestClusters)
	} else {
		cs := g.clusterSize
		wp.changed, err = MergeExtents(ctx, ext.Changed, g.virtualSize, func(e Extent) error {
			if !g.aligned(e) {
				return fmt.Errorf("%w: extent [%d, %d) is not aligned to the %d-byte clusters",
					ErrUnstableSource, e.Offset, e.End(), cs)
			}
			return split(e.Offset/cs, ceilDiv(e.End(), cs))
		})
	}
	if err != nil {
		return wp, err
	}
	wp.zeros, err = z.finish()
	return wp, err
}

// zeroCursor pulls merged zero extents and converts them to the range of
// clusters [lo, hi) they cover entirely.
type zeroCursor struct {
	g      geometry
	next   func() (Extent, bool)
	stopFn func()
	fp     Fingerprint
	err    error
	lo, hi uint64
	ok     bool
}

func (g geometry) zeroCursor(ctx context.Context, src ExtentSource) *zeroCursor {
	z := &zeroCursor{g: g}
	if src == nil {
		z.next = func() (Extent, bool) { return Extent{}, false }
		z.stopFn = func() {}
		return z
	}
	seq := func(yield func(Extent) bool) {
		z.fp, z.err = MergeExtents(ctx, src, g.virtualSize, func(e Extent) error {
			if !yield(e) {
				return errStopPull
			}
			return nil
		})
	}
	z.next, z.stopFn = iter.Pull(seq)
	return z
}

// skipBefore advances to the first non-empty cluster range ending after pos.
func (z *zeroCursor) skipBefore(pos uint64) {
	for !z.ok || z.hi <= pos {
		e, ok := z.next()
		if !ok {
			z.ok = false
			return
		}
		z.lo, z.hi = ceilDiv(e.Offset, z.g.clusterSize), e.End()/z.g.clusterSize
		if e.End() == z.g.virtualSize {
			z.hi = z.g.guestClusters // the tail past the virtual end is zero anyway
		}
		z.ok = z.lo < z.hi
	}
}

// finish drains the zero source so its fingerprint covers the whole walk.
func (z *zeroCursor) finish() (Fingerprint, error) {
	for {
		if _, ok := z.next(); !ok {
			break
		}
	}
	return z.fp, z.err
}

func (z *zeroCursor) stop() { z.stopFn() }

// aligned reports whether e covers whole clusters.
func (g geometry) aligned(e Extent) bool {
	cs := g.clusterSize
	return e.Offset%cs == 0 && (e.End()%cs == 0 || e.End() == g.virtualSize)
}

// runBytes returns the source bytes a data run covers.
func (g geometry) runBytes(r clusterRun) uint64 {
	return min((r.first+r.count)*g.clusterSize, g.virtualSize) - r.first*g.clusterSize
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
