package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/brmzkw/qcowstream"
	"github.com/brmzkw/qcowstream/nbdsource"
)

const usage = `usage:
  qcowstream full        SOURCE [OPTIONS] --output PATH|-
  qcowstream incremental SOURCE [OPTIONS] --backing-file NAME --output PATH|-
  qcowstream plan full|incremental SOURCE [OPTIONS]

SOURCE is --source-file PATH (with --regions FILE for incremental), or
--nbd-socket PATH --export NAME (with --bitmap NAME for incremental).

The image goes to --output; "-" is stdout, which then carries only image
bytes. Diagnostics go to stderr. plan prints the layout as JSON and reads no
disk data.
`

const (
	exitOK    = 0
	exitError = 1
	exitUsage = 2
)

// file is what the CLI needs from a raw source file.
type file interface {
	io.ReaderAt
	io.Closer
	Size() (uint64, error)
	Holes(size uint64) qcowstream.ExtentSource
}

type cli struct {
	stdout, stderr io.Writer
	openFile       func(path string) (file, error)
}

type osFile struct{ *os.File }

func (f osFile) Size() (uint64, error) {
	// Seek works for block devices, where Stat reports 0.
	n, err := f.Seek(0, io.SeekEnd)
	return uint64(n), err
}

func (f osFile) Holes(size uint64) qcowstream.ExtentSource {
	return qcowstream.FileHoles{F: f.File, Size: size}
}

func openOSFile(path string) (file, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	return osFile{f}, nil
}

type usageError struct{ msg string }

func (e usageError) Error() string { return e.msg }

func usagef(format string, args ...any) error { return usageError{fmt.Sprintf(format, args...)} }

type config struct {
	mode    qcowstream.Mode
	planned bool // plan subcommand

	sourceFile, regions        string
	nbdSocket, export, bitmap  string
	backingFile, backingFormat string
	clusterSize                string
	memoryLimit, bufferSize    string
	concurrency                int
	noZeroDetection            bool
	output                     string
	force                      bool
}

func (c *cli) run(ctx context.Context, args []string) int {
	err := c.dispatch(ctx, args)
	var ue usageError
	switch {
	case err == nil:
		return exitOK
	case errors.As(err, &ue):
		fmt.Fprintf(c.stderr, "qcowstream: %v\n\n%s", err, usage)
		return exitUsage
	case errors.Is(err, flag.ErrHelp):
		fmt.Fprint(c.stderr, usage)
		return exitUsage
	default:
		// Library errors already carry the prefix.
		fmt.Fprintf(c.stderr, "qcowstream: %s\n", strings.TrimPrefix(err.Error(), "qcowstream: "))
		return exitError
	}
}

func (c *cli) dispatch(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return usagef("missing command")
	}
	var cfg config
	switch args[0] {
	case "full", "incremental":
	case "plan":
		cfg.planned = true
		args = args[1:]
		if len(args) == 0 || (args[0] != "full" && args[0] != "incremental") {
			return usagef("plan needs full or incremental")
		}
	case "-h", "-help", "--help", "help":
		return flag.ErrHelp
	default:
		return usagef("unknown command %q", args[0])
	}
	cfg.mode = qcowstream.Full
	if args[0] == "incremental" {
		cfg.mode = qcowstream.Incremental
	}

	fs := flag.NewFlagSet(args[0], flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&cfg.sourceFile, "source-file", "", "raw disk image or block device")
	fs.StringVar(&cfg.regions, "regions", "", "JSON file of changed regions, for an incremental from --source-file")
	fs.StringVar(&cfg.nbdSocket, "nbd-socket", "", "Unix socket of the NBD server")
	fs.StringVar(&cfg.export, "export", "", "NBD export name")
	fs.StringVar(&cfg.bitmap, "bitmap", "", "QEMU dirty bitmap name, for an incremental over NBD")
	fs.StringVar(&cfg.backingFile, "backing-file", "", "relative name of the parent image")
	fs.StringVar(&cfg.backingFormat, "backing-format", "qcow2", "format of the parent image")
	fs.StringVar(&cfg.clusterSize, "cluster-size", "auto", "cluster size, or auto")
	fs.StringVar(&cfg.memoryLimit, "memory-limit", "128MiB", "working memory budget")
	fs.StringVar(&cfg.bufferSize, "buffer-size", "", "size of one source read (default max(4MiB, cluster size))")
	fs.IntVar(&cfg.concurrency, "concurrency", 1, "source reads in flight")
	fs.BoolVar(&cfg.noZeroDetection, "no-zero-detection", false, "read every cluster, even those the source reports as zero")
	if !cfg.planned {
		fs.StringVar(&cfg.output, "output", "", "destination file, or - for stdout")
		fs.BoolVar(&cfg.force, "force", false, "overwrite an existing output file")
	}
	if err := fs.Parse(args[1:]); err != nil {
		return usagef("%v", err)
	}
	if fs.NArg() > 0 {
		return usagef("unexpected argument %q", fs.Arg(0))
	}
	if err := cfg.check(); err != nil {
		return err
	}
	return c.generate(ctx, &cfg)
}

