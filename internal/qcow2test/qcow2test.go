// Package qcow2test parses QCOW2 images held in memory and checks the
// invariants qcowstream promises. It is an independent reader written from
// the QCOW2 specification, used by tests only.
package qcow2test

import (
	"bytes"
	"encoding/binary"
	"fmt"
)

const (
	offsetMask = 0x00fffffffffffe00
	copied     = uint64(1) << 63
	compressed = uint64(1) << 62
	zeroFlag   = uint64(1)
)

// Kind classifies a guest cluster.
type Kind int

const (
	Absent Kind = iota // unallocated: backing file, or zeros without one
	Data               // stored in this image
	Zero               // "reads as zeros" flag
)

// Image is a parsed and checked image.
type Image struct {
	raw           []byte
	VirtualSize   uint64
	ClusterSize   uint64
	BackingFile   string
	BackingFormat string
	L1Size        uint64
	L2Tables      int
	clusters      []clusterRef // per guest cluster
}

type clusterRef struct {
	kind Kind
	host uint64
}

var be = binary.BigEndian

// Check parses img and verifies the header, the tables and that refcounts
// match references exactly: every used cluster has refcount 1, every other
// cluster 0, and COPIED flags are set on every allocated entry.
func Check(img []byte) (*Image, error) {
	if len(img) < 104 || !bytes.Equal(img[:4], []byte("QFI\xfb")) {
		return nil, fmt.Errorf("bad magic")
	}
	if v := be.Uint32(img[4:]); v != 3 {
		return nil, fmt.Errorf("version %d", v)
	}
	bitsN := be.Uint32(img[20:])
	if bitsN < 9 || bitsN > 21 {
		return nil, fmt.Errorf("cluster bits %d", bitsN)
	}
	cs := uint64(1) << bitsN
	if uint64(len(img))%cs != 0 {
		return nil, fmt.Errorf("file length %d is not a multiple of the cluster size %d", len(img), cs)
	}
	nclusters := uint64(len(img)) / cs
	im := &Image{raw: img, ClusterSize: cs, VirtualSize: be.Uint64(img[24:]), L1Size: uint64(be.Uint32(img[36:]))}
	if be.Uint32(img[32:]) != 0 {
		return nil, fmt.Errorf("encrypted")
	}
	if be.Uint32(img[60:]) != 0 || be.Uint64(img[64:]) != 0 {
		return nil, fmt.Errorf("snapshots present")
	}
	for _, f := range []int{72, 80, 88} {
		if be.Uint64(img[f:]) != 0 {
			return nil, fmt.Errorf("feature bits set at header offset %d", f)
		}
	}
	if o := be.Uint32(img[96:]); o != 4 {
		return nil, fmt.Errorf("refcount order %d", o)
	}
	hl := uint64(be.Uint32(img[100:]))
	if hl != 104 {
		return nil, fmt.Errorf("header length %d", hl)
	}
	if im.VirtualSize == 0 || im.VirtualSize%512 != 0 {
		return nil, fmt.Errorf("virtual size %d", im.VirtualSize)
	}

	backingOff, backingLen := be.Uint64(img[8:]), uint64(be.Uint32(img[16:]))
	extEnd := cs
	if backingOff != 0 {
		extEnd = backingOff
		if backingLen == 0 || backingLen > 1023 || backingOff+backingLen > cs {
			return nil, fmt.Errorf("backing file at %d, %d bytes", backingOff, backingLen)
		}
		im.BackingFile = string(img[backingOff : backingOff+backingLen])
	}
	if err := im.parseExtensions(hl, extEnd); err != nil {
		return nil, err
	}
	if (im.BackingFile == "") != (im.BackingFormat == "") {
		return nil, fmt.Errorf("backing file %q with format %q", im.BackingFile, im.BackingFormat)
	}

	refs := make([]int, nclusters) // references found by walking structures
	ref := func(off uint64, what string) error {
		if off%cs != 0 || off/cs >= nclusters {
			return fmt.Errorf("%s at %d is not a cluster inside the file", what, off)
		}
		refs[off/cs]++
		return nil
	}
	if err := ref(0, "header"); err != nil {
		return nil, err
	}

	rtOff, rtClusters := be.Uint64(img[48:]), uint64(be.Uint32(img[56:]))
	if rtClusters == 0 {
		return nil, fmt.Errorf("no refcount table")
	}
	refcount := make([]uint64, nclusters)
	for c := range rtClusters {
		if err := ref(rtOff+c*cs, "refcount table"); err != nil {
			return nil, err
		}
	}
	perBlock := cs / 2
	for i := range rtClusters * cs / 8 {
		e := be.Uint64(img[rtOff+i*8:])
		if e == 0 {
			continue
		}
		if e&0x1ff != 0 {
			return nil, fmt.Errorf("refcount table entry %d has reserved bits: %#x", i, e)
		}
		if err := ref(e, "refcount block"); err != nil {
			return nil, err
		}
		for j := range perBlock {
			k := i*perBlock + j
			v := uint64(be.Uint16(img[e+j*2:]))
			if k >= nclusters {
				if v != 0 {
					return nil, fmt.Errorf("refcount %d for cluster %d past the end of file", v, k)
				}
				continue
			}
			refcount[k] = v
		}
	}

	l1Off := be.Uint64(img[40:])
	l2Bits := bitsN - 3
	guestClusters := (im.VirtualSize + cs - 1) / cs
	needL1 := (guestClusters + (1 << l2Bits) - 1) >> l2Bits
	if im.L1Size < needL1 || im.L1Size*8 > 32<<20 {
		return nil, fmt.Errorf("L1 size %d, need %d", im.L1Size, needL1)
	}
	for c := range (im.L1Size*8 + cs - 1) / cs {
		if err := ref(l1Off+c*cs, "L1 table"); err != nil {
			return nil, err
		}
	}
	im.clusters = make([]clusterRef, guestClusters)
	for j := range im.L1Size {
		e := be.Uint64(img[l1Off+j*8:])
		if e == 0 {
			continue
		}
		if e&^(offsetMask|copied) != 0 || e&copied == 0 {
			return nil, fmt.Errorf("L1 entry %d is %#x", j, e)
		}
		l2 := e & offsetMask
		if err := ref(l2, "L2 table"); err != nil {
			return nil, err
		}
		im.L2Tables++
		for i := range uint64(1) << l2Bits {
			g := j<<l2Bits + i
			e := be.Uint64(img[l2+i*8:])
			if g >= guestClusters {
				if e != 0 {
					return nil, fmt.Errorf("L2 entry for cluster %d past the disk is %#x", g, e)
				}
				continue
			}
			switch {
			case e == 0:
			case e == zeroFlag:
				im.clusters[g] = clusterRef{kind: Zero}
			case e&compressed != 0 || e&^(offsetMask|copied) != 0 || e&copied == 0:
				return nil, fmt.Errorf("L2 entry for cluster %d is %#x", g, e)
			default:
				host := e & offsetMask
				if err := ref(host, "data"); err != nil {
					return nil, err
				}
				im.clusters[g] = clusterRef{kind: Data, host: host}
			}
		}
	}
	for k := range nclusters {
		if uint64(refs[k]) != refcount[k] || refs[k] > 1 {
			return nil, fmt.Errorf("cluster %d: %d references, refcount %d", k, refs[k], refcount[k])
		}
		if refs[k] == 0 {
			return nil, fmt.Errorf("cluster %d is unused: the file has a gap or leak", k)
		}
	}
	return im, nil
}

