package qcowstream_test

import (
	"context"
	"errors"
	"runtime"
	"testing"

	"github.com/brmzkw/qcowstream"
)

const (
	KiB = uint64(1) << 10
	MiB = KiB << 10
	GiB = MiB << 10
	TiB = GiB << 10
)

func planFull(t testing.TB, size, cs uint64) (*qcowstream.ImagePlan, error) {
	t.Helper()
	return qcowstream.Plan(context.Background(), qcowstream.Extents{}, qcowstream.Options{
		Mode: qcowstream.Full, VirtualSize: size, ClusterSize: cs,
	})
}

// checkLayoutInvariants verifies properties every plan must satisfy,
// independently of how the plan computed them.
func checkLayoutInvariants(t testing.TB, info qcowstream.PlanInfo) {
	t.Helper()
	cs := info.ClusterSize
	l1Clusters := (info.L1Entries*8 + cs - 1) / cs
	parts := 1 + info.RefcountTableClusters + info.RefcountBlocks + l1Clusters + info.L2Tables + info.DataClusters
	if parts != info.TotalClusters {
		t.Fatalf("total clusters %d, structures add up to %d", info.TotalClusters, parts)
	}
	if info.ImageSize != info.TotalClusters*cs {
		t.Fatalf("image size %d is not %d clusters of %d", info.ImageSize, info.TotalClusters, cs)
	}
	perBlock := cs / 2
	if info.RefcountBlocks*perBlock < info.TotalClusters {
		t.Fatalf("%d refcount blocks cannot count %d clusters", info.RefcountBlocks, info.TotalClusters)
	}
	if info.RefcountBlocks > 0 && (info.RefcountBlocks-1)*perBlock >= info.TotalClusters {
		t.Fatalf("%d refcount blocks is more than needed for %d clusters", info.RefcountBlocks, info.TotalClusters)
	}
	perTable := cs / 8
	if info.RefcountTableClusters*perTable < info.RefcountBlocks ||
		(info.RefcountTableClusters-1)*perTable >= info.RefcountBlocks {
		t.Fatalf("refcount table of %d clusters for %d blocks", info.RefcountTableClusters, info.RefcountBlocks)
	}
	if info.L1Entries*(cs/8)*cs < info.VirtualSize {
		t.Fatalf("L1 of %d entries does not map %d bytes", info.L1Entries, info.VirtualSize)
	}
	if info.GuestClusters*cs < info.VirtualSize || (info.GuestClusters-1)*cs >= info.VirtualSize {
		t.Fatalf("%d guest clusters for %d bytes", info.GuestClusters, info.VirtualSize)
	}
}

func TestPlanFull100TiB(t *testing.T) {
	cases := []struct {
		cs       uint64
		l1Bytes  uint64
		l2Bytes  uint64
		l2Tables uint64
	}{
		// Figures from the spec: L1 1.5625 MiB, L2 12.5 GiB at 64 KiB.
		{64 * KiB, 1600 * KiB, 12*GiB + 512*MiB, 204800},
		{2 * MiB, 1600, 400 * MiB, 200},
	}
	for _, c := range cases {
		var before, after runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&before)
		p, err := planFull(t, 100*TiB, c.cs)
		runtime.ReadMemStats(&after)
		if err != nil {
			t.Fatalf("cluster size %d: %v", c.cs, err)
		}
		if alloc := after.TotalAlloc - before.TotalAlloc; alloc > 64*KiB {
			t.Fatalf("planning 100 TiB allocated %d bytes", alloc)
		}
		info := p.Info()
		checkLayoutInvariants(t, info)
		if info.L1Entries*8 != c.l1Bytes {
			t.Fatalf("cluster size %d: L1 is %d bytes, want %d", c.cs, info.L1Entries*8, c.l1Bytes)
		}
		if info.L2Tables != c.l2Tables || info.L2Tables*c.cs != c.l2Bytes {
			t.Fatalf("cluster size %d: %d L2 tables", c.cs, info.L2Tables)
		}
		if info.DataClusters != 100*TiB/c.cs || info.SourceBytes != 100*TiB {
			t.Fatalf("cluster size %d: %d data clusters, %d source bytes", c.cs, info.DataClusters, info.SourceBytes)
		}
		if info.MemoryEstimate > qcowstream.DefaultMemoryLimit {
			t.Fatalf("memory estimate %d over the default budget", info.MemoryEstimate)
		}
	}
}

func TestPlanFullAutoClusterSize(t *testing.T) {
	p, err := planFull(t, 10*GiB, qcowstream.ClusterSizeAuto)
	if err != nil {
		t.Fatal(err)
	}
	if cs := p.Info().ClusterSize; cs != 2*MiB {
		t.Fatalf("auto picked %d", cs)
	}
}

func TestPlanFullPartialLastCluster(t *testing.T) {
	p, err := planFull(t, 64*KiB+512, 64*KiB)
	if err != nil {
		t.Fatal(err)
	}
	info := p.Info()
	checkLayoutInvariants(t, info)
	if info.DataClusters != 2 || info.SourceBytes != 64*KiB+512 {
		t.Fatalf("%+v", info)
	}
}

