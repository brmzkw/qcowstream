package qcowstream

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// Stats reports what Write did.
type Stats struct {
	BytesWritten    uint64 // equals the plan size on success
	SourceBytesRead uint64
	DataClusters    uint64
	Walks           int // walks over the region sources
}

// Write emits the planned image to dst in one sequential pass. src must be
// the source the plan describes and ext the same region sources.
//
// On error the stream is partial and must be discarded. Success means every
// byte was handed to dst, not that dst stored it durably.
func Write(ctx context.Context, dst io.Writer, src Reader, ext Extents, p *ImagePlan) (Stats, error) {
	if p == nil {
		return Stats{}, invalid("nil plan")
	}
	if (ext.Changed == nil) != (p.opts.Mode == Full) || (ext.Zeros != nil) != p.hasZeros {
		return Stats{}, invalid("region sources do not match the %v plan", p.opts.Mode)
	}
	if s, ok := src.(Sizer); ok && s.Size() != p.geo.virtualSize {
		return Stats{}, invalid("source is %d bytes, the plan is for %d", s.Size(), p.geo.virtualSize)
	}
	e := &emitter{ctx: ctx, w: dst}
	st, err := p.write(e, src, ext)
	st.BytesWritten = e.written
	if err != nil {
		return st, err
	}
	if e.written != p.layout.imageSize {
		return st, fmt.Errorf("qcowstream: internal error: wrote %d bytes, planned %d", e.written, p.layout.imageSize)
	}
	return st, nil
}

func (p *ImagePlan) write(e *emitter, src Reader, ext Extents) (st Stats, err error) {
	buf := make([]byte, p.geo.clusterSize)
	if err := e.write(p.header(buf)); err != nil {
		return st, err
	}
	if err := p.emitRefcounts(e, buf); err != nil {
		return st, err
	}
	if err := p.emitL1(e, buf); err != nil {
		return st, err
	}
	if p.touchedL1 == nil {
		if err := p.emitFullL2(e, buf); err != nil {
			return st, err
		}
		d := newDataWriter(e, src, p)
		defer d.close()
		defer func() { st.SourceBytesRead, st.DataClusters = d.read, d.emitted }()
		if err := d.run(0, p.geo.guestClusters); err != nil {
			return st, err
		}
		return st, d.flush()
	}

	st.Walks++
	if err := p.emitWalkedL2(e, buf, ext); err != nil {
		return st, err
	}
	st.Walks++
	d := newDataWriter(e, src, p)
	defer d.close()
	defer func() { st.SourceBytesRead, st.DataClusters = d.read, d.emitted }()
	prints, err := p.geo.walkRuns(e.ctx, ext, func(r clusterRun) error {
		if r.zero {
			return nil
		}
		if d.clusters+r.count > p.layout.dataClusters {
			return p.unstable("more data clusters than planned")
		}
		return d.run(r.first, r.count)
	})
	if err != nil {
		return st, err
	}
	if prints != p.prints || d.clusters != p.layout.dataClusters {
		return st, p.unstable("the data walk differs from the plan")
	}
	return st, d.flush()
}

func (p *ImagePlan) unstable(what string) error {
	return fmt.Errorf("%w: %s; the stream is incomplete", ErrUnstableSource, what)
}

// emitWalkedL2 walks the region sources and writes the touched L2 tables,
// one at a time. Data clusters are numbered in guest order, matching the
// order the data walk emits them.
func (p *ImagePlan) emitWalkedL2(e *emitter, buf []byte, ext Extents) error {
	g, l := p.geo, p.layout
	const none = ^uint64(0)
	cur, tables, k := none, uint64(0), uint64(0)
	flush := func() error {
		if cur == none {
			return nil
		}
		tables++
		return e.write(buf)
	}
	incremental := p.opts.Mode == Incremental
	prints, err := g.walkRuns(e.ctx, ext, func(r clusterRun) error {
		if r.zero && !incremental {
			return nil // absent in a full image
		}
		end := r.first + r.count
		if !r.zero && k+r.count > l.dataClusters {
			return p.unstable("more data clusters than planned")
		}
		for gc := r.first; gc < end; {
			if t := gc / g.l2Entries; t != cur {
				if err := flush(); err != nil {
					return err
				}
				if tables >= l.l2Clusters || !p.touched(t) {
					return p.unstable("an L2 table that was not planned")
				}
				cur = t
				clear(buf)
			}
			tableEnd := min(end, (cur+1)*g.l2Entries)
			for ; gc < tableEnd; gc++ {
				entry := flagZero
				if !r.zero {
					entry = (l.dataOffset + k*g.clusterSize) | flagCopied
					k++
				}
				binary.BigEndian.PutUint64(buf[(gc%g.l2Entries)*entryBytes:], entry)
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	if err := flush(); err != nil {
		return err
	}
	if prints != p.prints || tables != l.l2Clusters || k != l.dataClusters {
		return p.unstable("the L2 walk differs from the plan")
	}
	return nil
}

// maxNoProgress bounds consecutive empty reads without an error.
const maxNoProgress = 100

// readFull fills p from src at off. A short read without error is retried; an
// early io.EOF is an error since the source is smaller than planned.
func readFull(ctx context.Context, src Reader, p []byte, off uint64) error {
	var done, stalled int
	for done < len(p) {
		if err := ctx.Err(); err != nil {
			return canceled(ctx)
		}
		n, err := src.ReadAtContext(ctx, p[done:], int64(off)+int64(done))
		if n < 0 || n > len(p)-done {
			return fmt.Errorf("%w at offset %d: source returned %d bytes", ErrIncompleteRead, off, n)
		}
		done += n
		if done == len(p) {
			return nil
		}
		switch {
		case errors.Is(err, io.EOF):
			err = io.ErrUnexpectedEOF
		case err == nil && n == 0:
			if stalled++; stalled < maxNoProgress {
				continue
			}
			err = io.ErrNoProgress
		case err == nil:
			stalled = 0
			continue
		}
		if err = ctxErr(ctx, err); errors.Is(err, ErrCanceled) {
			return err
		}
		return fmt.Errorf("%w at offset %d: %w", ErrIncompleteRead, off+uint64(done), err)
	}
	return nil
}
