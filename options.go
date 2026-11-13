package qcowstream

import (
	"fmt"
	"math"
	"math/bits"
	"path"
	"strings"
)

// Mode selects the kind of image.
type Mode int

const (
	// Full images have no backing file and represent the whole disk.
	Full Mode = iota + 1
	// Incremental images hold the changed regions and inherit the rest from
	// a backing file.
	Incremental
)

func (m Mode) String() string {
	switch m {
	case Full:
		return "full"
	case Incremental:
		return "incremental"
	}
	return fmt.Sprintf("Mode(%d)", int(m))
}

const (
	// ClusterSizeAuto picks the largest cluster size up to 2 MiB that the
	// regions allow.
	ClusterSizeAuto = 0

	DefaultMemoryLimit = 128 << 20
	DefaultBufferSize  = 4 << 20
	MaxConcurrency     = 256

	autoClusterSize = MaxClusterSize
)

// Options configures a plan. Zero values select the documented defaults.
type Options struct {
	Mode        Mode
	VirtualSize uint64 // bytes, positive multiple of 512

	// ClusterSize is ClusterSizeAuto or a power of two between
	// MinClusterSize and MaxClusterSize.
	ClusterSize uint64

	// BackingFile is the relative name of the parent image, recorded in an
	// incremental image. It must be empty for a full image.
	BackingFile string
	// BackingFormat must be empty or "qcow2" for an incremental image, and
	// empty for a full image.
	BackingFormat string

	// MemoryLimit bounds the working memory the writer controls. Default
	// DefaultMemoryLimit.
	MemoryLimit uint64
	// BufferSize is the size of one source read, a multiple of the cluster
	// size. Default max(DefaultBufferSize, cluster size).
	BufferSize uint64
	// Concurrency is the number of source reads in flight. Default 1.
	Concurrency int
}

// Extents are the region sources of an image.
type Extents struct {
	// Changed lists the regions an incremental image stores. Required in
	// incremental mode, even when empty; must be nil in full mode.
	Changed ExtentSource
}

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidOptions, fmt.Sprintf(format, args...))
}

func validateOptions(opts *Options, ext Extents) error {
	if opts.VirtualSize == 0 || opts.VirtualSize%512 != 0 || opts.VirtualSize > math.MaxInt64 {
		return invalid("virtual size %d must be a positive multiple of 512 below 2^63", opts.VirtualSize)
	}
	if cs := opts.ClusterSize; cs != ClusterSizeAuto &&
		(bits.OnesCount64(cs) != 1 || cs < MinClusterSize || cs > MaxClusterSize) {
		return invalid("cluster size %d must be a power of two between %d and %d", cs, MinClusterSize, MaxClusterSize)
	}
	switch opts.Mode {
	case Full:
		if opts.BackingFile != "" || opts.BackingFormat != "" {
			return invalid("a full image has no backing file")
		}
		if ext.Changed != nil {
			return invalid("a full image takes no changed regions")
		}
	case Incremental:
		if ext.Changed == nil {
			return invalid("an incremental image needs changed regions, even an empty list")
		}
		if err := validateBackingFile(opts.BackingFile); err != nil {
			return err
		}
		if opts.BackingFormat != "" && opts.BackingFormat != "qcow2" {
			return invalid("backing format %q is not supported, only qcow2", opts.BackingFormat)
		}
	default:
		return invalid("unknown mode %v", opts.Mode)
	}
	if opts.Concurrency < 0 || opts.Concurrency > MaxConcurrency {
		return invalid("concurrency %d must be between 1 and %d", opts.Concurrency, MaxConcurrency)
	}
	return nil
}

func validateBackingFile(name string) error {
	switch {
	case name == "":
		return invalid("an incremental image needs a backing file")
	case len(name) > maxBackingName:
		return invalid("backing file name is %d bytes, QEMU allows %d", len(name), maxBackingName)
	case strings.IndexByte(name, 0) >= 0:
		return invalid("backing file name contains a NUL byte")
	case path.IsAbs(name):
		return invalid("backing file %q must be relative", name)
	case hasProtocol(name):
		return invalid("backing file %q would be read by QEMU as a protocol prefix; prefix it with ./", name)
	}
	return nil
}

// hasProtocol mirrors QEMU's path_has_protocol: a colon before the first
// slash makes QEMU treat the name as "protocol:target".
func hasProtocol(name string) bool {
	i := strings.IndexAny(name, ":/")
	return i >= 0 && name[i] == ':'
}
