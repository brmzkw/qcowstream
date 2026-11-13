package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/brmzkw/qcowstream"
)

func generate(t *testing.T, disk []byte, ext qcowstream.Extents, opts qcowstream.Options, src qcowstream.Reader) []byte {
	t.Helper()
	opts.VirtualSize = uint64(len(disk))
	p, err := qcowstream.Plan(context.Background(), ext, opts)
	if err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	if _, err := qcowstream.Write(context.Background(), &b, src, ext, p); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

type mapEntry struct {
	Start  uint64 `json:"start"`
	Length uint64 `json:"length"`
	Data   bool   `json:"data"`
	Zero   bool   `json:"zero"`
}

// TestSparseFullImage generates a full image from a sparse file using its
// holes, and checks QEMU sees them unallocated and the content is intact.
func TestSparseFullImage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sparse.raw")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	size := 32 * MiB
	if err := f.Truncate(int64(size)); err != nil {
		t.Fatal(err)
	}
	for _, off := range []uint64{4 * MiB, 20 * MiB} {
		if _, err := f.WriteAt(randomDisk(1*MiB, off), int64(off)); err != nil {
			t.Fatal(err)
		}
	}
	disk := make([]byte, size)
	if _, err := f.ReadAt(disk, 0); err != nil {
		t.Fatal(err)
	}
	holes := qcowstream.FileHoles{F: f, Size: size}
	img := generate(t, disk, qcowstream.Extents{Zeros: holes},
		qcowstream.Options{Mode: qcowstream.Full, ClusterSize: 64 * KiB}, qcowstream.NewReaderAtSource(f, size))
	if uint64(len(img)) > 8*MiB {
		t.Fatalf("image of a 2 MiB-used sparse disk is %d bytes", len(img))
	}

	each(t, func(t *testing.T, e *qemuEnv) {
		dir := e.dir(t)
		e.put(t, dir+"/full.qcow2", img)
		checkImage(t, e, dir, "full.qcow2", disk)
		var entries []mapEntry
		if err := json.Unmarshal([]byte(e.run(t, "qemu-img map --output=json "+dir+"/full.qcow2")), &entries); err != nil {
			t.Fatal(err)
		}
		var allocated uint64
		for _, m := range entries {
			if m.Data {
				allocated += m.Length
			}
		}
		if allocated > 8*MiB {
			t.Fatalf("QEMU sees %d allocated bytes", allocated)
		}
	})
}

// TestZeroFlaggedIncrement writes zero-flagged clusters over a non-zero
// parent: they must read as zeros through the chain.
func TestZeroFlaggedIncrement(t *testing.T) {
	size := 4 * MiB
	raw0 := randomDisk(size, 50)
	raw1, changed := apply(raw0, 60,
		change{off: 0, len: 1 * MiB, zero: true},
		change{off: 2 * MiB, len: 512 * KiB},
		change{off: 3 * MiB, len: 1 * MiB, zero: true},
	)
	zeros := qcowstream.SliceExtents{{Offset: 0, Length: 1 * MiB}, {Offset: 3 * MiB, Length: 1 * MiB}}
	inc := generate(t, raw1, qcowstream.Extents{Changed: changed, Zeros: zeros},
		qcowstream.Options{Mode: qcowstream.Incremental, ClusterSize: 64 * KiB, BackingFile: "base.qcow2"},
		qcowstream.NewReaderAtSource(bytes.NewReader(raw1), size))
	if uint64(len(inc)) > 1*MiB {
		t.Fatalf("increment is %d bytes: zero clusters were stored", len(inc))
	}
	each(t, func(t *testing.T, e *qemuEnv) {
		dir := e.dir(t)
		e.put(t, dir+"/base.qcow2", fullImage(t, raw0, 64*KiB))
		e.put(t, dir+"/inc.qcow2", inc)
		checkImage(t, e, dir, "inc.qcow2", raw1)
	})
}
