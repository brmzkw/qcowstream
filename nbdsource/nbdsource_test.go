package nbdsource_test

import (
	"bytes"
	"context"
	"errors"

	"math/rand/v2"
	"testing"
	"time"

	"github.com/brmzkw/qcowstream"
	"github.com/brmzkw/qcowstream/nbdsource"
	"github.com/brmzkw/qcowstream/nbdsource/internal/nbdtest"
)

const (
	KiB = uint64(1) << 10
	MiB = KiB << 10
)

func randomDisk(size uint64, seed uint64) []byte {
	r := rand.New(rand.NewPCG(seed, ^seed))
	b := make([]byte, size)
	for i := range b {
		b[i] = byte(r.Uint32())
	}
	return b
}

func open(t *testing.T, srv *nbdtest.Server, conns int) *nbdsource.Source {
	t.Helper()
	sock := srv.Start(t)
	s, err := nbdsource.Open(context.Background(), nbdsource.Options{Socket: sock, Export: srv.Export, Connections: conns})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestReadHonoursBlockSizes(t *testing.T) {
	disk := randomDisk(4*MiB, 1)
	srv := &nbdtest.Server{Export: "scsi0", Data: disk, MinBlock: 512, PrefBlock: 4096, MaxBlock: 192 * 1024}
	s := open(t, srv, 1)
	if s.Size() != uint64(len(disk)) {
		t.Fatalf("size %d", s.Size())
	}
	buf := make([]byte, 1*MiB)
	n, err := s.ReadAtContext(context.Background(), buf, 512)
	if err != nil || n != len(buf) {
		t.Fatalf("read %d: %v", n, err)
	}
	if !bytes.Equal(buf, disk[512:512+len(buf)]) {
		t.Fatal("data differs")
	}
	if v := srv.Violations(); len(v) > 0 {
		t.Fatalf("violations: %v", v)
	}
	for _, r := range srv.Reads() {
		if r.Length > 192*1024 || r.Length%512 != 0 {
			t.Fatalf("request of %d bytes", r.Length)
		}
	}
	if _, err := s.ReadAtContext(context.Background(), buf[:100], 0); err == nil {
		t.Fatal("unaligned read accepted")
	}
	if _, err := s.ReadAtContext(context.Background(), buf, int64(len(disk))); err == nil {
		t.Fatal("read past the end accepted")
	}
}

func TestReadWithoutBlockSizeInfo(t *testing.T) {
	disk := randomDisk(40*MiB, 2)
	srv := &nbdtest.Server{Export: "scsi0", Data: disk}
	s := open(t, srv, 1)
	buf := make([]byte, 40*MiB)
	if _, err := s.ReadAtContext(context.Background(), buf, 0); err != nil {
		t.Fatal(err)
	}
	for _, r := range srv.Reads() {
		if r.Length > 32*MiB {
			t.Fatalf("request of %d bytes over the 32 MiB default", r.Length)
		}
	}
}

func TestOpenUnknownExport(t *testing.T) {
	srv := &nbdtest.Server{Export: "scsi0", Data: make([]byte, MiB)}
	sock := srv.Start(t)
	if _, err := nbdsource.Open(context.Background(), nbdsource.Options{Socket: sock, Export: "scsi1"}); err == nil {
		t.Fatal("opened an unknown export")
	}
}

func TestCancelInterruptsStalledServer(t *testing.T) {
	srv := &nbdtest.Server{Export: "scsi0", Data: make([]byte, MiB)}
	s := open(t, srv, 1)
	srv.Stall.Store(true)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := s.ReadAtContext(ctx, make([]byte, 4096), 0)
	if !errors.Is(err, qcowstream.ErrCanceled) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("cancellation took too long")
	}
	// The interrupted connection is replaced on the next read.
	srv.Stall.Store(false)
	if _, err := s.ReadAtContext(context.Background(), make([]byte, 4096), 0); err != nil {
		t.Fatalf("read after cancel: %v", err)
	}
}

// TestFullImageOverNBD generates a full image over NBD with several
// connections and compares it with the image of the same bytes from memory.
func TestFullImageOverNBD(t *testing.T) {
	disk := randomDisk(8*MiB+512, 3)
	srv := &nbdtest.Server{Export: "scsi0", Data: disk, MinBlock: 512, PrefBlock: 4096, MaxBlock: 1 << 20}
	s := open(t, srv, 4)

	gen := func(src qcowstream.Reader) []byte {
		opts := qcowstream.Options{Mode: qcowstream.Full, VirtualSize: uint64(len(disk)), ClusterSize: 64 * KiB,
			BufferSize: 256 * KiB, Concurrency: 4}
		p, err := qcowstream.Plan(context.Background(), qcowstream.Extents{}, opts)
		if err != nil {
			t.Fatal(err)
		}
		var b bytes.Buffer
		if _, err := qcowstream.Write(context.Background(), &b, src, qcowstream.Extents{}, p); err != nil {
			t.Fatal(err)
		}
		return b.Bytes()
	}
	got := gen(s)
	want := gen(qcowstream.NewReaderAtSource(bytes.NewReader(disk), uint64(len(disk))))
	if !bytes.Equal(got, want) {
		t.Fatal("image over NBD differs from the image from memory")
	}
	if n := srv.MaxConns(); n < 2 || n > 4 {
		t.Fatalf("%d simultaneous connections, want 2 to 4", n)
	}
	if v := srv.Violations(); len(v) > 0 {
		t.Fatalf("violations: %v", v)
	}

}
