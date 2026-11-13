// Command qcowbench measures planning, memory and throughput on
// reproducible scenarios and prints a Markdown table. Each scenario runs in
// its own process so its peak RSS is its own.
//
//	go run ./internal/cmd/qcowbench > docs/measurements.md
package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/brmzkw/qcowstream"
	"github.com/brmzkw/qcowstream/internal/nbdtest"
	"github.com/brmzkw/qcowstream/internal/testutil"
	"github.com/brmzkw/qcowstream/internal/units"
	"github.com/brmzkw/qcowstream/internal/virtual"
	"github.com/brmzkw/qcowstream/nbdsource"
)

const (
	nbdSocketEnv  = "QCOWBENCH_NBD_SOCKET"
	qemuSocketEnv = "QCOWBENCH_QEMU_NBD_SOCKET"
)

type result struct {
	Name        string  `json:"name"`
	Walks       int     `json:"walks"`
	PlanTime    float64 `json:"plan_ms"`
	WriteTime   float64 `json:"write_s"`
	SourceBytes uint64  `json:"source_bytes"`
	ImageBytes  uint64  `json:"image_bytes"`
	Estimate    uint64  `json:"estimate"`
	Allocated   uint64  `json:"allocated"` // heap bytes allocated by Plan and Write
	RSS         uint64  `json:"rss"`       // filled by the parent
}

type scenario struct {
	name string
	run  func() result
}

var scenarios = []scenario{
	{"plan full 100 TiB, 64 KiB clusters", func() result {
		return planOnly(qcowstream.Extents{}, qcowstream.Options{Mode: qcowstream.Full, VirtualSize: 100 * units.TiB, ClusterSize: 64 * units.KiB})
	}},
	{"plan full 100 TiB, auto (2 MiB) clusters", func() result {
		return planOnly(qcowstream.Extents{}, qcowstream.Options{Mode: qcowstream.Full, VirtualSize: 100 * units.TiB})
	}},
	{"plan incremental 100 TiB, 1 Mi alternating 64 KiB extents", func() result {
		return planOnly(qcowstream.Extents{Changed: virtual.Strided{Stride: 128 * units.KiB, Length: 64 * units.KiB, Count: 1 << 20}},
			qcowstream.Options{Mode: qcowstream.Incremental, VirtualSize: 100 * units.TiB, ClusterSize: 64 * units.KiB, BackingFile: "base.qcow2"})
	}},
	{"plan incremental 100 TiB, 12.8 M alternating 4 MiB extents", func() result {
		return planOnly(qcowstream.Extents{Changed: virtual.Strided{Stride: 8 * units.MiB, Length: 4 * units.MiB, Count: 12_800_000}},
			qcowstream.Options{Mode: qcowstream.Incremental, VirtualSize: 100 * units.TiB, BackingFile: "base.qcow2"})
	}},
	{"write incremental 100 TiB, 100 x 4 MiB regions", func() result {
		return generate(virtual.Disk{Len: 100 * units.TiB}, qcowstream.Extents{Changed: virtual.Strided{Start: units.GiB, Stride: units.TiB, Length: 4 * units.MiB, Count: 100}},
			qcowstream.Options{Mode: qcowstream.Incremental, VirtualSize: 100 * units.TiB, BackingFile: "base.qcow2"})
	}},
	{"write full 16 GiB virtual disk, concurrency 1", func() result { return fullVirtual(1) }},
	{"write full 16 GiB virtual disk, concurrency 4", func() result { return fullVirtual(4) }},
	{"write full 2 GiB over NBD (Go test server), concurrency 1", func() result { return fullNBD(nbdSocketEnv, 1) }},
	{"write full 2 GiB over NBD (Go test server), concurrency 4", func() result { return fullNBD(nbdSocketEnv, 4) }},
	{"write full 2 GiB over NBD (qemu-nbd), concurrency 1", func() result { return fullNBD(qemuSocketEnv, 1) }},
	{"write full 2 GiB over NBD (qemu-nbd), concurrency 4", func() result { return fullNBD(qemuSocketEnv, 4) }},
	{"write full 2 GiB over NBD (qemu-nbd), concurrency 8", func() result { return fullNBD(qemuSocketEnv, 8) }},
}

func planOnly(ext qcowstream.Extents, opts qcowstream.Options) result {
	var p *qcowstream.ImagePlan
	var err error
	start := time.Now()
	alloc := testutil.Allocated(func() { p, err = qcowstream.Plan(context.Background(), ext, opts) })
	check(err)
	info := p.Info()
	return result{Walks: info.PlanWalks, PlanTime: ms(time.Since(start)), SourceBytes: info.SourceBytes,
		ImageBytes: info.ImageSize, Estimate: info.MemoryEstimate, Allocated: alloc}
}

