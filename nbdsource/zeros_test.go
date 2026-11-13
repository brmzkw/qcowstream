package nbdsource_test

import (
	"bytes"
	"context"
	"slices"
	"testing"

	"github.com/brmzkw/qcowstream"
	"github.com/brmzkw/qcowstream/internal/nbdtest"
	"github.com/brmzkw/qcowstream/internal/qcow2test"
	"github.com/brmzkw/qcowstream/nbdsource"
)

func openZeros(t *testing.T, srv *nbdtest.Server, bitmap string) *nbdsource.Source {
	t.Helper()
	sock := srv.Start(t)
	s, err := nbdsource.Open(context.Background(), nbdsource.Options{
		Socket: sock, Export: srv.Export, Bitmap: bitmap, DetectZeros: true, Connections: 2})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestZerosFull(t *testing.T) {
	for name, tweak := range map[string]func(*nbdtest.Server){
		"plain":      func(*nbdtest.Server) {},
		"fragmented": func(s *nbdtest.Server) { s.ExtentsPerChunk = 1; s.StatusLimit = 3 * MiB },
	} {
		t.Run(name, func(t *testing.T) {
			srv := &nbdtest.Server{Export: "scsi0", Size: 64 * MiB, BaseAllocation: true,
				Zero: []nbdtest.Extent{{Offset: 0, Length: 8 * MiB}, {Offset: 32 * MiB, Length: 16 * MiB}},
				Hole: []nbdtest.Extent{{Offset: 50 * MiB, Length: 4 * MiB}}, // HOLE without ZERO: unknown content
			}
			tweak(srv)
			s := openZeros(t, srv, "")
			got, err := walk(t, s.Zeros(), s.Size())
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(got, wantExtents(srv.Zero)) {
				t.Fatalf("got %v", got)
			}
		})
	}
}

func TestZerosWithoutBaseAllocation(t *testing.T) {
	srv := &nbdtest.Server{Export: "scsi0", Size: 4 * MiB}
	s := openZeros(t, srv, "")
	if s.Zeros() != nil {
		t.Fatal("zero information from a server without base:allocation")
	}
}

// TestZerosIncremental checks allocation is only queried inside dirty
// extents, and that zero clusters there skip the read.
func TestZerosIncremental(t *testing.T) {
	size := 64 * MiB
	parent := randomDisk(size, 30)
	child := bytes.Clone(parent)
	for i, d := range dirty {
		copy(child[d.Offset:d.Offset+d.Length], randomDisk(d.Length, uint64(40+i)))
	}
	zero := []nbdtest.Extent{
		{Offset: 2 * MiB, Length: 12 * MiB}, // straddles a clean region
		{Offset: 60 * MiB, Length: 4 * MiB},
	}
	want := []qcowstream.Extent{{Offset: 2 * MiB, Length: 2 * MiB}, {Offset: 12 * MiB, Length: 2 * MiB}, {Offset: 60 * MiB, Length: 4 * MiB}}
	for _, z := range want { // the zero parts of the dirty regions
		clear(child[z.Offset:z.End()])
	}
	srv := &nbdtest.Server{Export: "scsi0", Data: child, Bitmap: "backup", Dirty: dirty,
		BaseAllocation: true, Zero: zero, ExtentsPerChunk: 1}
	s := openZeros(t, srv, "backup")

	got, err := walk(t, s.Zeros(), s.Size())
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("zeros %v, want %v", got, want)
	}

	ext := qcowstream.Extents{Changed: s.Changed(), Zeros: s.Zeros()}
	p, err := qcowstream.Plan(context.Background(), ext, qcowstream.Options{
		Mode: qcowstream.Incremental, VirtualSize: s.Size(), BackingFile: "base.qcow2", Concurrency: 2})
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	st, err := qcowstream.Write(context.Background(), &out, s, ext, p)
	if err != nil {
		t.Fatal(err)
	}
	if v := srv.Violations(); len(v) > 0 {
		t.Fatalf("forbidden accesses: %v", v)
	}
	for _, r := range srv.Reads() {
		for _, z := range want {
			if r.Offset < z.End() && z.Offset < r.Offset+r.Length {
				t.Fatalf("read [%d, +%d) of a zero region", r.Offset, r.Length)
			}
		}
	}
	if st.SourceBytesRead != 16*MiB-8*MiB {
		t.Fatalf("read %d bytes", st.SourceBytesRead)
	}
	im, err := qcow2test.Check(out.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(im.Materialize(parent), child) {
		t.Fatal("parent + increment differs from the export")
	}
}
