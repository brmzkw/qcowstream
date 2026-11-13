package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/brmzkw/qcowstream"
	"github.com/brmzkw/qcowstream/internal/qcow2test"
	"github.com/brmzkw/qcowstream/internal/testutil"
)

const (
	KiB = uint64(1) << 10
	MiB = KiB << 10
)

// fakeFile serves a disk from memory and counts reads.
type fakeFile struct {
	data   []byte
	holes  qcowstream.SliceExtents
	reads  atomic.Int64
	failAt int64 // fail the n-th read, if set
}

func (f *fakeFile) ReadAt(p []byte, off int64) (int, error) {
	if n := f.reads.Add(1); f.failAt != 0 && n >= f.failAt {
		return 0, errors.New("disk failure")
	}
	return copy(p, f.data[off:]), nil
}
func (f *fakeFile) Close() error                         { return nil }
func (f *fakeFile) Size() (uint64, error)                { return uint64(len(f.data)), nil }
func (f *fakeFile) Holes(uint64) qcowstream.ExtentSource { return f.holes }

type harness struct {
	t              *testing.T
	files          map[string]*fakeFile
	stdout, stderr bytes.Buffer
}

func newHarness(t *testing.T) *harness {
	return &harness{t: t, files: map[string]*fakeFile{}}
}

func (h *harness) run(args ...string) int {
	h.stdout.Reset()
	h.stderr.Reset()
	c := &cli{stdout: &h.stdout, stderr: &h.stderr, openFile: func(path string) (file, error) {
		f, ok := h.files[path]
		if !ok {
			return nil, os.ErrNotExist
		}
		return f, nil
	}}
	return c.run(context.Background(), args)
}

