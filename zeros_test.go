package qcowstream_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/brmzkw/qcowstream"
	"github.com/brmzkw/qcowstream/internal/qcow2test"
	"github.com/brmzkw/qcowstream/internal/testutil"
)

// zeroClusters returns the clusters lying entirely inside zeros, computed
// independently from the library: every byte of the cluster within the
// virtual size must be listed.
func zeroClusters(size, cs uint64, zeros qcowstream.SliceExtents) map[uint64]bool {
	inZero := func(b uint64) bool {
		for _, z := range zeros {
			if b >= z.Offset && b < z.End() {
				return true
			}
		}
		return false
	}
	out := map[uint64]bool{}
	for g := uint64(0); g*cs < size; g++ {
		all := true
		for b := g * cs; b < min((g+1)*cs, size); b += 512 {
			if !inZero(b) || !inZero(min((g+1)*cs, size)-1) {
				all = false
				break
			}
		}
		if all {
			out[g] = true
		}
	}
	return out
}

// zeroGuard fails on any read touching a cluster known to be zero.
type zeroGuard struct {
	disk  []byte
	cs    uint64
	zero  map[uint64]bool
	reads []string
}

func (z *zeroGuard) ReadAtContext(_ context.Context, p []byte, off int64) (int, error) {
	for g := uint64(off) / z.cs; g*z.cs < uint64(off)+uint64(len(p)); g++ {
		if z.zero[g] {
			z.reads = append(z.reads, fmt.Sprintf("cluster %d", g))
			return 0, errors.New("read of a known-zero cluster")
		}
	}
	return copy(p, z.disk[off:]), nil
}

func generateWith(t *testing.T, disk []byte, ext qcowstream.Extents, opts qcowstream.Options, src qcowstream.Reader) ([]byte, *qcowstream.ImagePlan) {
	t.Helper()
	opts.VirtualSize = uint64(len(disk))
	p, err := qcowstream.Plan(context.Background(), ext, opts)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	st, err := qcowstream.Write(context.Background(), writerOnly{&out}, src, ext, p)
	if err != nil {
		t.Fatal(err)
	}
	info := p.Info()
	if uint64(out.Len()) != p.Size() || st.DataClusters != info.DataClusters {
		t.Fatalf("wrote %d bytes and %d data clusters, planned %d and %d", out.Len(), st.DataClusters, p.Size(), info.DataClusters)
	}
	return out.Bytes(), p
}

// zeroed returns disk with every zero extent cleared, so the zero list is true.
func zeroed(disk []byte, zeros qcowstream.SliceExtents) []byte {
	d := bytes.Clone(disk)
	for _, z := range zeros {
		clear(d[z.Offset:z.End()])
	}
	return d
}

func TestFullSkipsZeroClusters(t *testing.T) {
	const cs = 512 // 64 entries per L2 table: 32 KiB each
	size := 1*MiB + 512
	zeros := qcowstream.SliceExtents{
		{0, 1024},                // two whole clusters
		{1536, 256},              // part of a cluster: read
		{32 * KiB, 64 * KiB},     // two whole L2 tables: omitted
		{200*KiB + 100, 2 * KiB}, // unaligned: only the clusters fully inside
		{1 * MiB, 512},           // the partial last cluster
	}
	disk := zeroed(testutil.RandomDisk(size, 1), zeros)
	want := zeroClusters(size, cs, zeros)
	guard := &zeroGuard{disk: disk, cs: cs, zero: want}
	img, p := generateWith(t, disk, qcowstream.Extents{Zeros: zeros}, qcowstream.Options{Mode: qcowstream.Full, ClusterSize: cs}, guard)
	if len(guard.reads) > 0 {
		t.Fatalf("read known-zero clusters: %v", guard.reads)
	}
	im, err := qcow2test.Check(img)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(im.Materialize(nil), disk) {
		t.Fatal("content differs")
	}
	for g := range im.GuestClusters() {
		if wantKind := map[bool]qcow2test.Kind{true: qcow2test.Absent, false: qcow2test.Data}[want[g]]; im.Kind(g) != wantKind {
			t.Fatalf("cluster %d is %v, want %v", g, im.Kind(g), wantKind)
		}
	}
	info := p.Info()
	// 33 tables, minus the two all-zero ones and the last one, which only
	// maps the zero partial cluster.
	if im.L2Tables != 30 || info.L2Tables != 30 {
		t.Fatalf("%d L2 tables written, %d planned; want 30", im.L2Tables, info.L2Tables)
	}
	if info.ZeroClusters != uint64(len(want)) || info.DataClusters+info.ZeroClusters != info.GuestClusters {
		t.Fatalf("%+v", info)
	}
}

