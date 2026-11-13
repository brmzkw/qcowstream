package qcowstream_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/brmzkw/qcowstream"
)

func planInc(size, cs uint64, ext qcowstream.ExtentSource) (*qcowstream.ImagePlan, error) {
	return qcowstream.Plan(context.Background(), qcowstream.Extents{Changed: ext}, qcowstream.Options{
		Mode: qcowstream.Incremental, VirtualSize: size, ClusterSize: cs, BackingFile: "base.qcow2",
	})
}

func TestPlanIncrementalCountsCoveredClusters(t *testing.T) {
	cases := []struct {
		name         string
		size, cs     uint64
		ext          qcowstream.SliceExtents
		data, tables uint64
	}{
		{"empty", 1 * GiB, 64 * KiB, nil, 0, 0},
		{"one cluster", 1 * GiB, 64 * KiB, qcowstream.SliceExtents{{64 * KiB, 64 * KiB}}, 1, 1},
		{"adjacent halves merge", 1 * GiB, 64 * KiB, qcowstream.SliceExtents{{0, 32 * KiB}, {32 * KiB, 32 * KiB}}, 1, 1},
		{"unaligned end at disk end", 1*MiB + 512, 64 * KiB, qcowstream.SliceExtents{{1 * MiB, 512}}, 1, 1},
		// 512-byte clusters: 64 entries per L2 table, 32 KiB each.
		{"across an L2 boundary", 1 * MiB, 512, qcowstream.SliceExtents{{32*KiB - 512, 1 * KiB}}, 2, 2},
		{"disjoint tables", 1 * MiB, 512, qcowstream.SliceExtents{{0, 512}, {512 * KiB, 512}, {1*MiB - 512, 512}}, 3, 3},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p, err := planInc(c.size, c.cs, c.ext)
			if err != nil {
				t.Fatal(err)
			}
			info := p.Info()
			checkLayoutInvariants(t, info)
			if info.DataClusters != c.data || info.L2Tables != c.tables {
				t.Fatalf("%d data clusters, %d L2 tables; want %d, %d", info.DataClusters, info.L2Tables, c.data, c.tables)
			}
			var bytes uint64
			for _, e := range c.ext {
				bytes += e.Length
			}
			if info.SourceBytes != bytes {
				t.Fatalf("source bytes %d, want %d", info.SourceBytes, bytes)
			}
			if info.BackingFile != "base.qcow2" || info.BackingFormat != "qcow2" || info.PlanWalks != 2 || info.WriteWalks != 2 {
				t.Fatalf("%+v", info)
			}
		})
	}
}

func TestPlanIncrementalRefusesPartialClusters(t *testing.T) {
	cases := map[string]qcowstream.SliceExtents{
		"half cluster":         {{0, 32 * KiB}},
		"unaligned start":      {{32 * KiB, 64 * KiB}},
		"gap inside a cluster": {{0, 16 * KiB}, {32 * KiB, 32 * KiB}},
		"end before disk end":  {{0, 64*KiB + 512}},
	}
	for name, ext := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := planInc(1*GiB, 64*KiB, ext)
			var ge *qcowstream.GranularityError
			if !errors.As(err, &ge) {
				t.Fatalf("got %v", err)
			}
			if ge.Alignment == 0 || ge.Alignment >= 64*KiB || ge.ClusterSize != 64*KiB {
				t.Fatalf("%+v", ge)
			}
		})
	}
}

func TestPlanIncrementalAutoClusterSize(t *testing.T) {
	cases := []struct {
		name string
		ext  qcowstream.SliceExtents
		want uint64
	}{
		{"empty", nil, 2 * MiB},
		{"4 MiB bitmap (Proxmox)", qcowstream.SliceExtents{{4 * MiB, 8 * MiB}, {64 * MiB, 4 * MiB}}, 2 * MiB},
		{"64 KiB bitmap", qcowstream.SliceExtents{{64 * KiB, 64 * KiB}, {1 * MiB, 4 * MiB}}, 64 * KiB},
		{"512-byte sectors", qcowstream.SliceExtents{{512, 512}}, 512},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p, err := planInc(1*GiB, qcowstream.ClusterSizeAuto, c.ext)
			if err != nil {
				t.Fatal(err)
			}
			if cs := p.Info().ClusterSize; cs != c.want {
				t.Fatalf("auto picked %d, want %d", cs, c.want)
			}
		})
	}
	_, err := planInc(1*GiB, qcowstream.ClusterSizeAuto, qcowstream.SliceExtents{{256, 256}})
	if !errors.Is(err, qcowstream.ErrUnsupportedGranularity) {
		t.Fatalf("256-byte regions: %v", err)
	}
}

// sparseExtents yields count extents of length bytes, one every stride
// bytes, without storing them.
type sparseExtents struct{ start, stride, length, count uint64 }

func (s sparseExtents) Walk(ctx context.Context, yield func(qcowstream.Extent) error) error {
	for i := range s.count {
		if err := yield(qcowstream.Extent{Offset: s.start + i*s.stride, Length: s.length}); err != nil {
			return err
		}
	}
	return nil
}

