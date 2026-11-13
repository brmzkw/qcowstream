package nbdsource

import (
	"context"
	"errors"

	"github.com/digitalocean/go-nbd/nbdmeta"

	"github.com/brmzkw/qcowstream"
)

// zeroRegions enumerates regions base:allocation reports with the ZERO flag.
// HOLE alone does not promise zeros and is ignored. With a bitmap, only the
// dirty extents are queried: a Proxmox export refuses allocation queries
// elsewhere. It walks its own bitmap connection, so it can run in lockstep
// with Changed.
type zeroRegions struct {
	s     *Source
	alloc *statusConn
	dirty *statusConn // nil in full mode
}

func (s *Source) openZeros(ctx context.Context) error {
	z := &zeroRegions{s: s, alloc: &statusConn{s: s, context: "base:allocation"}}
	if err := z.alloc.acquire(ctx); errors.Is(err, ErrMissingContext) {
		return nil // no zero information
	} else if err != nil {
		return err
	}
	if s.opts.Bitmap != "" {
		z.dirty = &statusConn{s: s, context: "qemu:dirty-bitmap:" + s.opts.Bitmap}
	}
	s.zeros = z
	return nil
}

// Zeros returns the regions the server reports as reading zeros, or nil
// without zero information. Pass it as qcowstream.Extents.Zeros.
func (s *Source) Zeros() qcowstream.ExtentSource {
	if s.zeros == nil {
		return nil
	}
	return s.zeros
}

func (z *zeroRegions) Walk(ctx context.Context, yield func(qcowstream.Extent) error) error {
	z.alloc.mu.Lock()
	defer z.alloc.mu.Unlock()
	query := func(lo, hi uint64) error {
		return z.alloc.walk(ctx, lo, hi, func(off, n uint64, flags uint32) error {
			if !nbdmeta.BaseAllocationFlags(flags).Zero() {
				return nil
			}
			return yield(qcowstream.Extent{Offset: off, Length: n})
		})
	}
	if z.dirty == nil {
		return query(0, z.s.size)
	}
	z.dirty.mu.Lock()
	defer z.dirty.mu.Unlock()
	return z.dirty.walk(ctx, 0, z.s.size, func(off, n uint64, flags uint32) error {
		if !nbdmeta.DirtyBitmapFlags(flags).Dirty() {
			return nil
		}
		return query(off, off+n)
	})
}
