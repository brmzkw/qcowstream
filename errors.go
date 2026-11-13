package qcowstream

import (
	"context"
	"errors"
	"fmt"
)

var (
	// ErrInvalidRegion reports an extent that is empty, out of order,
	// overlapping, overflowing or outside the virtual disk.
	ErrInvalidRegion = errors.New("qcowstream: invalid region")

	// ErrOverflow reports a size or offset computation that does not fit.
	ErrOverflow = errors.New("qcowstream: arithmetic overflow")

	// ErrCanceled reports that the context was canceled. The context error is
	// wrapped as well, so errors.Is(err, context.Canceled) holds.
	ErrCanceled = errors.New("qcowstream: canceled")
)

func canceled(ctx context.Context) error {
	return fmt.Errorf("%w: %w", ErrCanceled, context.Cause(ctx))
}

// ctxErr wraps err as ErrCanceled when it comes from ctx, and returns it
// unchanged otherwise.
func ctxErr(ctx context.Context, err error) error {
	if err == nil || errors.Is(err, ErrCanceled) {
		return err
	}
	if ctx.Err() != nil && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) {
		return canceled(ctx)
	}
	return err
}

var (
	// ErrInvalidOptions reports options that are missing, inconsistent or
	// out of range.
	ErrInvalidOptions = errors.New("qcowstream: invalid options")

	// ErrUnsupportedGranularity reports a geometry QCOW2 with standard L2
	// tables cannot represent: regions finer than the cluster size, or a disk
	// too large for the cluster size.
	ErrUnsupportedGranularity = errors.New("qcowstream: unsupported granularity")

	// ErrMemoryBudget reports options needing more working memory than
	// allowed. The error is a *MemoryBudgetError.
	ErrMemoryBudget = errors.New("qcowstream: memory budget exceeded")
)

// GranularityError details an ErrUnsupportedGranularity.
type GranularityError struct {
	ClusterSize    uint64 // cluster size tried, 0 if none could be chosen
	Alignment      uint64 // alignment of the region boundaries, 0 if unknown
	MinClusterSize uint64 // smallest cluster size the disk size allows, 0 if unknown
	Reason         string
}

func (e *GranularityError) Error() string {
	return fmt.Sprintf("%v: %s", ErrUnsupportedGranularity, e.Reason)
}

func (e *GranularityError) Is(target error) bool { return target == ErrUnsupportedGranularity }

// MemoryBudgetError details an ErrMemoryBudget.
type MemoryBudgetError struct {
	Required uint64 // estimated minimum, in bytes
	Limit    uint64
}

func (e *MemoryBudgetError) Error() string {
	return fmt.Sprintf("%v: options need about %d bytes, the limit is %d", ErrMemoryBudget, e.Required, e.Limit)
}

func (e *MemoryBudgetError) Is(target error) bool { return target == ErrMemoryBudget }
