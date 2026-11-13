package qcowstream

import (
	"fmt"
	"math"
	"math/bits"
)

// Limits enforced by QEMU when opening an image. Checked against
// block/qcow2.h and block/qcow2.c in QEMU 8.2.0 and 10.0.0.
const (
	MinClusterSize = 1 << 9  // MIN_CLUSTER_BITS
	MaxClusterSize = 1 << 21 // MAX_CLUSTER_BITS

	maxL1Bytes       = 32 << 20 // QCOW_MAX_L1_SIZE
	maxReftableBytes = 8 << 20  // QCOW_MAX_REFTABLE_SIZE
	maxHostOffset    = 1 << 56  // L2E_OFFSET_MASK covers bits 9-55
	maxBackingName   = 1023

	headerLength   = 104 // v3 header without the compression type field
	refcountOrder  = 4   // 16-bit refcounts
	refcountBytes  = 1 << refcountOrder / 8
	entryBytes     = 8 // L1, L2 and refcount table entries
	extHeaderBytes = 8
)

// geometry is the part of the layout that depends only on the virtual size
// and the cluster size.
type geometry struct {
	virtualSize   uint64
	clusterBits   uint
	clusterSize   uint64
	guestClusters uint64 // ceil(virtualSize / clusterSize)
	l2Entries     uint64 // entries per L2 table
	l1Entries     uint64 // L1 entries covering the virtual size
	rbEntries     uint64 // refcount entries per refcount block
}

func newGeometry(virtualSize, clusterSize uint64) (geometry, error) {
	g := geometry{
		virtualSize: virtualSize,
		clusterSize: clusterSize,
		clusterBits: uint(bits.TrailingZeros64(clusterSize)),
		l2Entries:   clusterSize / entryBytes,
		rbEntries:   clusterSize / refcountBytes,
	}
	g.guestClusters = ceilDiv(virtualSize, clusterSize)
	g.l1Entries = ceilDiv(g.guestClusters, g.l2Entries)
	if g.l1Entries > maxL1Bytes/entryBytes {
		return geometry{}, &GranularityError{
			ClusterSize:    clusterSize,
			MinClusterSize: minClusterSizeFor(virtualSize),
			Reason: fmt.Sprintf("a %d-byte disk needs %d L1 entries at this cluster size, QEMU allows %d",
				virtualSize, g.l1Entries, maxL1Bytes/entryBytes),
		}
	}
	return g, nil
}

// minClusterSizeFor returns the smallest cluster size whose L1 table can map
// virtualSize bytes, or 0 if none can.
func minClusterSizeFor(virtualSize uint64) uint64 {
	for cs := uint64(MinClusterSize); cs <= MaxClusterSize; cs <<= 1 {
		if ceilDiv(ceilDiv(virtualSize, cs), cs/entryBytes) <= maxL1Bytes/entryBytes {
			return cs
		}
	}
	return 0
}

// layout is the physical placement of every structure, in clusters. All
// structures are contiguous, in this order, starting with the header at 0.
type layout struct {
	rtClusters   uint64 // refcount table
	rbClusters   uint64 // refcount blocks
	l1Clusters   uint64
	l2Clusters   uint64
	dataClusters uint64
	total        uint64

	rtOffset   uint64
	rbOffset   uint64
	l1Offset   uint64
	l2Offset   uint64
	dataOffset uint64
	imageSize  uint64
}

// newLayout places the structures for an image with the given number of L2
// tables and data clusters. The refcount structures must count themselves, so
// their size is a fixed point, reached by iterating from zero: the sequence
// only grows and is bounded by the other structures' size.
func newLayout(g geometry, l2Tables, dataClusters uint64) (layout, error) {
	l := layout{
		l1Clusters:   ceilDiv(g.l1Entries*entryBytes, g.clusterSize),
		l2Clusters:   l2Tables,
		dataClusters: dataClusters,
	}
	fixed, err := sum(1, l.l1Clusters, l.l2Clusters, l.dataClusters)
	if err != nil {
		return layout{}, err
	}
	var rt, rb uint64
	for i := 0; ; i++ {
		if i == 64 {
			return layout{}, fmt.Errorf("%w: refcount layout does not converge", ErrOverflow)
		}
		total, err := sum(fixed, rt, rb)
		if err != nil {
			return layout{}, err
		}
		nrb := ceilDiv(total, g.rbEntries)
		rtBytes, err := mul64(nrb, entryBytes)
		if err != nil {
			return layout{}, err
		}
		nrt := ceilDiv(rtBytes, g.clusterSize)
		if nrb == rb && nrt == rt {
			l.total = total
			break
		}
		rb, rt = nrb, nrt
	}
	l.rtClusters, l.rbClusters = rt, rb

	if rt*g.clusterSize > maxReftableBytes {
		return layout{}, &GranularityError{
			ClusterSize: g.clusterSize,
			Reason: fmt.Sprintf("the refcount table needs %d bytes, QEMU allows %d; use a larger cluster size",
				rt*g.clusterSize, maxReftableBytes),
		}
	}
	size, err := mul64(l.total, g.clusterSize)
	if err != nil || size > maxHostOffset || size > math.MaxInt64 {
		return layout{}, &GranularityError{
			ClusterSize: g.clusterSize,
			Reason:      fmt.Sprintf("the image would be %d clusters, past the QCOW2 host offset limit of 2^56 bytes", l.total),
		}
	}
	l.imageSize = size
	cs := g.clusterSize
	l.rtOffset = 1 * cs
	l.rbOffset = l.rtOffset + rt*cs
	l.l1Offset = l.rbOffset + rb*cs
	l.l2Offset = l.l1Offset + l.l1Clusters*cs
	l.dataOffset = l.l2Offset + l.l2Clusters*cs
	return l, nil
}
