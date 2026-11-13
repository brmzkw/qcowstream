package qcowstream_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math/rand/v2"
	"testing"

	"github.com/brmzkw/qcowstream"
	"github.com/brmzkw/qcowstream/internal/qcow2test"
)

// randomDisk returns deterministic pseudo-random content.
func randomDisk(size uint64, seed uint64) []byte {
	r := rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))
	b := make([]byte, size)
	for i := 0; i+8 <= len(b); i += 8 {
		v := r.Uint64()
		for j := range 8 {
			b[i+j] = byte(v >> (8 * j))
		}
	}
	return b
}

// writerOnly hides any method besides Write, so the writer cannot seek.
type writerOnly struct{ w io.Writer }

func (w writerOnly) Write(p []byte) (int, error) { return w.w.Write(p) }

func generate(t *testing.T, disk []byte, ext qcowstream.Extents, opts qcowstream.Options) ([]byte, *qcowstream.ImagePlan, qcowstream.Stats) {
	t.Helper()
	opts.VirtualSize = uint64(len(disk))
	p, err := qcowstream.Plan(context.Background(), ext, opts)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	st, err := qcowstream.Write(context.Background(), writerOnly{&out}, qcowstream.NewReaderAtSource(bytes.NewReader(disk), uint64(len(disk))), ext, p)
	if err != nil {
		t.Fatal(err)
	}
	if uint64(out.Len()) != p.Size() || st.BytesWritten != p.Size() {
		t.Fatalf("wrote %d bytes (stats %d), plan says %d", out.Len(), st.BytesWritten, p.Size())
	}
	return out.Bytes(), p, st
}

func TestWriteFull(t *testing.T) {
	cases := []struct {
		name string
		size uint64
		cs   uint64
	}{
		{"one sector", 512, 512},
		{"partial last cluster", 64*KiB + 512, 64 * KiB},
		{"many L2 tables", 1 * MiB, 512},       // 64 entries per table: 32 tables
		{"many refcount blocks", 2 * MiB, 512}, // 256 entries per block
		{"odd size, 4 KiB clusters", 3*MiB + 3584, 4 * KiB},
		{"2 MiB clusters", 5*MiB + 512, 2 * MiB},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			disk := randomDisk(c.size, uint64(i))
			img, p, st := generate(t, disk, qcowstream.Extents{}, qcowstream.Options{Mode: qcowstream.Full, ClusterSize: c.cs})
			im, err := qcow2test.Check(img)
			if err != nil {
				t.Fatal(err)
			}
			if im.BackingFile != "" || im.ClusterSize != c.cs || im.VirtualSize != c.size {
				t.Fatalf("parsed %+v", im)
			}
			if !bytes.Equal(im.Materialize(nil), disk) {
				t.Fatal("image content differs from the source")
			}
			if tail := im.DataTail(); len(tail) > 0 && !bytes.Equal(tail, make([]byte, len(tail))) {
				t.Fatal("padding past the virtual end is not zero")
			}
			info := p.Info()
			if st.SourceBytesRead != c.size || st.DataClusters != info.DataClusters || uint64(im.L2Tables) != info.L2Tables {
				t.Fatalf("stats %+v, plan %+v, %d L2 tables", st, info, im.L2Tables)
			}
		})
	}
}

func TestWriteFullDeterministic(t *testing.T) {
	disk := randomDisk(3*MiB+512, 7)
	ref, _, _ := generate(t, disk, qcowstream.Extents{}, qcowstream.Options{Mode: qcowstream.Full, ClusterSize: 64 * KiB})
	for _, bs := range []uint64{64 * KiB, 192 * KiB, 8 * MiB} {
		img, _, _ := generate(t, disk, qcowstream.Extents{}, qcowstream.Options{Mode: qcowstream.Full, ClusterSize: 64 * KiB, BufferSize: bs})
		if !bytes.Equal(img, ref) {
			t.Fatalf("buffer size %d changed the output", bs)
		}
	}
}

func TestWriteFullZeroDisk(t *testing.T) {
	disk := make([]byte, 1*MiB)
	img, _, _ := generate(t, disk, qcowstream.Extents{}, qcowstream.Options{Mode: qcowstream.Full, ClusterSize: 64 * KiB})
	im, err := qcow2test.Check(img)
	if err != nil {
		t.Fatal(err)
	}
	for g := range im.GuestClusters() {
		if im.Kind(g) != qcow2test.Data {
			t.Fatalf("cluster %d is not stored", g)
		}
	}
}

// shortReader returns at most 1000 bytes per call without error, and every
// other call returns nothing at all.
type shortReader struct {
	r     io.ReaderAt
	calls int
}

func (s *shortReader) ReadAtContext(_ context.Context, p []byte, off int64) (int, error) {
	if s.calls++; s.calls%2 == 0 {
		return 0, nil
	}
	return s.r.ReadAt(p[:min(len(p), 1000)], off)
}