func TestIncrementalZeroClustersAreFlagged(t *testing.T) {
	const cs = 64 * KiB
	size := 4 * MiB
	parent := testutil.RandomDisk(size, 2)
	changed := qcowstream.SliceExtents{{0, 256 * KiB}, {1 * MiB, 128 * KiB}}
	zeros := qcowstream.SliceExtents{
		{64 * KiB, 128 * KiB}, // inside the first change
		{1 * MiB, 32 * KiB},   // half a cluster: read
		{2 * MiB, 1 * MiB},    // outside any change: ignored
	}
	child := mutate(parent, changed, false, 3)
	child = zeroed(child, qcowstream.SliceExtents{zeros[0], zeros[1]})
	want := zeroClusters(size, cs, zeros)
	guard := &zeroGuard{disk: child, cs: cs, zero: want}
	ext := qcowstream.Extents{Changed: changed, Zeros: zeros}
	img, p := generateWith(t, child, ext, qcowstream.Options{
		Mode: qcowstream.Incremental, ClusterSize: cs, BackingFile: "base.qcow2"}, guard)
	if len(guard.reads) > 0 {
		t.Fatalf("read known-zero clusters: %v", guard.reads)
	}
	im, err := qcow2test.Check(img)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(im.Materialize(parent), child) {
		t.Fatal("parent + increment differs from the new disk")
	}
	kinds := map[uint64]qcow2test.Kind{0: qcow2test.Data, 1: qcow2test.Zero, 2: qcow2test.Zero, 3: qcow2test.Data, 16: qcow2test.Data, 17: qcow2test.Data}
	for g := range im.GuestClusters() {
		if im.Kind(g) != kinds[g] {
			t.Fatalf("cluster %d is %v, want %v", g, im.Kind(g), kinds[g])
		}
	}
	if info := p.Info(); info.ZeroClusters != 2 || info.DataClusters != 4 || info.SourceBytes != 4*cs {
		t.Fatalf("%+v", info)
	}
}

func TestIncrementalZerosOutsideChangesChangeNothing(t *testing.T) {
	size := 4 * MiB
	disk := testutil.RandomDisk(size, 4)
	changed := qcowstream.SliceExtents{{1 * MiB, 1 * MiB}}
	opts := qcowstream.Options{Mode: qcowstream.Incremental, ClusterSize: 64 * KiB, BackingFile: "base.qcow2"}
	src := qcowstream.NewReaderAtSource(bytes.NewReader(disk), uint64(size))
	without, _ := generateWith(t, disk, qcowstream.Extents{Changed: changed}, opts, src)
	with, _ := generateWith(t, disk, qcowstream.Extents{Changed: changed,
		Zeros: qcowstream.SliceExtents{{0, 1 * MiB}, {2 * MiB, 2 * MiB}}}, opts, src)
	if !bytes.Equal(with, without) {
		t.Fatal("zeros outside the changed regions altered the image")
	}
}

