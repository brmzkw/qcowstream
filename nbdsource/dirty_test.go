package nbdsource_test

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/brmzkw/qcowstream"
	"github.com/brmzkw/qcowstream/internal/nbdtest"
	"github.com/brmzkw/qcowstream/internal/qcow2test"
	"github.com/brmzkw/qcowstream/internal/testutil"
	"github.com/brmzkw/qcowstream/nbdsource"
)

func openBitmap(t *testing.T, srv *nbdtest.Server, bitmap string) (*nbdsource.Source, error) {
	t.Helper()
	sock := srv.Start(t)
	s, err := nbdsource.Open(context.Background(), nbdsource.Options{Socket: sock, Export: srv.Export, Bitmap: bitmap})
	if err == nil {
		t.Cleanup(func() { s.Close() })
	}
	return s, err
}

func walk(t *testing.T, src qcowstream.ExtentSource, size uint64) ([]qcowstream.Extent, error) {
	t.Helper()
	var out []qcowstream.Extent
	_, err := qcowstream.MergeExtents(context.Background(), src, size, func(e qcowstream.Extent) error {
		out = append(out, e)
		return nil
	})
	return out, err
}

var dirty = []nbdtest.Extent{
	{Offset: 0, Length: 4 * MiB},
	{Offset: 12 * MiB, Length: 8 * MiB},
	{Offset: 60 * MiB, Length: 4 * MiB},
}

func wantExtents(d []nbdtest.Extent) []qcowstream.Extent {
	var out []qcowstream.Extent
	for _, e := range d {
		out = append(out, qcowstream.Extent{Offset: e.Offset, Length: e.Length})
	}
	return out
}

func TestDirtyRegions(t *testing.T) {
	cases := map[string]func(*nbdtest.Server){
		"plain":                 func(*nbdtest.Server) {},
		"one extent per chunk":  func(s *nbdtest.Server) { s.ExtentsPerChunk = 1 },
		"short replies":         func(s *nbdtest.Server) { s.StatusLimit = 3 * MiB },
		"fragmented and short":  func(s *nbdtest.Server) { s.ExtentsPerChunk = 1; s.StatusLimit = 5 * MiB },
		"block size constraint": func(s *nbdtest.Server) { s.MinBlock, s.PrefBlock, s.MaxBlock = 4096, 4096, 1<<20 },
	}
	for name, tweak := range cases {
		t.Run(name, func(t *testing.T) {
			srv := &nbdtest.Server{Export: "scsi0", Data: make([]byte, 64*MiB), Bitmap: "backup", Dirty: dirty}
			tweak(srv)
			s, err := openBitmap(t, srv, "backup")
			if err != nil {
				t.Fatal(err)
			}
			for range 2 { // replayable
				got, err := walk(t, s.Changed(), s.Size())
				if err != nil {
					t.Fatal(err)
				}
				if !slices.Equal(got, wantExtents(dirty)) {
					t.Fatalf("got %v", got)
				}
			}
			if v := srv.Violations(); len(v) > 0 {
				t.Fatalf("violations: %v", v)
			}
		})
	}
}

func TestDirtyRegionsLargeDisk(t *testing.T) {
	// A disk larger than one 32-bit request: the walk spans two windows.
	srv := &nbdtest.Server{Export: "scsi0", Size: 5*1024*MiB + 512, Bitmap: "backup",
		Dirty: []nbdtest.Extent{{Offset: 1023 * MiB, Length: 2 * MiB}, {Offset: 5 * 1024 * MiB, Length: 512}}}
	s, err := openBitmap(t, srv, "backup")
	if err != nil {
		t.Fatal(err)
	}
	got, err := walk(t, s.Changed(), s.Size())
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, wantExtents(srv.Dirty)) {
		t.Fatalf("got %v", got)
	}
}

func TestMissingBitmapContext(t *testing.T) {
	for name, srv := range map[string]*nbdtest.Server{
		"unknown bitmap":  {Export: "scsi0", Data: make([]byte, MiB), Bitmap: "other"},
		"no bitmaps":      {Export: "scsi0", Data: make([]byte, MiB)},
		"context refused": {Export: "scsi0", Data: make([]byte, MiB), Bitmap: "backup", RefuseMetaContext: true},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := openBitmap(t, srv, "backup")
			if !errors.Is(err, nbdsource.ErrMissingContext) {
				t.Fatalf("got %v", err)
			}
		})
	}
}

func TestMalformedBlockStatus(t *testing.T) {
	for name, tweak := range map[string]func(*nbdtest.Server){
		"wrong context id":   func(s *nbdtest.Server) { s.WrongContextID = true },
		"zero-length extent": func(s *nbdtest.Server) { s.ZeroLengthExtent = true },
	} {
		t.Run(name, func(t *testing.T) {
			srv := &nbdtest.Server{Export: "scsi0", Data: make([]byte, 64*MiB), Bitmap: "backup", Dirty: dirty}
			tweak(srv)
			s, err := openBitmap(t, srv, "backup")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := walk(t, s.Changed(), s.Size()); err == nil {
				t.Fatal("malformed reply accepted")
			}
		})
	}
}

// TestIncrementalOverNBD generates an incremental image from a server that
// refuses every read outside the dirty regions, like a Proxmox export.
func TestIncrementalOverNBD(t *testing.T) {
	size := 64 * MiB
	parent := testutil.RandomDisk(size, 10)
	child := bytes.Clone(parent)
	for i, d := range dirty {
		copy(child[d.Offset:d.Offset+d.Length], testutil.RandomDisk(d.Length, uint64(20+i)))
	}
	clear(child[12*MiB : 14*MiB]) // zeros inside a dirty region are data too

	srv := &nbdtest.Server{Export: "scsi0", Data: child, Bitmap: "backup", Dirty: dirty,
		MinBlock: 512, PrefBlock: 4096, MaxBlock: 1 << 20, ExtentsPerChunk: 1}
	sock := srv.Start(t)
	s, err := nbdsource.Open(context.Background(), nbdsource.Options{Socket: sock, Export: "scsi0", Bitmap: "backup", Connections: 4})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// The server enforces the rule this test relies on.
	if _, err := s.ReadAtContext(context.Background(), make([]byte, 4096), int64(5*MiB)); err == nil {
		t.Fatal("test server allowed a read outside the dirty regions")
	}
	srv2 := len(srv.Violations())

	ext := qcowstream.Extents{Changed: s.Changed()}
	p, err := qcowstream.Plan(context.Background(), ext, qcowstream.Options{
		Mode: qcowstream.Incremental, VirtualSize: s.Size(), BackingFile: "base.qcow2", Concurrency: 4})
	if err != nil {
		t.Fatal(err)
	}
	if cs := p.Info().ClusterSize; cs != 2*MiB {
		t.Fatalf("auto cluster size %d for a 4 MiB-aligned bitmap", cs)
	}
	var out bytes.Buffer
	if _, err := qcowstream.Write(context.Background(), &out, s, ext, p); err != nil {
		t.Fatal(err)
	}
	if v := srv.Violations()[srv2:]; len(v) > 0 {
		t.Fatalf("forbidden accesses: %v", v)
	}
	im, err := qcow2test.Check(out.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(im.Materialize(parent), child) {
		t.Fatal("parent + increment differs from the export")
	}
}
