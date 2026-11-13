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