func planOf(t *testing.T, size, cs uint64) *qcowstream.ImagePlan {
	t.Helper()
	p, err := qcowstream.Plan(context.Background(), qcowstream.Extents{}, qcowstream.Options{
		Mode: qcowstream.Full, VirtualSize: size, ClusterSize: cs})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestWriteShortReadsAreRetried(t *testing.T) {
	disk := randomDisk(256*KiB, 3)
	p := planOf(t, uint64(len(disk)), 4*KiB)
	var out bytes.Buffer
	if _, err := qcowstream.Write(context.Background(), &out, &shortReader{r: bytes.NewReader(disk)}, qcowstream.Extents{}, p); err != nil {
		t.Fatal(err)
	}
	im, err := qcow2test.Check(out.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(im.Materialize(nil), disk) {
		t.Fatal("content differs")
	}
}

type readerFunc func(ctx context.Context, p []byte, off int64) (int, error)

func (f readerFunc) ReadAtContext(ctx context.Context, p []byte, off int64) (int, error) {
	return f(ctx, p, off)
}

func TestWriteIncompleteRead(t *testing.T) {
	disk := randomDisk(256*KiB, 3)
	boom := errors.New("disk on fire")
	cases := map[string]qcowstream.Reader{
		"source too small": qcowstream.NewReaderAtSource(bytes.NewReader(disk[:100*KiB]), 100*KiB),
		"never progresses": readerFunc(func(context.Context, []byte, int64) (int, error) { return 0, nil }),
		"fails":            readerFunc(func(context.Context, []byte, int64) (int, error) { return 0, boom }),
		"lies about n":     readerFunc(func(_ context.Context, p []byte, _ int64) (int, error) { return len(p) + 1, nil }),
	}
	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			p := planOf(t, uint64(len(disk)), 4*KiB)
			if name == "source too small" {
				// Hide Size so the mismatch is only found while reading.
				src = readerFunc(src.ReadAtContext)
			}
			_, err := qcowstream.Write(context.Background(), io.Discard, src, qcowstream.Extents{}, p)
			if !errors.Is(err, qcowstream.ErrIncompleteRead) {
				t.Fatalf("got %v", err)
			}
			if name == "fails" && !errors.Is(err, boom) {
				t.Fatalf("cause lost: %v", err)
			}
		})
	}
}

func TestWriteRejectsSizeMismatch(t *testing.T) {
	disk := randomDisk(256*KiB, 3)
	p := planOf(t, 512*KiB, 4*KiB)
	_, err := qcowstream.Write(context.Background(), io.Discard, qcowstream.NewReaderAtSource(bytes.NewReader(disk), uint64(len(disk))), qcowstream.Extents{}, p)
	if !errors.Is(err, qcowstream.ErrInvalidOptions) {
		t.Fatalf("got %v", err)
	}
}

func TestWriteRejectsRegionsForFullPlan(t *testing.T) {
	p := planOf(t, 512*KiB, 4*KiB)
	_, err := qcowstream.Write(context.Background(), io.Discard, readerFunc(nil),
		qcowstream.Extents{Changed: qcowstream.SliceExtents{}}, p)
	if !errors.Is(err, qcowstream.ErrInvalidOptions) {
		t.Fatalf("got %v", err)
	}
}

// limitedWriter accepts n bytes, then misbehaves.
type limitedWriter struct {
	n   int
	err error // returned once the limit is hit; nil means a silent short write
}

func (w *limitedWriter) Write(p []byte) (int, error) {
	if len(p) <= w.n {
		w.n -= len(p)
		return len(p), nil
	}
	n := w.n
	w.n = 0
	return n, w.err
}

func TestWriteIncompleteWrite(t *testing.T) {
	disk := randomDisk(256*KiB, 3)
	src := qcowstream.NewReaderAtSource(bytes.NewReader(disk), uint64(len(disk)))
	boom := errors.New("pipe closed")
	for _, limit := range []int{0, 100, 4096, 200 * 1024} {
		for _, werr := range []error{nil, boom} {
			p := planOf(t, uint64(len(disk)), 4*KiB)
			st, err := qcowstream.Write(context.Background(), &limitedWriter{n: limit, err: werr}, src, qcowstream.Extents{}, p)
			if !errors.Is(err, qcowstream.ErrIncompleteWrite) {
				t.Fatalf("limit %d: got %v", limit, err)
			}
			if werr == nil && !errors.Is(err, io.ErrShortWrite) {
				t.Fatalf("limit %d: silent short write not reported as such: %v", limit, err)
			}
			if werr != nil && !errors.Is(err, boom) {
				t.Fatalf("limit %d: cause lost: %v", limit, err)
			}
			if st.BytesWritten != uint64(limit) {
				t.Fatalf("limit %d: stats report %d bytes", limit, st.BytesWritten)
			}
		}
	}
}

// cancelWriter cancels the context after n bytes.
type cancelWriter struct {
	n      int
	cancel context.CancelFunc
}

func (w *cancelWriter) Write(p []byte) (int, error) {
	w.n -= len(p)
	if w.n <= 0 {
		w.cancel()
	}
	return len(p), nil
}

func TestWriteCanceledMidStream(t *testing.T) {
	disk := randomDisk(1*MiB, 3)
	p, err := qcowstream.Plan(context.Background(), qcowstream.Extents{}, qcowstream.Options{
		Mode: qcowstream.Full, VirtualSize: uint64(len(disk)), ClusterSize: 4 * KiB, BufferSize: 64 * KiB})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, err = qcowstream.Write(ctx, &cancelWriter{n: 300 * 1024, cancel: cancel},
		qcowstream.NewReaderAtSource(bytes.NewReader(disk), uint64(len(disk))), qcowstream.Extents{}, p)
	if !errors.Is(err, qcowstream.ErrCanceled) || !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
}
