package qcowstream_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/brmzkw/qcowstream"
	"github.com/brmzkw/qcowstream/internal/qcow2test"
	"github.com/brmzkw/qcowstream/internal/testutil"
)

// jitterReader delays each read by a pseudo-random amount, so concurrent
// reads complete out of order.
type jitterReader struct{ disk []byte }

func (j jitterReader) ReadAtContext(ctx context.Context, p []byte, off int64) (int, error) {
	d := time.Duration((off*7919)%5) * 200 * time.Microsecond
	select {
	case <-time.After(d):
	case <-ctx.Done():
		return 0, ctx.Err()
	}
	return copy(p, j.disk[off:]), nil
}

func TestConcurrencyDoesNotChangeOutput(t *testing.T) {
	const cs = 4 * KiB
	disk := testutil.RandomDisk(8*MiB+512, 11)
	changed := qcowstream.SliceExtents{{0, 1 * MiB}, {2 * MiB, 3 * MiB}, {8 * MiB, 512}}
	zeros := qcowstream.SliceExtents{{512 * KiB, 64 * KiB}, {3 * MiB, 128 * KiB}}
	disk = zeroed(disk, zeros)
	for _, mode := range []qcowstream.Mode{qcowstream.Full, qcowstream.Incremental} {
		ext := qcowstream.Extents{Zeros: zeros}
		opts := qcowstream.Options{Mode: mode, ClusterSize: cs, BufferSize: 64 * KiB}
		if mode == qcowstream.Incremental {
			ext.Changed, opts.BackingFile = changed, "base.qcow2"
		}
		var ref []byte
		for _, n := range []int{1, 4, 16} {
			opts.Concurrency = n
			img, _ := generateWith(t, disk, ext, opts, jitterReader{disk})
			if ref == nil {
				ref = img
				if _, err := qcow2test.Check(img); err != nil {
					t.Fatal(err)
				}
				continue
			}
			if !bytes.Equal(img, ref) {
				t.Fatalf("%v: concurrency %d changed the output", mode, n)
			}
		}
	}
}

// slowWriter delays every write and counts the data chunks written.
type slowWriter struct {
	dataStart, chunk uint64
	written          atomic.Uint64
}

func (w *slowWriter) Write(p []byte) (int, error) {
	time.Sleep(200 * time.Microsecond)
	w.written.Add(uint64(len(p)))
	return len(p), nil
}

func (w *slowWriter) chunks() uint64 {
	n := w.written.Load()
	if n <= w.dataStart {
		return 0
	}
	return (n - w.dataStart) / w.chunk
}

// aheadReader checks reads never run more than max chunks ahead of the
// chunks written.
type aheadReader struct {
	disk   []byte
	w      *slowWriter
	max    uint64
	mu     sync.Mutex
	starts uint64
	worst  uint64
}

func (a *aheadReader) ReadAtContext(_ context.Context, p []byte, off int64) (int, error) {
	a.mu.Lock()
	a.starts++
	ahead := a.starts - a.w.chunks()
	a.worst = max(a.worst, ahead)
	a.mu.Unlock()
	return copy(p, a.disk[off:]), nil
}

func TestSlowDestinationBoundsReads(t *testing.T) {
	const n = 4
	disk := testutil.RandomDisk(16*MiB, 12)
	p, err := qcowstream.Plan(context.Background(), qcowstream.Extents{}, qcowstream.Options{
		Mode: qcowstream.Full, VirtualSize: uint64(len(disk)), ClusterSize: 64 * KiB,
		BufferSize: 64 * KiB, Concurrency: n})
	if err != nil {
		t.Fatal(err)
	}
	info := p.Info()
	w := &slowWriter{dataStart: info.ImageSize - info.DataClusters*info.ClusterSize, chunk: 64 * KiB}
	r := &aheadReader{disk: disk, w: w}
	if _, err := qcowstream.Write(context.Background(), w, r, qcowstream.Extents{}, p); err != nil {
		t.Fatal(err)
	}
	// N reads in flight plus the chunk being written.
	if r.worst > n+1 {
		t.Fatalf("reads ran %d chunks ahead of the destination, limit %d", r.worst, n+1)
	}
	if r.worst < 2 {
		t.Fatalf("reads never overlapped (worst %d): concurrency is not used", r.worst)
	}
}

