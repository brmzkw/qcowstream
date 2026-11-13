package qcowstream

import (
	"context"
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
	if p.opts.Mode == Incremental {
		return Stats{}, invalid("writing incremental images is not supported yet")
	}
	if (ext.Changed == nil) != (p.opts.Mode == Full) {
		return Stats{}, invalid("region sources do not match the %v plan", p.opts.Mode)
	}
	if s, ok := src.(Sizer); ok && s.Size() != p.geo.virtualSize {
		return Stats{}, invalid("source is %d bytes, the plan is for %d", s.Size(), p.geo.virtualSize)
	}
	e := &emitter{ctx: ctx, w: dst}
	st, err := p.write(e, src)
	st.BytesWritten = e.written
	if err != nil {
		return st, err
	}
	if e.written != p.layout.imageSize {
		return st, fmt.Errorf("qcowstream: internal error: wrote %d bytes, planned %d", e.written, p.layout.imageSize)
	}
	return st, nil
}

func (p *ImagePlan) write(e *emitter, src Reader) (Stats, error) {
	var st Stats
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
	if err := p.emitFullL2(e, buf); err != nil {
		return st, err
	}
	d := newDataWriter(e, src, p)
	err := d.run(0, p.geo.guestClusters)
	st.SourceBytesRead, st.DataClusters = d.read, d.clusters
	return st, err
}

// dataWriter reads runs of data clusters and emits them in guest order.
type dataWriter struct {
	e        *emitter
	src      Reader
	p        *ImagePlan
	buf      []byte
	read     uint64
	clusters uint64
}

func newDataWriter(e *emitter, src Reader, p *ImagePlan) *dataWriter {
	return &dataWriter{e: e, src: src, p: p, buf: make([]byte, p.opts.BufferSize)}
}

// run emits guest clusters [first, first+count). The last cluster of the disk
// is zero-padded past the virtual size.
func (d *dataWriter) run(first, count uint64) error {
	cs, vs := d.p.geo.clusterSize, d.p.geo.virtualSize
	pos, end := first*cs, min((first+count)*cs, vs)
	for pos < end {
		n := min(uint64(len(d.buf)), end-pos)
		chunk := d.buf[:ceilDiv(n, cs)*cs]
		if err := readFull(d.e.ctx, d.src, chunk[:n], pos); err != nil {
			return err
		}
		clear(chunk[n:])
		if err := d.e.write(chunk); err != nil {
			return err
		}
		d.read += n
		d.clusters += uint64(len(chunk)) / cs
		pos += n
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