func generate(src qcowstream.Reader, ext qcowstream.Extents, opts qcowstream.Options) result {
	var r result
	alloc := testutil.Allocated(func() {
		start := time.Now()
		p, err := qcowstream.Plan(context.Background(), ext, opts)
		check(err)
		r.PlanTime = ms(time.Since(start))
		info := p.Info()
		start = time.Now()
		st, err := qcowstream.Write(context.Background(), io.Discard, src, ext, p)
		check(err)
		r.WriteTime = time.Since(start).Seconds()
		r.Walks, r.SourceBytes, r.ImageBytes, r.Estimate = info.PlanWalks+st.Walks, st.SourceBytesRead, st.BytesWritten, info.MemoryEstimate
	})
	r.Allocated = alloc
	return r
}

func fullVirtual(n int) result {
	return generate(virtual.Disk{Len: 16 * units.GiB}, qcowstream.Extents{},
		qcowstream.Options{Mode: qcowstream.Full, VirtualSize: 16 * units.GiB, Concurrency: n})
}

// fullNBD reads from the test server the parent process runs, so the
// server's work and allocations are not measured.
func fullNBD(env string, n int) result {
	sock := os.Getenv(env)
	if sock == "" {
		return result{} // server unavailable
	}
	s, err := nbdsource.Open(context.Background(), nbdsource.Options{Socket: sock, Export: "scsi0", Connections: n})
	check(err)
	defer s.Close()
	return generate(s, qcowstream.Extents{}, qcowstream.Options{Mode: qcowstream.Full, VirtualSize: s.Size(), Concurrency: n})
}

// startQemuNBD serves a random raw file of size bytes with qemu-nbd, if it
// is installed. The file has no holes, so every byte crosses the socket.
func startQemuNBD(size uint64) (string, func()) {
	bin, err := exec.LookPath("qemu-nbd")
	if err != nil {
		return "", func() {}
	}
	dir, err := os.MkdirTemp("", "qcowbench")
	check(err)
	raw := filepath.Join(dir, "disk.raw")
	f, err := os.Create(raw)
	check(err)
	buf := make([]byte, 4*units.MiB)
	for range size / uint64(len(buf)) {
		_, _ = rand.Read(buf)
		_, err := f.Write(buf)
		check(err)
	}
	check(f.Close())
	sock := filepath.Join(dir, "s")
	cmd := exec.Command(bin, "--read-only", "--persistent", "--shared=16", "-f", "raw", "-x", "scsi0", "-k", sock, raw)
	check(cmd.Start())
	for range 100 {
		if _, err := os.Stat(sock); err == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	return sock, func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		os.RemoveAll(dir)
	}
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }

func check(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func main() {
	if len(os.Args) == 3 && os.Args[1] == "-scenario" {
		for _, s := range scenarios {
			if s.name == os.Args[2] {
				r := s.run()
				r.Name = s.name
				check(json.NewEncoder(os.Stdout).Encode(r))
				return
			}
		}
		check(fmt.Errorf("unknown scenario %q", os.Args[2]))
	}
	self, err := os.Executable()
	check(err)
	srv := &nbdtest.Server{Export: "scsi0", Size: 2 * units.GiB, MinBlock: 512, PrefBlock: 4096, MaxBlock: 32 << 20}
	sock, stop, err := srv.Listen()
	check(err)
	defer stop()
	qemuSock, stopQemu := startQemuNBD(2 * units.GiB)
	defer stopQemu()
	var results []result
	for _, s := range scenarios {
		fmt.Fprintf(os.Stderr, "qcowbench: %s\n", s.name)
		cmd := exec.Command(self, "-scenario", s.name)
		cmd.Env = append(os.Environ(), nbdSocketEnv+"="+sock, qemuSocketEnv+"="+qemuSock)
		cmd.Stderr = os.Stderr
		out, err := cmd.Output()
		check(err)
		var r result
		check(json.Unmarshal(out, &r))
		if r.SourceBytes == 0 && strings.Contains(s.name, "write") {
			fmt.Fprintf(os.Stderr, "qcowbench: skipped %s: server unavailable\n", s.name)
			continue
		}
		if ru, ok := cmd.ProcessState.SysUsage().(*syscall.Rusage); ok {
			r.RSS = uint64(ru.Maxrss)
			if runtime.GOOS == "linux" {
				r.RSS *= 1024 // units.KiB on Linux, bytes on macOS
			}
		}
		results = append(results, r)
	}
	report(results)
}

func report(rs []result) {
	fmt.Println("| Scenario | Walks | Plan | Write | Throughput | Estimate | Allocated | Peak RSS |")
	fmt.Println("|---|---:|---:|---:|---:|---:|---:|---:|")
	for _, r := range rs {
		write, tput := "-", "-"
		if r.WriteTime > 0 {
			write = fmt.Sprintf("%.2f s", r.WriteTime)
			tput = fmt.Sprintf("%.0f MiB/s", float64(r.SourceBytes)/float64(units.MiB)/r.WriteTime)
		}
		fmt.Printf("| %s | %d | %.1f ms | %s | %s | %s | %s | %s |\n", r.Name, r.Walks, r.PlanTime, write, tput,
			units.IEC(r.Estimate), units.IEC(r.Allocated), units.IEC(r.RSS))
	}
	fmt.Printf("\nMeasured on %s/%s, %d CPUs, %s.\n", runtime.GOOS, runtime.GOARCH, runtime.NumCPU(), strings.TrimPrefix(runtime.Version(), "go"))
}
