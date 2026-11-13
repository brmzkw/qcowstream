package qcowstream_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"testing"

	"github.com/brmzkw/qcowstream"
	"github.com/brmzkw/qcowstream/internal/qcow2test"
)

// mutate returns a copy of parent where every extent is overwritten: with
// zeros when zero is true, otherwise with fresh random bytes.
func mutate(parent []byte, ext qcowstream.SliceExtents, zero bool, seed uint64) []byte {
	child := bytes.Clone(parent)
	for i, e := range ext {
		if zero {
			clear(child[e.Offset:e.End()])
			continue
		}
		copy(child[e.Offset:e.End()], randomDisk(e.Length, seed+uint64(i)))
	}
	return child
}

// recordingReader checks every read falls inside the allowed extents.
type recordingReader struct {
	disk    []byte
	allowed qcowstream.SliceExtents
	mu      sync.Mutex
	bad     []string
}

func (r *recordingReader) ReadAtContext(_ context.Context, p []byte, off int64) (int, error) {
	lo, hi := uint64(off), uint64(off)+uint64(len(p))
	ok := false
	for _, e := range r.allowed {
		if lo >= e.Offset && hi <= e.End() {
			ok = true
			break
		}
	}
	if !ok {
		r.mu.Lock()
		r.bad = append(r.bad, fmt.Sprintf("[%d, %d)", lo, hi))
		r.mu.Unlock()
		return 0, errors.New("forbidden read")
	}
	return copy(p, r.disk[off:]), nil
}

func (r *recordingReader) Size() uint64 { return uint64(len(r.disk)) }

func writeIncremental(t *testing.T, disk []byte, ext qcowstream.ExtentSource, allowed qcowstream.SliceExtents, cs uint64) ([]byte, qcowstream.Stats) {
	t.Helper()
	p, err := planInc(uint64(len(disk)), cs, ext)
	if err != nil {
		t.Fatal(err)
	}
	src := &recordingReader{disk: disk, allowed: allowed}
	var out bytes.Buffer
	st, err := qcowstream.Write(context.Background(), writerOnly{&out}, src, qcowstream.Extents{Changed: ext}, p)
	if len(src.bad) > 0 {
		t.Fatalf("reads outside the changed regions: %v", src.bad)
	}
	if err != nil {
		t.Fatal(err)
	}
	if uint64(out.Len()) != p.Size() {
		t.Fatalf("wrote %d bytes, planned %d", out.Len(), p.Size())
	}
	return out.Bytes(), st
}

func TestWriteIncremental(t *testing.T) {
	cases := []struct {
		name string
		size uint64
		cs   uint64
		ext  qcowstream.SliceExtents
		zero bool
	}{
		{"empty", 1 * MiB, 64 * KiB, nil, false},
		{"one cluster", 1 * MiB, 64 * KiB, qcowstream.SliceExtents{{128 * KiB, 64 * KiB}}, false},
		{"disjoint regions", 4 * MiB, 64 * KiB, qcowstream.SliceExtents{{0, 64 * KiB}, {1 * MiB, 192 * KiB}, {4*MiB - 64*KiB, 64 * KiB}}, false},
		{"across L2 tables", 1 * MiB, 512, qcowstream.SliceExtents{{32*KiB - 1024, 2 * KiB}, {96 * KiB, 40 * KiB}}, false},
		{"partial last cluster", 1*MiB + 512, 64 * KiB, qcowstream.SliceExtents{{1 * MiB, 512}}, false},
		{"zeroed regions", 2 * MiB, 64 * KiB, qcowstream.SliceExtents{{64 * KiB, 128 * KiB}, {1 * MiB, 64 * KiB}}, true},
		{"2 MiB clusters", 16 * MiB, 2 * MiB, qcowstream.SliceExtents{{4 * MiB, 4 * MiB}}, false},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			parent := randomDisk(c.size, uint64(100+i))
			child := mutate(parent, c.ext, c.zero, uint64(200+i))
			img, st := writeIncremental(t, child, c.ext, c.ext, c.cs)

			im, err := qcow2test.Check(img)
			if err != nil {
				t.Fatal(err)
			}
			if im.BackingFile != "base.qcow2" || im.BackingFormat != "qcow2" {
				t.Fatalf("backing %q format %q", im.BackingFile, im.BackingFormat)
			}
			if !bytes.Equal(im.Materialize(parent), child) {
				t.Fatal("parent + increment differs from the new disk")
			}
			// Exactly the covered clusters are stored, the rest inherits.
			covered := map[uint64]bool{}
			for _, e := range c.ext {
				for g := e.Offset / c.cs; g*c.cs < e.End(); g++ {
					covered[g] = true
				}
			}
			for g := range im.GuestClusters() {
				want := qcow2test.Absent
				if covered[g] {
					want = qcow2test.Data
				}
				if im.Kind(g) != want {
					t.Fatalf("cluster %d is %v, want %v", g, im.Kind(g), want)
				}
			}
			if st.DataClusters != uint64(len(covered)) || st.Walks != 2 {
				t.Fatalf("stats %+v", st)
			}
		})
	}
}

