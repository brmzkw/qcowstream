package qcowstream

import (
	"context"
	"sync"
)

// dataWriter reads data clusters with up to N reads in flight and emits them
// in guest order.
//
// Job i uses slot i mod N. Before issuing job i, job i-N is emitted, which
// frees its slot: at most N buffers exist, and a slow destination stops new
// reads. N workers serve the jobs; nothing is spawned per region.
type dataWriter struct {
	e   *emitter
	src Reader
	cs  uint64
	vs  uint64

	ctx    context.Context
	cancel context.CancelFunc
	slots  []job
	jobs   chan *job
	wg     sync.WaitGroup
	issued uint64 // jobs issued
	head   uint64 // jobs emitted

	clusters uint64 // data clusters issued
	emitted  uint64 // data clusters emitted
	read     uint64 // source bytes emitted
}

type job struct {
	buf  []byte
	off  uint64 // source offset
	n    uint64 // bytes to read
	size uint64 // bytes to emit: n rounded up to a cluster
	done chan error
}

func newDataWriter(e *emitter, src Reader, p *ImagePlan) *dataWriter {
	n := p.opts.Concurrency
	ctx, cancel := context.WithCancel(e.ctx)
	d := &dataWriter{
		e: e, src: src, cs: p.geo.clusterSize, vs: p.geo.virtualSize,
		ctx: ctx, cancel: cancel,
		slots: make([]job, n),
		jobs:  make(chan *job, n),
	}
	for i := range d.slots {
		d.slots[i] = job{buf: make([]byte, p.opts.BufferSize), done: make(chan error, 1)}
	}
	d.wg.Add(n)
	for range n {
		go d.worker()
	}
	return d
}

func (d *dataWriter) worker() {
	defer d.wg.Done()
	for j := range d.jobs {
		j.done <- readFull(d.ctx, d.src, j.buf[:j.n], j.off)
	}
}

// run issues guest clusters [first, first+count) in chunks of the buffer
// size. The last cluster of the disk is zero-padded past the virtual size.
func (d *dataWriter) run(first, count uint64) error {
	pos, end := first*d.cs, min((first+count)*d.cs, d.vs)
	for pos < end {
		if d.issued-d.head == uint64(len(d.slots)) {
			if err := d.emitHead(); err != nil {
				return err
			}
		}
		j := &d.slots[d.issued%uint64(len(d.slots))]
		j.off, j.n = pos, min(uint64(len(j.buf)), end-pos)
		j.size = ceilDiv(j.n, d.cs) * d.cs
		d.issued++
		d.clusters += j.size / d.cs
		d.jobs <- j
		pos += j.n
	}
	return nil
}

func (d *dataWriter) emitHead() error {
	j := &d.slots[d.head%uint64(len(d.slots))]
	if err := <-j.done; err != nil {
		return err
	}
	clear(j.buf[j.n:j.size])
	if err := d.e.write(j.buf[:j.size]); err != nil {
		return err
	}
	d.head++
	d.read += j.n
	d.emitted += j.size / d.cs
	return nil
}

// flush emits every job still in flight.
func (d *dataWriter) flush() error {
	for d.head < d.issued {
		if err := d.emitHead(); err != nil {
			return err
		}
	}
	return nil
}

// close stops the workers. Reads in flight are canceled.
func (d *dataWriter) close() {
	d.cancel()
	close(d.jobs)
	d.wg.Wait()
}
