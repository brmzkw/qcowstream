package qcowstream_test

import (
	"bytes"
	"context"
	"runtime"
	"testing"
	"time"

	"github.com/brmzkw/qcowstream"
	"github.com/brmzkw/qcowstream/internal/qcow2test"
	"github.com/brmzkw/qcowstream/internal/virtual"
)

// allocated returns the bytes allocated while f runs.
func allocated(f func()) uint64 {
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	f()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}

// TestPlanFragmented100TiB plans an increment of a million alternating
// clusters on a 100 TiB disk: memory stays bounded by the L1 summary, not
// by the number of extents.
func TestPlanFragmented100TiB(t *testing.T) {
	if testing.Short() {
		t.Skip("plans a million extents")
	}
	changed := virtual.Strided{Start: 0, Stride: 128 * KiB, Length: 64 * KiB, Count: 1 << 20}
	var p *qcowstream.ImagePlan
	var err error
	start := time.Now()
	alloc := allocated(func() {
		p, err = planInc(100*TiB, 64*KiB, changed)
	})
	if err != nil {
		t.Fatal(err)
	}
	info := p.Info()
	checkLayoutInvariants(t, info)
	if info.DataClusters != 1<<20 || info.L2Tables != 256 {
		t.Fatalf("%d data clusters, %d L2 tables", info.DataClusters, info.L2Tables)
	}
	// The touched-L1 bitset of 204800 entries is 25 KiB; walks are constant.
	if alloc > 1*MiB {
		t.Fatalf("planning allocated %d bytes", alloc)
	}
	t.Logf("planned 2 x %d extents in %v, %d bytes allocated", 1<<20, time.Since(start), alloc)
}

// TestWriteSmallIncrementOn100TiB emits an increment on a 100 TiB virtual
// disk without writing 100 TiB, and checks it with the reference parser.
func TestWriteSmallIncrementOn100TiB(t *testing.T) {
	disk := virtual.Disk{Len: 100 * TiB}
	changed := virtual.Strided{Start: 1 * GiB, Stride: 1 * TiB, Length: 4 * MiB, Count: 100}
	ext := qcowstream.Extents{Changed: changed}
	p, err := qcowstream.Plan(context.Background(), ext, qcowstream.Options{
		Mode: qcowstream.Incremental, VirtualSize: disk.Len, ClusterSize: 64 * KiB, BackingFile: "base.qcow2"})
	if err != nil {
		t.Fatal(err)
	}
	info := p.Info()
	var out bytes.Buffer
	out.Grow(int(info.ImageSize))
	var st qcowstream.Stats
	alloc := allocated(func() {
		st, err = qcowstream.Write(context.Background(), &out, disk, ext, p)
	})
	if err != nil {
		t.Fatal(err)
	}
	if uint64(out.Len()) != info.ImageSize || st.SourceBytesRead != 400*MiB {
		t.Fatalf("wrote %d bytes, read %d", out.Len(), st.SourceBytesRead)
	}
	if alloc > info.MemoryEstimate+1*MiB {
		t.Fatalf("writer allocated %d bytes, estimate %d", alloc, info.MemoryEstimate)
	}

	im, err := qcow2test.Check(out.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if data, zero := im.Allocated(); data != 100*64 || zero != 0 || im.GuestClusters() != 100*TiB/(64*KiB) {
		t.Fatalf("%d data, %d zero, %d guest clusters", data, zero, im.GuestClusters())
	}
	for i := range uint64(100) {
		off := 1*GiB + i*TiB
		got := im.Read(off-64*KiB, 4*MiB+128*KiB, nil)
		want := make([]byte, len(got))
		_, _ = disk.ReadAtContext(context.Background(), want[64*KiB:64*KiB+4*MiB], int64(off))
		if !bytes.Equal(got, want) {
			t.Fatalf("region %d differs", i)
		}
	}
}

func TestPlanFull100TiBWithZeros(t *testing.T) {
	// A 100 TiB disk with 10 GiB of data at the end of each TiB. One L2
	// table maps 512 GiB at 2 MiB clusters: one table per TiB.
	zeros := virtual.Strided{Start: 0, Stride: 1 * TiB, Length: 1*TiB - 10*GiB, Count: 100}
	p, err := qcowstream.Plan(context.Background(), qcowstream.Extents{Zeros: zeros}, qcowstream.Options{
		Mode: qcowstream.Full, VirtualSize: 100 * TiB})
	if err != nil {
		t.Fatal(err)
	}
	info := p.Info()
	checkLayoutInvariants(t, info)
	if info.SourceBytes != 100*10*GiB || info.ClusterSize != 2*MiB || info.L2Tables != 100 {
		t.Fatalf("%+v", info)
	}
}
