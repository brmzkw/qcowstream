package qcowstream

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

const (
	flagCopied = uint64(1) << 63 // QCOW_OFLAG_COPIED: refcount is exactly 1
	flagZero   = uint64(1)       // QCOW_OFLAG_ZERO: reads as zeros, never from the backing file

	extBackingFormat = 0xe2792aca
	extEnd           = 0
)

// emitter writes the stream strictly in order and counts its bytes.
type emitter struct {
	ctx     context.Context
	w       io.Writer
	written uint64
}

func (e *emitter) write(p []byte) error {
	if err := e.ctx.Err(); err != nil {
		return canceled(e.ctx)
	}
	n, err := e.w.Write(p)
	e.written += uint64(max(n, 0))
	if err == nil && n < len(p) {
		err = io.ErrShortWrite
	}
	if err = ctxErr(e.ctx, err); errors.Is(err, ErrCanceled) {
		return err
	}
	if err != nil {
		return fmt.Errorf("%w after %d bytes: %w", ErrIncompleteWrite, e.written, err)
	}
	return nil
}

// header builds cluster 0: the header, its extensions and the backing file
// name, then zero padding.
func (p *ImagePlan) header(buf []byte) []byte {
	clear(buf)
	be := binary.BigEndian
	copy(buf[0:4], "QFI\xfb")
	be.PutUint32(buf[4:], 3)
	be.PutUint32(buf[20:], uint32(p.geo.clusterBits))
	be.PutUint64(buf[24:], p.geo.virtualSize)
	// 32: crypt_method 0
	be.PutUint32(buf[36:], uint32(p.geo.l1Entries))
	be.PutUint64(buf[40:], p.layout.l1Offset)
	be.PutUint64(buf[48:], p.layout.rtOffset)
	be.PutUint32(buf[56:], uint32(p.layout.rtClusters))
	// 60: no snapshots; 72-95: no feature bits
	be.PutUint32(buf[96:], refcountOrder)
	be.PutUint32(buf[100:], headerLength)

	off := headerLength
	if p.opts.BackingFormat != "" {
		be.PutUint32(buf[off:], extBackingFormat)
		be.PutUint32(buf[off+4:], uint32(len(p.opts.BackingFormat)))
		copy(buf[off+extHeaderBytes:], p.opts.BackingFormat)
		off += extHeaderBytes + align8(len(p.opts.BackingFormat))
	}
	be.PutUint32(buf[off:], extEnd) // end of extensions: type 0, length 0
	off += extHeaderBytes
	if name := p.opts.BackingFile; name != "" {
		be.PutUint64(buf[8:], uint64(off))
		be.PutUint32(buf[16:], uint32(len(name)))
		copy(buf[off:], name)
	}
	return buf
}

func align8(n int) int { return (n + 7) &^ 7 }

// headerBytes is the space cluster 0 needs.
func headerBytes(backingFile, backingFormat string) uint64 {
	n := headerLength + extHeaderBytes + len(backingFile)
	if backingFormat != "" {
		n += extHeaderBytes + align8(len(backingFormat))
	}
	return uint64(n)
}

// emitRefcounts writes the refcount table and blocks. Every cluster of the
// image is in use exactly once, so entries 0 to total-1 are 1.
func (p *ImagePlan) emitRefcounts(e *emitter, buf []byte) error {
	cs, l := p.geo.clusterSize, p.layout
	perCluster := cs / entryBytes
	for c := range l.rtClusters {
		clear(buf)
		for i := range perCluster {
			rb := c*perCluster + i
			if rb >= l.rbClusters {
				break
			}
			binary.BigEndian.PutUint64(buf[i*entryBytes:], l.rbOffset+rb*cs)
		}
		if err := e.write(buf); err != nil {
			return err
		}
	}
	for b := range l.rbClusters {
		clear(buf)
		for i := range p.geo.rbEntries {
			if b*p.geo.rbEntries+i >= l.total {
				break
			}
			binary.BigEndian.PutUint16(buf[i*refcountBytes:], 1)
		}
		if err := e.write(buf); err != nil {
			return err
		}
	}
	return nil
}

// emitL1 writes the L1 table. Touched entries point to consecutive L2
// tables in L1 order.
func (p *ImagePlan) emitL1(e *emitter, buf []byte) error {
	cs, l := p.geo.clusterSize, p.layout
	perCluster := cs / entryBytes
	var rank uint64
	for c := range l.l1Clusters {
		clear(buf)
		for i := range perCluster {
			j := c*perCluster + i
			if j >= p.geo.l1Entries {
				break
			}
			if p.touched(j) {
				binary.BigEndian.PutUint64(buf[i*entryBytes:], (l.l2Offset+rank*cs)|flagCopied)
				rank++
			}
		}
		if err := e.write(buf); err != nil {
			return err
		}
	}
	return nil
}
