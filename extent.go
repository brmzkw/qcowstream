package qcowstream

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"hash"
)

// Extent is the half-open byte interval [Offset, Offset+Length) of the
// virtual disk.
type Extent struct {
	Offset uint64
	Length uint64
}

// End returns Offset+Length. It is only meaningful for validated extents.
func (e Extent) End() uint64 { return e.Offset + e.Length }

// ExtentSource enumerates extents in increasing offset order.
//
// Each call to Walk restarts from the beginning and must yield the same
// extents. Extents must be non-empty, must not overlap and must lie within the
// virtual disk; adjacent extents are allowed and merged. Walk must stop and
// return the error when yield returns one.
type ExtentSource interface {
	Walk(ctx context.Context, yield func(Extent) error) error
}

// SliceExtents is an in-memory ExtentSource, meant for small lists and tests.
type SliceExtents []Extent

func (s SliceExtents) Walk(ctx context.Context, yield func(Extent) error) error {
	for _, e := range s {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := yield(e); err != nil {
			return err
		}
	}
	return nil
}

// Fingerprint identifies one canonical (merged) enumeration of a source.
// Two walks of a stable source give equal fingerprints.
type Fingerprint struct {
	Extents uint64   // merged extents
	Bytes   uint64   // bytes covered
	Sum     [32]byte // SHA-256 over the merged extents
}

// merger validates raw extents, merges adjacent ones and fingerprints the
// merged sequence. It keeps one pending extent, so memory is constant.
type merger struct {
	size    uint64
	emit    func(Extent) error
	prevEnd uint64
	seen    bool
	pending Extent
	have    bool
	h       hash.Hash
	fp      Fingerprint
}

func newMerger(size uint64, emit func(Extent) error) *merger {
	return &merger{size: size, emit: emit, h: sha256.New()}
}

func (m *merger) add(e Extent) error {
	if e.Length == 0 {
		return fmt.Errorf("%w: empty extent at offset %d", ErrInvalidRegion, e.Offset)
	}
	end := e.Offset + e.Length
	if end < e.Offset {
		return fmt.Errorf("%w: extent [%d, +%d) overflows: %w", ErrInvalidRegion, e.Offset, e.Length, ErrOverflow)
	}
	if end > m.size {
		return fmt.Errorf("%w: extent [%d, %d) ends past the virtual size %d", ErrInvalidRegion, e.Offset, end, m.size)
	}
	if m.seen && e.Offset < m.prevEnd {
		return fmt.Errorf("%w: extent at offset %d is out of order or overlaps the previous one ending at %d",
			ErrInvalidRegion, e.Offset, m.prevEnd)
	}
	m.seen = true
	m.prevEnd = end
	if m.have && m.pending.End() == e.Offset {
		m.pending.Length += e.Length
		return nil
	}
	if err := m.flush(); err != nil {
		return err
	}
	m.pending, m.have = e, true
	return nil
}

func (m *merger) flush() error {
	if !m.have {
		return nil
	}
	e := m.pending
	m.have = false
	var buf [16]byte
	binary.BigEndian.PutUint64(buf[:8], e.Offset)
	binary.BigEndian.PutUint64(buf[8:], e.Length)
	m.h.Write(buf[:])
	m.fp.Extents++
	m.fp.Bytes += e.Length
	if m.emit != nil {
		return m.emit(e)
	}
	return nil
}

func (m *merger) finish() (Fingerprint, error) {
	if err := m.flush(); err != nil {
		return Fingerprint{}, err
	}
	copy(m.fp.Sum[:], m.h.Sum(nil))
	return m.fp, nil
}

// MergeExtents walks src once, validates its extents against a virtual disk of
// size bytes and calls fn, if not nil, with the merged extents in order. It
// returns the fingerprint of the merged sequence. Plan and Write use it on
// every walk.
func MergeExtents(ctx context.Context, src ExtentSource, size uint64, fn func(Extent) error) (Fingerprint, error) {
	m := newMerger(size, fn)
	err := src.Walk(ctx, func(e Extent) error {
		if err := ctx.Err(); err != nil {
			return canceled(ctx)
		}
		return m.add(e)
	})
	if err != nil {
		return Fingerprint{}, ctxErr(ctx, err)
	}
	if err := ctx.Err(); err != nil {
		return Fingerprint{}, canceled(ctx)
	}
	return m.finish()
}
