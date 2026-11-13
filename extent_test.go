package qcowstream_test

import (
	"context"
	"errors"
	"math"
	"testing"

	"github.com/brmzkw/qcowstream"
)

func merge(t *testing.T, size uint64, in ...qcowstream.Extent) ([]qcowstream.Extent, qcowstream.Fingerprint, error) {
	t.Helper()
	var out []qcowstream.Extent
	fp, err := qcowstream.MergeExtents(context.Background(), qcowstream.SliceExtents(in), size, func(e qcowstream.Extent) error {
		out = append(out, e)
		return nil
	})
	return out, fp, err
}

type E = qcowstream.Extent

func TestMergeExtentsMergesAdjacent(t *testing.T) {
	out, fp, err := merge(t, 1<<20, E{0, 512}, E{512, 512}, E{4096, 512}, E{4608, 1024}, E{8192, 512})
	if err != nil {
		t.Fatal(err)
	}
	want := []E{{0, 1024}, {4096, 1536}, {8192, 512}}
	if len(out) != len(want) {
		t.Fatalf("got %v, want %v", out, want)
	}
	for i := range want {
		if out[i] != want[i] {
			t.Fatalf("got %v, want %v", out, want)
		}
	}
	if fp.Extents != 3 || fp.Bytes != 3072 {
		t.Fatalf("fingerprint counters %+v", fp)
	}
}

func TestMergeExtentsEmptyList(t *testing.T) {
	out, fp, err := merge(t, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 0 || fp.Extents != 0 || fp.Bytes != 0 {
		t.Fatalf("empty list gave %v %+v", out, fp)
	}
}

func TestMergeExtentsRejectsInvalid(t *testing.T) {
	const size = 1 << 20
	cases := map[string][]E{
		"zero length":       {{0, 0}},
		"out of order":      {{4096, 512}, {0, 512}},
		"overlap":           {{0, 1024}, {512, 1024}},
		"duplicate":         {{0, 512}, {0, 512}},
		"past end":          {{size - 512, 1024}},
		"starts past end":   {{size, 512}},
		"offset overflow":   {{math.MaxUint64 - 10, 512}},
		"length overflow":   {{512, math.MaxUint64}},
		"overlap after gap": {{0, 512}, {4096, 4096}, {6000, 10}},
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			_, _, err := merge(t, size, in...)
			if !errors.Is(err, qcowstream.ErrInvalidRegion) {
				t.Fatalf("got %v, want ErrInvalidRegion", err)
			}
		})
	}
}

func TestMergeExtentsFingerprintIsCanonical(t *testing.T) {
	_, a, err := merge(t, 1<<20, E{0, 1024}, E{4096, 512})
	if err != nil {
		t.Fatal(err)
	}
	_, b, err := merge(t, 1<<20, E{0, 512}, E{512, 512}, E{4096, 256}, E{4352, 256})
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Fatal("same merged extents gave different fingerprints")
	}
	_, c, err := merge(t, 1<<20, E{0, 1024}, E{4096, 1024})
	if err != nil {
		t.Fatal(err)
	}
	if a == c {
		t.Fatal("different extents gave the same fingerprint")
	}
}

func TestMergeExtentsCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := qcowstream.MergeExtents(ctx, qcowstream.SliceExtents{{0, 512}}, 1<<20, nil)
	if !errors.Is(err, qcowstream.ErrCanceled) || !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
}

func TestMergeExtentsStopsOnCallbackError(t *testing.T) {
	boom := errors.New("boom")
	_, err := qcowstream.MergeExtents(context.Background(), qcowstream.SliceExtents{{0, 512}, {4096, 512}}, 1<<20,
		func(qcowstream.Extent) error { return boom })
	if !errors.Is(err, boom) {
		t.Fatalf("got %v", err)
	}
}

// FuzzMergeExtents checks that merging preserves exactly the covered bytes,
// keeps order and never leaves two adjacent extents.
func FuzzMergeExtents(f *testing.F) {
	f.Add([]byte{0, 1, 1, 2, 0, 3})
	f.Add([]byte{5, 0, 0, 0, 2, 2})
	f.Fuzz(func(t *testing.T, data []byte) {
		const size = 4096
		// Each byte pair is (gap, length) in 16-byte units.
		var in []E
		var pos uint64
		covered := make([]bool, size)
		for i := 0; i+1 < len(data); i += 2 {
			gap, length := uint64(data[i]%8)*16, uint64(data[i+1]%8)*16
			e := E{pos + gap, length}
			in = append(in, e)
			pos = e.End()
		}
		valid := true
		for _, e := range in {
			if e.Length == 0 || e.End() > size {
				valid = false
				break
			}
			for b := e.Offset; b < e.End(); b++ {
				covered[b] = true
			}
		}
		out, fp, err := merge(t, size, in...)
		if !valid {
			if !errors.Is(err, qcowstream.ErrInvalidRegion) {
				t.Fatalf("invalid input %v accepted: %v", in, err)
			}
			return
		}
		if err != nil {
			t.Fatalf("valid input %v rejected: %v", in, err)
		}
		got := make([]bool, size)
		var bytes uint64
		for i, e := range out {
			if e.Length == 0 {
				t.Fatalf("empty merged extent in %v", out)
			}
			if i > 0 && out[i-1].End() >= e.Offset {
				t.Fatalf("merged extents not strictly separated: %v", out)
			}
			for b := e.Offset; b < e.End(); b++ {
				got[b] = true
			}
			bytes += e.Length
		}
		for b := range covered {
			if covered[b] != got[b] {
				t.Fatalf("byte %d coverage differs: input %v merged %v", b, in, out)
			}
		}
		if fp.Bytes != bytes || fp.Extents != uint64(len(out)) {
			t.Fatalf("fingerprint counters %+v for %v", fp, out)
		}
	})
}