func writeRegions(t *testing.T, size uint64, ext ...qcowstream.Extent) string {
	t.Helper()
	type e struct {
		Offset uint64 `json:"offset"`
		Length uint64 `json:"length"`
	}
	doc := struct {
		VirtualSize uint64 `json:"virtual_size"`
		Extents     []e    `json:"extents"`
	}{VirtualSize: size, Extents: []e{}}
	for _, x := range ext {
		doc.Extents = append(doc.Extents, e{x.Offset, x.Length})
	}
	b, _ := json.Marshal(doc)
	path := filepath.Join(t.TempDir(), "regions.json")
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestFullToStdout(t *testing.T) {
	h := newHarness(t)
	disk := testutil.RandomDisk(4*MiB+512, 1)
	h.files["disk.raw"] = &fakeFile{data: disk}
	if rc := h.run("full", "--source-file", "disk.raw", "--cluster-size", "64KiB", "--output", "-"); rc != 0 {
		t.Fatalf("exit %d: %s", rc, h.stderr.String())
	}
	im, err := qcow2test.Check(h.stdout.Bytes())
	if err != nil {
		t.Fatalf("stdout is not just the image: %v", err)
	}
	if !bytes.Equal(im.Materialize(nil), disk) || im.ClusterSize != 64*KiB {
		t.Fatal("content differs")
	}
	if !strings.Contains(h.stderr.String(), "wrote") {
		t.Fatalf("no summary on stderr: %q", h.stderr.String())
	}
}

func TestIncrementalFromRegionsFile(t *testing.T) {
	h := newHarness(t)
	parent := testutil.RandomDisk(4*MiB, 2)
	child := bytes.Clone(parent)
	copy(child[1*MiB:2*MiB], testutil.RandomDisk(1*MiB, 3))
	clear(child[3*MiB : 3*MiB+64*KiB])
	h.files["disk.raw"] = &fakeFile{data: child, holes: qcowstream.SliceExtents{{Offset: 3 * MiB, Length: 64 * KiB}}}
	regions := writeRegions(t, 4*MiB, qcowstream.Extent{Offset: 1 * MiB, Length: 1 * MiB}, qcowstream.Extent{Offset: 3 * MiB, Length: 64 * KiB})
	rc := h.run("incremental", "--source-file", "disk.raw", "--regions", regions,
		"--backing-file", "base.qcow2", "--cluster-size", "64KiB", "--output", "-")
	if rc != 0 {
		t.Fatalf("exit %d: %s", rc, h.stderr.String())
	}
	im, err := qcow2test.Check(h.stdout.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if im.BackingFile != "base.qcow2" || !bytes.Equal(im.Materialize(parent), child) {
		t.Fatal("parent + increment differs")
	}
	if im.Kind(48) != qcow2test.Zero {
		t.Fatalf("hole in a changed region is %v, want zero-flagged", im.Kind(48))
	}

	// Without zero detection the zero cluster is stored as data.
	if rc := h.run("incremental", "--source-file", "disk.raw", "--regions", regions, "--no-zero-detection",
		"--backing-file", "base.qcow2", "--cluster-size", "64KiB", "--output", "-"); rc != 0 {
		t.Fatalf("exit %d: %s", rc, h.stderr.String())
	}
	if im, err = qcow2test.Check(h.stdout.Bytes()); err != nil || im.Kind(48) != qcow2test.Data {
		t.Fatalf("%v, cluster 48 is %v", err, im.Kind(48))
	}
}

func TestPlanReadsNoData(t *testing.T) {
	h := newHarness(t)
	f := &fakeFile{data: make([]byte, 64*MiB)}
	h.files["disk.raw"] = f
	regions := writeRegions(t, 64*MiB, qcowstream.Extent{Offset: 4 * MiB, Length: 4 * MiB})
	for _, args := range [][]string{
		{"plan", "full", "--source-file", "disk.raw"},
		{"plan", "incremental", "--source-file", "disk.raw", "--regions", regions, "--backing-file", "base.qcow2"},
	} {
		if rc := h.run(args...); rc != 0 {
			t.Fatalf("%v: exit %d: %s", args, rc, h.stderr.String())
		}
		var out map[string]any
		if err := json.Unmarshal(h.stdout.Bytes(), &out); err != nil {
			t.Fatalf("%v: stdout is not JSON: %v", args, err)
		}
		human, _ := out["human"].(map[string]any)
		if out["size_unit"] != "bytes" || !strings.HasSuffix(fmt.Sprint(human["virtual_size"]), "MiB") {
			t.Fatalf("%v: units not explicit: %s", args, h.stdout.String())
		}
		for _, k := range []string{"image_size", "data_clusters", "cluster_size", "memory_estimate", "l1_entries"} {
			if _, ok := out[k]; !ok {
				t.Fatalf("%v: missing %s", args, k)
			}
		}
	}
	if n := f.reads.Load(); n != 0 {
		t.Fatalf("plan read the disk %d times", n)
	}
}

func TestRejectsBadFlags(t *testing.T) {
	h := newHarness(t)
	h.files["disk.raw"] = &fakeFile{data: make([]byte, MiB)}
	cases := [][]string{
		{},
		{"backup"},
		{"plan"},
		{"plan", "both", "--source-file", "disk.raw"},
		{"full", "--output", "-"},
		{"full", "--source-file", "disk.raw", "--nbd-socket", "/s", "--export", "e", "--output", "-"},
		{"full", "--nbd-socket", "/s", "--output", "-"},
		{"full", "--source-file", "disk.raw"},
		{"full", "--source-file", "disk.raw", "--backing-file", "b.qcow2", "--output", "-"},
		{"full", "--source-file", "disk.raw", "--regions", "r.json", "--output", "-"},
		{"full", "--source-file", "disk.raw", "--bitmap", "b", "--output", "-"},
		{"incremental", "--source-file", "disk.raw", "--regions", "r.json", "--output", "-"},
		{"incremental", "--source-file", "disk.raw", "--backing-file", "b.qcow2", "--output", "-"},
		{"incremental", "--nbd-socket", "/s", "--export", "e", "--backing-file", "b.qcow2", "--output", "-"},
		{"incremental", "--nbd-socket", "/s", "--export", "e", "--regions", "r.json", "--bitmap", "b", "--backing-file", "b.qcow2", "--output", "-"},
		{"full", "--source-file", "disk.raw", "--cluster-size", "64K", "--output", "-"},
		{"full", "--source-file", "disk.raw", "--cluster-size", "0", "--output", "-"},
		{"full", "--source-file", "disk.raw", "--concurrency", "0", "--output", "-"},
		{"full", "--source-file", "disk.raw", "--output", "-", "extra"},
		{"plan", "full", "--source-file", "disk.raw", "--output", "-"},
	}
	for _, args := range cases {
		if rc := h.run(args...); rc != exitUsage {
			t.Errorf("%v: exit %d, want %d: %s", args, rc, exitUsage, h.stderr.String())
		}
		if h.stdout.Len() != 0 {
			t.Errorf("%v: wrote to stdout", args)
		}
	}
}

func TestOutputFile(t *testing.T) {
	h := newHarness(t)
	disk := testutil.RandomDisk(1*MiB, 4)
	h.files["disk.raw"] = &fakeFile{data: disk}
	out := filepath.Join(t.TempDir(), "base.qcow2")
	if err := os.WriteFile(out, []byte("precious"), 0o644); err != nil {
		t.Fatal(err)
	}
	if rc := h.run("full", "--source-file", "disk.raw", "--output", out); rc != exitError {
		t.Fatalf("existing output: exit %d", rc)
	}
	if b, _ := os.ReadFile(out); string(b) != "precious" {
		t.Fatal("existing output was modified")
	}
	if rc := h.run("full", "--source-file", "disk.raw", "--output", out, "--force"); rc != 0 {
		t.Fatalf("--force: exit %d: %s", rc, h.stderr.String())
	}
	b, _ := os.ReadFile(out)
	if im, err := qcow2test.Check(b); err != nil || !bytes.Equal(im.Materialize(nil), disk) {
		t.Fatalf("output image: %v", err)
	}
}

func TestFailureRemovesPartialOutput(t *testing.T) {
	h := newHarness(t)
	h.files["disk.raw"] = &fakeFile{data: testutil.RandomDisk(8*MiB, 5), failAt: 3}
	out := filepath.Join(t.TempDir(), "base.qcow2")
	if rc := h.run("full", "--source-file", "disk.raw", "--cluster-size", "64KiB", "--buffer-size", "1MiB", "--output", out); rc != exitError {
		t.Fatalf("exit %d", rc)
	}
	if _, err := os.Stat(out); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("partial output left behind: %v", err)
	}
	if !strings.Contains(h.stderr.String(), "disk failure") {
		t.Fatalf("cause not reported: %s", h.stderr.String())
	}
}

func TestRegionsFileErrors(t *testing.T) {
	h := newHarness(t)
	h.files["disk.raw"] = &fakeFile{data: make([]byte, 4*MiB)}
	dir := t.TempDir()
	write := func(name, content string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	cases := map[string]string{
		"wrong size":     write("size.json", `{"virtual_size": 8388608, "extents": []}`),
		"not json":       write("bad.json", `[1, 2`),
		"missing length": write("len.json", `{"virtual_size": 4194304, "extents": [{"offset": 0}]}`),
		"unknown key":    write("key.json", `{"virtual_size": 4194304, "extents": [], "bitmap": 1}`),
		"out of order":   write("order.json", `{"virtual_size": 4194304, "extents": [{"offset": 65536, "length": 65536}, {"offset": 0, "length": 65536}]}`),
		"too large":      write("big.json", `{"virtual_size": 4194304, "extents": []}`+strings.Repeat(" ", maxRegionsFile)),
	}
	for name, path := range cases {
		rc := h.run("incremental", "--source-file", "disk.raw", "--regions", path, "--backing-file", "b.qcow2", "--output", "-")
		if rc != exitError || h.stdout.Len() != 0 {
			t.Errorf("%s: exit %d, %d bytes on stdout", name, rc, h.stdout.Len())
		}
	}
}