// check rejects missing and incompatible flags.
func (cfg *config) check() error {
	file, nbd := cfg.sourceFile != "", cfg.nbdSocket != "" || cfg.export != ""
	switch {
	case file == nbd:
		return usagef("give either --source-file or --nbd-socket with --export")
	case nbd && (cfg.nbdSocket == "" || cfg.export == ""):
		return usagef("--nbd-socket and --export go together")
	case file && cfg.bitmap != "":
		return usagef("--bitmap needs an NBD source; use --regions with --source-file")
	case nbd && cfg.regions != "":
		return usagef("--regions is for --source-file; use --bitmap over NBD")
	}
	if cfg.mode == qcowstream.Full {
		switch {
		case cfg.regions != "" || cfg.bitmap != "":
			return usagef("a full image takes no --regions or --bitmap")
		case cfg.backingFile != "":
			return usagef("a full image has no --backing-file")
		}
	} else {
		switch {
		case cfg.backingFile == "":
			return usagef("an incremental image needs --backing-file")
		case file && cfg.regions == "":
			return usagef("an incremental image from a file needs --regions")
		case nbd && cfg.bitmap == "":
			return usagef("an incremental image over NBD needs --bitmap")
		}
	}
	if !cfg.planned && cfg.output == "" {
		return usagef("missing --output (use - for stdout)")
	}
	if cfg.concurrency < 1 {
		return usagef("--concurrency must be at least 1")
	}
	return nil
}

func (cfg *config) options() (qcowstream.Options, error) {
	opts := qcowstream.Options{Mode: cfg.mode, Concurrency: cfg.concurrency}
	var err error
	if cfg.clusterSize != "auto" {
		if opts.ClusterSize, err = parseSize(cfg.clusterSize); err != nil {
			return opts, usagef("--cluster-size: %v", err)
		}
		if opts.ClusterSize == qcowstream.ClusterSizeAuto {
			return opts, usagef("--cluster-size: use auto instead of 0")
		}
	}
	if opts.MemoryLimit, err = parseSize(cfg.memoryLimit); err != nil {
		return opts, usagef("--memory-limit: %v", err)
	}
	if cfg.bufferSize != "" {
		if opts.BufferSize, err = parseSize(cfg.bufferSize); err != nil {
			return opts, usagef("--buffer-size: %v", err)
		}
	}
	if cfg.mode == qcowstream.Incremental {
		opts.BackingFile, opts.BackingFormat = cfg.backingFile, cfg.backingFormat
	}
	return opts, nil
}

// source is an opened disk with its region sources.
type source struct {
	reader qcowstream.Reader
	ext    qcowstream.Extents
	size   uint64
	io.Closer
}

// open opens the configured source.
func (c *cli) open(ctx context.Context, cfg *config) (*source, error) {
	if cfg.nbdSocket != "" {
		s, err := nbdsource.Open(ctx, nbdsource.Options{
			Socket: cfg.nbdSocket, Export: cfg.export, Connections: cfg.concurrency,
			Bitmap: cfg.bitmap, DetectZeros: !cfg.noZeroDetection,
		})
		if err != nil {
			return nil, err
		}
		return &source{s, qcowstream.Extents{Changed: s.Changed(), Zeros: s.Zeros()}, s.Size(), s}, nil
	}
	f, err := c.openFile(cfg.sourceFile)
	if err != nil {
		return nil, err
	}
	src, err := fileSource(f, cfg)
	if err != nil {
		f.Close()
	}
	return src, err
}