func TestPlanIncremental100TiB(t *testing.T) {
	// One 64 KiB region every 64 GiB: 1600 L2 tables at 64 KiB clusters.
	ext := sparseExtents{start: 0, stride: 64 * GiB, length: 64 * KiB, count: 1600}
	p, err := planInc(100*TiB, 64*KiB, ext)
	if err != nil {
		t.Fatal(err)
	}
	info := p.Info()
	checkLayoutInvariants(t, info)
	if info.DataClusters != 1600 || info.L2Tables != 1600 || info.L1Entries != 204800 {
		t.Fatalf("%+v", info)
	}
	if info.MemoryEstimate > qcowstream.DefaultMemoryLimit {
		t.Fatalf("memory estimate %d", info.MemoryEstimate)
	}

	// 4 KiB tracking cannot be represented on 100 TiB with standard L2.
	ext4k := sparseExtents{start: 4 * KiB, stride: 1 * GiB, length: 4 * KiB, count: 100}
	for _, cs := range []uint64{qcowstream.ClusterSizeAuto, 4 * KiB, 64 * KiB} {
		_, err := planInc(100*TiB, cs, ext4k)
		var ge *qcowstream.GranularityError
		if !errors.As(err, &ge) {
			t.Fatalf("cluster size %d: got %v", cs, err)
		}
		if ge.MinClusterSize != 16*KiB || ge.Alignment != 4*KiB {
			t.Fatalf("cluster size %d: %+v", cs, ge)
		}
		if cs == qcowstream.ClusterSizeAuto && !strings.Contains(err.Error(), "Extended L2") {
			t.Fatalf("explanation does not mention Extended L2: %v", err)
		}
	}
}

func TestPlanIncrementalL1Limit(t *testing.T) {
	// 512-byte clusters map at most 4 Mi L1 entries * 64 clusters = 128 GiB.
	empty := qcowstream.SliceExtents{}
	if _, err := planInc(128*GiB, 512, empty); err != nil {
		t.Fatalf("128 GiB: %v", err)
	}
	if _, err := planInc(128*GiB+512, 512, empty); !errors.Is(err, qcowstream.ErrUnsupportedGranularity) {
		t.Fatalf("128 GiB + 512: %v", err)
	}
}

// oneShot yields its extents on the first walk only.
type oneShot struct {
	ext  qcowstream.SliceExtents
	used bool
}

func (o *oneShot) Walk(ctx context.Context, yield func(qcowstream.Extent) error) error {
	if o.used {
		return nil
	}
	o.used = true
	return o.ext.Walk(ctx, yield)
}

// drifting yields a different extent on every walk.
type drifting struct{ n uint64 }

func (d *drifting) Walk(ctx context.Context, yield func(qcowstream.Extent) error) error {
	d.n++
	return yield(qcowstream.Extent{Offset: d.n * 64 * KiB, Length: 64 * KiB})
}

func TestPlanIncrementalRejectsUnstableSources(t *testing.T) {
	for name, src := range map[string]qcowstream.ExtentSource{
		"one shot": &oneShot{ext: qcowstream.SliceExtents{{0, 64 * KiB}}},
		"drifting": &drifting{},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := planInc(1*GiB, 64*KiB, src)
			if !errors.Is(err, qcowstream.ErrUnstableSource) {
				t.Fatalf("got %v", err)
			}
		})
	}
}

func TestPlanIncrementalBackingValidation(t *testing.T) {
	cases := map[string]qcowstream.Options{
		"absolute":       {BackingFile: "/srv/base.qcow2"},
		"nul":            {BackingFile: "base\x00.qcow2"},
		"too long":       {BackingFile: strings.Repeat("a", 1024)},
		"protocol":       {BackingFile: "nbd:base.qcow2"},
		"json":           {BackingFile: "json:{}"},
		"raw format":     {BackingFile: "base.qcow2", BackingFormat: "raw"},
		"header too big": {BackingFile: strings.Repeat("a", 500), ClusterSize: 512},
	}
	for name, o := range cases {
		t.Run(name, func(t *testing.T) {
			o.Mode, o.VirtualSize = qcowstream.Incremental, 1*GiB
			_, err := qcowstream.Plan(context.Background(), qcowstream.Extents{Changed: qcowstream.SliceExtents{}}, o)
			if !errors.Is(err, qcowstream.ErrInvalidOptions) {
				t.Fatalf("got %v", err)
			}
		})
	}
	for _, name := range []string{"base.qcow2", "./nbd:base.qcow2", "chain/inc-1.qcow2", "../base.qcow2"} {
		if _, err := qcowstream.Plan(context.Background(), qcowstream.Extents{Changed: qcowstream.SliceExtents{}},
			qcowstream.Options{Mode: qcowstream.Incremental, VirtualSize: GiB, BackingFile: name, BackingFormat: "qcow2"}); err != nil {
			t.Fatalf("%q: %v", name, err)
		}
	}
}