func waitGoroutines(t *testing.T, base int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > base {
		if time.Now().After(deadline) {
			buf := make([]byte, 1<<16)
			t.Fatalf("%d goroutines left, started with %d:\n%s", runtime.NumGoroutine(), base, buf[:runtime.Stack(buf, true)])
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestNoGoroutineLeaks(t *testing.T) {
	disk := testutil.RandomDisk(4*MiB, 13)
	plan := func() *qcowstream.ImagePlan {
		p, err := qcowstream.Plan(context.Background(), qcowstream.Extents{}, qcowstream.Options{
			Mode: qcowstream.Full, VirtualSize: uint64(len(disk)), ClusterSize: 64 * KiB,
			BufferSize: 64 * KiB, Concurrency: 8})
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	t.Run("source error", func(t *testing.T) {
		base := runtime.NumGoroutine()
		boom := errors.New("boom")
		var calls atomic.Int64
		src := readerFunc(func(ctx context.Context, p []byte, off int64) (int, error) {
			if calls.Add(1) == 10 {
				return 0, boom
			}
			return copy(p, disk[off:]), nil
		})
		if _, err := qcowstream.Write(context.Background(), io.Discard, src, qcowstream.Extents{}, plan()); !errors.Is(err, boom) {
			t.Fatalf("got %v", err)
		}
		waitGoroutines(t, base)
	})
	t.Run("cancel", func(t *testing.T) {
		base := runtime.NumGoroutine()
		ctx, cancel := context.WithCancel(context.Background())
		w := &cancelWriter{n: 1 * int(MiB), cancel: cancel}
		if _, err := qcowstream.Write(ctx, w, jitterReader{disk}, qcowstream.Extents{}, plan()); !errors.Is(err, qcowstream.ErrCanceled) {
			t.Fatalf("got %v", err)
		}
		waitGoroutines(t, base)
	})
	t.Run("destination error", func(t *testing.T) {
		base := runtime.NumGoroutine()
		_, err := qcowstream.Write(context.Background(), &limitedWriter{n: 1 << 20}, jitterReader{disk}, qcowstream.Extents{}, plan())
		if !errors.Is(err, qcowstream.ErrIncompleteWrite) {
			t.Fatalf("got %v", err)
		}
		waitGoroutines(t, base)
	})
	t.Run("success", func(t *testing.T) {
		base := runtime.NumGoroutine()
		if _, err := qcowstream.Write(context.Background(), io.Discard, jitterReader{disk}, qcowstream.Extents{}, plan()); err != nil {
			t.Fatal(err)
		}
		waitGoroutines(t, base)
	})
}

func TestConcurrencyCountsInBudget(t *testing.T) {
	_, err := qcowstream.Plan(context.Background(), qcowstream.Extents{}, qcowstream.Options{
		Mode: qcowstream.Full, VirtualSize: GiB, BufferSize: 4 * MiB, Concurrency: 64})
	var be *qcowstream.MemoryBudgetError
	if !errors.As(err, &be) || be.Required < 256*MiB {
		t.Fatalf("got %v", err)
	}
	p, err := qcowstream.Plan(context.Background(), qcowstream.Extents{}, qcowstream.Options{
		Mode: qcowstream.Full, VirtualSize: GiB, BufferSize: 4 * MiB, Concurrency: 16})
	if err != nil {
		t.Fatal(err)
	}
	if est := p.Info().MemoryEstimate; est < 64*MiB || est > qcowstream.DefaultMemoryLimit {
		t.Fatalf("estimate %d for 16 x 4 MiB buffers", est)
	}
}

// overlapProbe records whether a read started while a write was in progress.
type overlapProbe struct {
	disk       []byte
	writing    atomic.Bool
	overlapped atomic.Bool
}

func (o *overlapProbe) ReadAtContext(_ context.Context, p []byte, off int64) (int, error) {
	if o.writing.Load() {
		o.overlapped.Store(true)
	}
	return copy(p, o.disk[off:]), nil
}

func (o *overlapProbe) Write(p []byte) (int, error) {
	o.writing.Store(true)
	time.Sleep(2 * time.Millisecond)
	o.writing.Store(false)
	return len(p), nil
}

func TestSequentialReadOverlapsWrite(t *testing.T) {
	disk := testutil.RandomDisk(2*MiB, 14)
	p, err := qcowstream.Plan(context.Background(), qcowstream.Extents{}, qcowstream.Options{
		Mode: qcowstream.Full, VirtualSize: uint64(len(disk)), ClusterSize: 64 * KiB, BufferSize: 64 * KiB})
	if err != nil {
		t.Fatal(err)
	}
	o := &overlapProbe{disk: disk}
	if _, err := qcowstream.Write(context.Background(), o, o, qcowstream.Extents{}, p); err != nil {
		t.Fatal(err)
	}
	if !o.overlapped.Load() {
		t.Fatal("with concurrency 1, no read ran while a chunk was being written")
	}
}