func TestZeroSourceMustReplay(t *testing.T) {
	disk := make([]byte, 1*MiB)
	z := &replays{walks: []qcowstream.SliceExtents{{{0, 64 * KiB}}, {{0, 128 * KiB}}}}
	_, err := qcowstream.Plan(context.Background(), qcowstream.Extents{Zeros: z}, qcowstream.Options{
		Mode: qcowstream.Full, VirtualSize: uint64(len(disk)), ClusterSize: 64 * KiB})
	if !errors.Is(err, qcowstream.ErrUnstableSource) {
		t.Fatalf("plan: %v", err)
	}

	// Stable during planning, different while writing.
	a := qcowstream.SliceExtents{{0, 64 * KiB}}
	for name, walks := range map[string][]qcowstream.SliceExtents{
		"L2 walk":   {a, a, {{0, 128 * KiB}}},
		"data walk": {a, a, a, {}},
	} {
		z := &replays{walks: walks}
		ext := qcowstream.Extents{Zeros: z}
		p, err := qcowstream.Plan(context.Background(), ext, qcowstream.Options{
			Mode: qcowstream.Full, VirtualSize: uint64(len(disk)), ClusterSize: 64 * KiB})
		if err != nil {
			t.Fatal(err)
		}
		_, err = qcowstream.Write(context.Background(), io.Discard, qcowstream.NewReaderAtSource(bytes.NewReader(disk), uint64(len(disk))), ext, p)
		if !errors.Is(err, qcowstream.ErrUnstableSource) {
			t.Fatalf("%s: %v", name, err)
		}
	}
}

func TestWriteRejectsZeroSourceMismatch(t *testing.T) {
	p, err := qcowstream.Plan(context.Background(), qcowstream.Extents{}, qcowstream.Options{Mode: qcowstream.Full, VirtualSize: MiB})
	if err != nil {
		t.Fatal(err)
	}
	_, err = qcowstream.Write(context.Background(), io.Discard, readerFunc(nil), qcowstream.Extents{Zeros: qcowstream.SliceExtents{}}, p)
	if !errors.Is(err, qcowstream.ErrInvalidOptions) {
		t.Fatalf("got %v", err)
	}
}

func TestFileHoles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sparse.raw")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	size := int64(64 * MiB)
	if err := f.Truncate(size); err != nil {
		t.Fatal(err)
	}
	data := testutil.RandomDisk(1*MiB, 5)
	for _, off := range []int64{8 * int64(MiB), 40 * int64(MiB)} {
		if _, err := f.WriteAt(data, off); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.Sync(); err != nil {
		t.Fatal(err)
	}
	holes := qcowstream.FileHoles{F: f, Size: uint64(size)}
	var got []qcowstream.Extent
	if _, err := qcowstream.MergeExtents(context.Background(), holes, uint64(size), func(e qcowstream.Extent) error {
		got = append(got, e)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// Whatever the file system reports, holes must read as zeros and must
	// not cover the written data.
	buf := make([]byte, 1*MiB)
	var holeBytes uint64
	for _, h := range got {
		holeBytes += h.Length
		for off := h.Offset; off < h.End(); off += uint64(len(buf)) {
			n := min(uint64(len(buf)), h.End()-off)
			if _, err := f.ReadAt(buf[:n], int64(off)); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(buf[:n], make([]byte, n)) {
				t.Fatalf("hole [%d, %d) holds data", h.Offset, h.End())
			}
		}
	}
	t.Logf("%d holes, %d of %d bytes", len(got), holeBytes, size)

	// The image of the sparse file equals its content.
	disk := make([]byte, size)
	if _, err := f.ReadAt(disk, 0); err != nil {
		t.Fatal(err)
	}
	img, _ := generateWith(t, disk, qcowstream.Extents{Zeros: holes}, qcowstream.Options{Mode: qcowstream.Full, ClusterSize: 64 * KiB},
		qcowstream.NewReaderAtSource(f, uint64(size)))
	im, err := qcow2test.Check(img)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(im.Materialize(nil), disk) {
		t.Fatal("content differs")
	}
}