// replays yields walks[i] on walk i, then the last one forever.
type replays struct {
	walks []qcowstream.SliceExtents
	n     int
}

func (r *replays) Walk(ctx context.Context, yield func(qcowstream.Extent) error) error {
	w := r.walks[min(r.n, len(r.walks)-1)]
	r.n++
	return w.Walk(ctx, yield)
}

func TestWriteIncrementalDetectsChangedSource(t *testing.T) {
	a := qcowstream.SliceExtents{{0, 64 * KiB}, {1 * MiB, 64 * KiB}}
	cases := map[string][]qcowstream.SliceExtents{
		// Plan walks twice, then Write walks for L2 tables and for data.
		"L2 walk adds a table":    {a, a, {{0, 64 * KiB}, {1 * MiB, 64 * KiB}, {3 * MiB, 64 * KiB}}},
		"L2 walk drops a cluster": {a, a, {{0, 64 * KiB}}},
		"L2 walk moves a cluster": {a, a, {{0, 64 * KiB}, {1*MiB + 64*KiB, 64 * KiB}}},
		"data walk grows":         {a, a, a, {{0, 128 * KiB}, {1 * MiB, 64 * KiB}}},
		"data walk shrinks":       {a, a, a, {{0, 64 * KiB}}},
		"data walk misaligned":    {a, a, a, {{0, 32 * KiB}, {1 * MiB, 64 * KiB}}},
	}
	disk := randomDisk(4*MiB, 5)
	for name, walks := range cases {
		t.Run(name, func(t *testing.T) {
			src := &replays{walks: walks}
			p, err := planInc(uint64(len(disk)), 64*KiB, src)
			if err != nil {
				t.Fatal(err)
			}
			_, err = qcowstream.Write(context.Background(), io.Discard, qcowstream.NewReaderAtSource(bytes.NewReader(disk), uint64(len(disk))),
				qcowstream.Extents{Changed: src}, p)
			if !errors.Is(err, qcowstream.ErrUnstableSource) {
				t.Fatalf("got %v", err)
			}
		})
	}
}

func TestWriteIncrementalDeterministic(t *testing.T) {
	disk := randomDisk(4*MiB, 9)
	ext := qcowstream.SliceExtents{{64 * KiB, 64 * KiB}, {2 * MiB, 1 * MiB}}
	a, _ := writeIncremental(t, disk, ext, ext, 64*KiB)
	b, _ := writeIncremental(t, disk, ext, ext, 64*KiB)
	if !bytes.Equal(a, b) {
		t.Fatal("two runs differ")
	}
}

func TestWriteRejectsMissingRegionsForIncrementalPlan(t *testing.T) {
	p, err := planInc(1*MiB, 64*KiB, qcowstream.SliceExtents{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = qcowstream.Write(context.Background(), io.Discard, readerFunc(nil), qcowstream.Extents{}, p)
	if !errors.Is(err, qcowstream.ErrInvalidOptions) {
		t.Fatalf("got %v", err)
	}
}
