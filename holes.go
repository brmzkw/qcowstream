package qcowstream

import (
	"context"
	"errors"
	"fmt"
	"os"
	"syscall"
)

// FileHoles is an ExtentSource listing the holes of a sparse file with
// SEEK_DATA and SEEK_HOLE. Holes read as zeros, so they can be passed as
// Extents.Zeros. Only file metadata is consulted, never content.
//
// On a file system or platform without SEEK_DATA, the file has no holes.
type FileHoles struct {
	F    *os.File
	Size uint64
}

func (h FileHoles) Walk(ctx context.Context, yield func(Extent) error) error {
	if seekData < 0 {
		return nil
	}
	var off uint64
	for off < h.Size {
		if err := ctx.Err(); err != nil {
			return err
		}
		data, err := h.F.Seek(int64(off), seekData)
		if errors.Is(err, syscall.ENXIO) {
			return yield(Extent{Offset: off, Length: h.Size - off}) // a hole up to the end
		}
		if errors.Is(err, syscall.EINVAL) {
			return nil // no support on this file system
		}
		if err != nil {
			return fmt.Errorf("SEEK_DATA at %d: %w", off, err)
		}
		if data < 0 {
			return fmt.Errorf("SEEK_DATA at %d returned %d", off, data)
		}
		if d := min(uint64(data), h.Size); d > off {
			if err := yield(Extent{Offset: off, Length: d - off}); err != nil {
				return err
			}
		}
		if uint64(data) >= h.Size {
			return nil
		}
		hole, err := h.F.Seek(data, seekHole)
		if err != nil {
			return fmt.Errorf("SEEK_HOLE at %d: %w", data, err)
		}
		if hole <= data {
			return fmt.Errorf("SEEK_HOLE at %d returned %d", data, hole)
		}
		off = uint64(hole)
	}
	return nil
}