func (im *Image) parseExtensions(start, end uint64) error {
	img := im.raw
	for off := start; ; {
		if off+8 > end {
			return fmt.Errorf("extensions run past %d without an end marker", end)
		}
		typ, n := be.Uint32(img[off:]), uint64(be.Uint32(img[off+4:]))
		if typ == 0 {
			if n != 0 {
				return fmt.Errorf("end marker with length %d", n)
			}
			return nil
		}
		data := off + 8
		if data+n > end {
			return fmt.Errorf("extension %#x overruns", typ)
		}
		switch typ {
		case 0xe2792aca:
			im.BackingFormat = string(img[data : data+n])
		default:
			return fmt.Errorf("unexpected extension %#x", typ)
		}
		off = data + (n+7)&^7
	}
}

// Kind returns the class of guest cluster g.
func (im *Image) Kind(g uint64) Kind { return im.clusters[g].kind }

// GuestClusters returns the number of guest clusters.
func (im *Image) GuestClusters() uint64 { return uint64(len(im.clusters)) }

// Materialize returns the virtual disk content. Absent clusters come from
// backing, which must hold the parent's virtual content, or are zeros when
// backing is nil.
func (im *Image) Materialize(backing []byte) []byte {
	out := make([]byte, im.VirtualSize)
	cs := im.ClusterSize
	for g, c := range im.clusters {
		lo := uint64(g) * cs
		hi := min(lo+cs, im.VirtualSize)
		switch c.kind {
		case Data:
			copy(out[lo:hi], im.raw[c.host:c.host+hi-lo])
		case Absent:
			if backing != nil {
				copy(out[lo:hi], backing[lo:hi])
			}
		}
	}
	return out
}

// DataTail returns the bytes stored past the virtual end in the last
// cluster, which must be zero.
func (im *Image) DataTail() []byte {
	n := uint64(len(im.clusters))
	if n == 0 || im.clusters[n-1].kind != Data {
		return nil
	}
	c := im.clusters[n-1]
	used := im.VirtualSize - (n-1)*im.ClusterSize
	return im.raw[c.host+used : c.host+im.ClusterSize]
}