func fileSource(f file, cfg *config) (*source, error) {
	size, err := f.Size()
	if err != nil {
		return nil, err
	}
	src := &source{reader: qcowstream.NewReaderAtSource(f, size), size: size, Closer: f}
	if cfg.regions != "" {
		r, err := openRegions(cfg.regions)
		if err != nil {
			return nil, err
		}
		if r.virtualSize != size {
			return nil, fmt.Errorf("regions file is for a %d-byte disk, %s is %d bytes", r.virtualSize, cfg.sourceFile, size)
		}
		src.ext.Changed = r
	}
	if !cfg.noZeroDetection {
		src.ext.Zeros = f.Holes(size)
	}
	return src, nil
}

func (c *cli) generate(ctx context.Context, cfg *config) error {
	opts, err := cfg.options()
	if err != nil {
		return err
	}
	src, err := c.open(ctx, cfg)
	if err != nil {
		return err
	}
	defer src.Close()
	opts.VirtualSize = src.size

	start := time.Now()
	plan, err := qcowstream.Plan(ctx, src.ext, opts)
	if err != nil {
		return err
	}
	info := plan.Info()
	if cfg.planned {
		return c.printPlan(info, time.Since(start))
	}
	fmt.Fprintf(c.stderr, "qcowstream: %s image of %s (%d bytes), %s clusters, %d data clusters to read, output %s (%d bytes)\n",
		info.Mode, formatIEC(info.VirtualSize), info.VirtualSize, formatIEC(info.ClusterSize), info.DataClusters,
		formatIEC(info.ImageSize), info.ImageSize)

	out, finish, err := openOutput(cfg.output, cfg.force, c.stdout)
	if err != nil {
		return err
	}
	st, err := qcowstream.Write(ctx, out, src.reader, src.ext, plan)
	if err = finish(err); err != nil {
		return err
	}
	elapsed := time.Since(start)
	fmt.Fprintf(c.stderr, "qcowstream: wrote %d bytes, read %d source bytes in %s (%s/s)\n",
		st.BytesWritten, st.SourceBytesRead, elapsed.Round(time.Millisecond),
		formatIEC(uint64(float64(st.SourceBytesRead)/max(elapsed.Seconds(), 1e-9))))
	return nil
}

// openOutput opens the destination. finish closes it and, on error, removes
// a file it created so a partial image is never left behind.
func openOutput(path string, force bool, stdout io.Writer) (io.Writer, func(error) error, error) {
	if path == "-" {
		return stdout, func(err error) error { return err }, nil
	}
	flags := os.O_WRONLY | os.O_CREATE | os.O_EXCL
	if force {
		flags = os.O_WRONLY | os.O_CREATE | os.O_TRUNC
	}
	f, err := os.OpenFile(path, flags, 0o644)
	if errors.Is(err, os.ErrExist) {
		return nil, nil, fmt.Errorf("%s exists; use --force to overwrite it", path)
	}
	if err != nil {
		return nil, nil, err
	}
	return f, func(err error) error {
		if err == nil {
			err = f.Sync()
		}
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			os.Remove(path)
		}
		return err
	}, nil
}

type planOutput struct {
	qcowstream.PlanInfo
	SizeUnit string            `json:"size_unit"`
	Human    map[string]string `json:"human"`
	PlanTime string            `json:"plan_time"`
}

func (c *cli) printPlan(info qcowstream.PlanInfo, took time.Duration) error {
	out := planOutput{
		PlanInfo: info,
		SizeUnit: "bytes",
		Human: map[string]string{
			"virtual_size":    formatIEC(info.VirtualSize),
			"cluster_size":    formatIEC(info.ClusterSize),
			"image_size":      formatIEC(info.ImageSize),
			"source_bytes":    formatIEC(info.SourceBytes),
			"memory_estimate": formatIEC(info.MemoryEstimate),
			"memory_limit":    formatIEC(info.MemoryLimit),
		},
		PlanTime: took.Round(time.Microsecond).String(),
	}
	enc := json.NewEncoder(c.stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}
