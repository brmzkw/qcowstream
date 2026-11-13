package integration

import (
	"bytes"
	"context"
	"fmt"
	"testing"

	"github.com/brmzkw/qcowstream"
	"github.com/brmzkw/qcowstream/internal/testutil"
)

func incrementalImage(t *testing.T, disk []byte, ext qcowstream.SliceExtents, backing string, cs uint64) []byte {
	t.Helper()
	exts := qcowstream.Extents{Changed: ext}
	p, err := qcowstream.Plan(context.Background(), exts, qcowstream.Options{
		Mode: qcowstream.Incremental, VirtualSize: uint64(len(disk)), ClusterSize: cs,
		BackingFile: backing, BackingFormat: "qcow2",
	})
	if err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	if _, err := qcowstream.Write(context.Background(), &b, qcowstream.NewReaderAtSource(bytes.NewReader(disk), uint64(len(disk))), exts, p); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// checkAlone checks an image with its backing chain disabled, as it exists
// when generated: the parent is absent.
func checkAlone(t *testing.T, e *qemuEnv, dir, image string) {
	t.Helper()
	e.run(t, fmt.Sprintf(`cd %s && qemu-img check 'json:{"driver":"qcow2","backing":null,"file":{"driver":"file","filename":"%s"}}'`, dir, image))
	// Opened normally, the image must look for a parent that is not there.
	e.fails(t, fmt.Sprintf("cd %s && qemu-img info --backing-chain %s", dir, image))
}

func TestIncrementalChain(t *testing.T) {
	const cs = 4 * KiB // one L2 table maps 2 MiB
	size := 8*MiB + 512
	raw0 := testutil.RandomDisk(size, 10)
	raw1, ext1 := testutil.Apply(raw0, 20,
		testutil.Change{Offset: 0, Length: 4 * KiB},
		testutil.Change{Offset: 2*MiB - 4*KiB, Length: 8 * KiB}, // across an L2 boundary
		testutil.Change{Offset: 5 * MiB, Length: 64 * KiB},      // changed again in inc-2
		testutil.Change{Offset: 6 * MiB, Length: 8 * KiB, Zero: true},
		testutil.Change{Offset: 8 * MiB, Length: 512}, // partial last cluster
	)
	raw2, ext2 := testutil.Apply(raw1, 30,
		testutil.Change{Offset: 0, Length: 4 * KiB, Zero: true}, // zeros over data from inc-1
		testutil.Change{Offset: 3 * MiB, Length: 16 * KiB, Zero: true},
		testutil.Change{Offset: 4*MiB - 4*KiB, Length: 4 * KiB}, // last cluster of an L2 table
		testutil.Change{Offset: 5 * MiB, Length: 64 * KiB},
	)
	images := map[string][]byte{
		"base.qcow2":  fullImage(t, raw0, 64*KiB), // the chain mixes cluster sizes
		"inc-1.qcow2": incrementalImage(t, raw1, ext1, "base.qcow2", cs),
		"inc-2.qcow2": incrementalImage(t, raw2, ext2, "inc-1.qcow2", cs),
		"inc-3.qcow2": incrementalImage(t, raw2, nil, "inc-2.qcow2", cs), // no change
	}
	states := []struct {
		top  string
		want []byte
	}{
		{"base.qcow2", raw0}, {"inc-1.qcow2", raw1}, {"inc-2.qcow2", raw2}, {"inc-3.qcow2", raw2},
	}

	each(t, func(t *testing.T, e *qemuEnv) {
		// Each increment exists alone, as when it was generated.
		for name, img := range images {
			if name == "base.qcow2" {
				continue
			}
			dir := e.dir(t) + "-" + name
			e.run(t, "mkdir -p "+dir)
			e.put(t, dir+"/"+name, img)
			checkAlone(t, e, dir, name)
		}
		// Restore: parents at the recorded relative paths.
		dir := e.dir(t)
		for name, img := range images {
			e.put(t, dir+"/"+name, img)
		}
		for _, s := range states {
			checkImage(t, e, dir, s.top, s.want)
		}
		chain := e.run(t, "cd "+dir+" && qemu-img info --backing-chain inc-3.qcow2 | grep '^image:'")
		want := "image: inc-3.qcow2\nimage: inc-2.qcow2\nimage: inc-1.qcow2\nimage: base.qcow2\n"
		if chain != want {
			t.Fatalf("chain:\n%s", chain)
		}
	})
}

// TestZeroOnlyIncrement checks zeros written over non-zero parent data
// replace it instead of falling through to the parent.
func TestZeroOnlyIncrement(t *testing.T) {
	size := 4 * MiB
	raw0 := testutil.RandomDisk(size, 40)
	raw1, ext := testutil.Apply(raw0, 0,
		testutil.Change{Offset: 0, Length: 1 * MiB, Zero: true},
		testutil.Change{Offset: 3 * MiB, Length: 1 * MiB, Zero: true},
	)
	each(t, func(t *testing.T, e *qemuEnv) {
		dir := e.dir(t)
		e.put(t, dir+"/base.qcow2", fullImage(t, raw0, 64*KiB))
		e.put(t, dir+"/inc.qcow2", incrementalImage(t, raw1, ext, "base.qcow2", 64*KiB))
		checkImage(t, e, dir, "inc.qcow2", raw1)
	})
}