// TestPlanFullRefcountTableLimit finds the largest disk a 512-byte cluster
// full image supports and checks both sides of the limit.
func TestPlanFullRefcountTableLimit(t *testing.T) {
	lo, hi := uint64(1), uint64(1)<<30 // in 512-byte sectors
	for lo+1 < hi {
		mid := (lo + hi) / 2
		if _, err := planFull(t, mid*512, 512); err == nil {
			lo = mid
		} else {
			hi = mid
		}
	}
	p, err := planFull(t, lo*512, 512)
	if err != nil {
		t.Fatal(err)
	}
	info := p.Info()
	checkLayoutInvariants(t, info)
	if info.RefcountTableClusters*512 > 8*MiB || info.L1Entries*8 > 32*MiB {
		t.Fatalf("largest plan breaks a QEMU limit: %+v", info)
	}
	_, err = planFull(t, hi*512, 512)
	var ge *qcowstream.GranularityError
	if !errors.As(err, &ge) || !errors.Is(err, qcowstream.ErrUnsupportedGranularity) {
		t.Fatalf("one sector more: %v", err)
	}
	if lo*512 < 100*GiB {
		t.Fatalf("512-byte clusters stop at %d bytes, expected over 100 GiB", lo*512)
	}
}

func TestPlanFullHostOffsetLimit(t *testing.T) {
	if _, err := planFull(t, 1<<55, 2*MiB); err != nil {
		t.Fatalf("32 PiB: %v", err)
	}
	if _, err := planFull(t, 1<<56, 2*MiB); !errors.Is(err, qcowstream.ErrUnsupportedGranularity) {
		t.Fatalf("64 PiB: %v", err)
	}
}

func TestPlanRejectsInvalidOptions(t *testing.T) {
	full := func(f func(*qcowstream.Options)) qcowstream.Options {
		o := qcowstream.Options{Mode: qcowstream.Full, VirtualSize: GiB}
		f(&o)
		return o
	}
	cases := map[string]struct {
		opts qcowstream.Options
		ext  qcowstream.Extents
	}{
		"no mode":                 {opts: qcowstream.Options{VirtualSize: GiB}},
		"zero size":               {opts: full(func(o *qcowstream.Options) { o.VirtualSize = 0 })},
		"unaligned size":          {opts: full(func(o *qcowstream.Options) { o.VirtualSize = GiB + 1 })},
		"size past int64":         {opts: full(func(o *qcowstream.Options) { o.VirtualSize = 1 << 63 })},
		"cluster not power of 2":  {opts: full(func(o *qcowstream.Options) { o.ClusterSize = 3 * 64 * KiB })},
		"cluster too small":       {opts: full(func(o *qcowstream.Options) { o.ClusterSize = 256 })},
		"cluster too large":       {opts: full(func(o *qcowstream.Options) { o.ClusterSize = 4 * MiB })},
		"full with backing":       {opts: full(func(o *qcowstream.Options) { o.BackingFile = "base.qcow2" })},
		"full with format":        {opts: full(func(o *qcowstream.Options) { o.BackingFormat = "qcow2" })},
		"full with regions":       {opts: full(func(*qcowstream.Options) {}), ext: qcowstream.Extents{Changed: qcowstream.SliceExtents{}}},
		"negative concurrency":    {opts: full(func(o *qcowstream.Options) { o.Concurrency = -1 })},
		"huge concurrency":        {opts: full(func(o *qcowstream.Options) { o.Concurrency = 100000 })},
		"buffer not cluster mult": {opts: full(func(o *qcowstream.Options) { o.ClusterSize = 64 * KiB; o.BufferSize = 100 * KiB })},
		"incremental no regions": {opts: qcowstream.Options{
			Mode: qcowstream.Incremental, VirtualSize: GiB, BackingFile: "base.qcow2"}},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := qcowstream.Plan(context.Background(), c.ext, c.opts)
			if !errors.Is(err, qcowstream.ErrInvalidOptions) {
				t.Fatalf("got %v", err)
			}
		})
	}
}

func TestPlanMemoryBudget(t *testing.T) {
	_, err := qcowstream.Plan(context.Background(), qcowstream.Extents{}, qcowstream.Options{
		Mode: qcowstream.Full, VirtualSize: GiB, ClusterSize: 64 * KiB,
		Concurrency: 8, BufferSize: 16 * MiB, MemoryLimit: 64 * MiB,
	})
	var be *qcowstream.MemoryBudgetError
	if !errors.As(err, &be) || !errors.Is(err, qcowstream.ErrMemoryBudget) {
		t.Fatalf("got %v", err)
	}
	if be.Limit != 64*MiB || be.Required < 128*MiB {
		t.Fatalf("budget error %+v", be)
	}
}

func TestPlanCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := qcowstream.Plan(ctx, qcowstream.Extents{}, qcowstream.Options{Mode: qcowstream.Full, VirtualSize: GiB})
	if !errors.Is(err, qcowstream.ErrCanceled) {
		t.Fatalf("got %v", err)
	}
}

func FuzzPlanFullLayout(f *testing.F) {
	f.Add(uint64(1), uint8(9))
	f.Add(uint64(1<<20), uint8(16))
	f.Add(uint64(1<<40), uint8(21))
	f.Fuzz(func(t *testing.T, sectors uint64, bits uint8) {
		sectors = sectors%(1<<45) + 1
		cs := uint64(1) << (9 + bits%13)
		p, err := planFull(t, sectors*512, cs)
		if err != nil {
			if !errors.Is(err, qcowstream.ErrUnsupportedGranularity) {
				t.Fatalf("%d sectors, cluster %d: %v", sectors, cs, err)
			}
			return
		}
		checkLayoutInvariants(t, p.Info())
	})
}
