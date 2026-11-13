package qcowstream

import (
	"context"
	"io"
)

// Reader reads the virtual disk at an offset. Implementations may return
// fewer bytes than asked without an error; callers loop.
type Reader interface {
	ReadAtContext(ctx context.Context, p []byte, off int64) (int, error)
}

// Sizer is implemented by sources that know their virtual size. Write checks
// it against the plan.
type Sizer interface {
	Size() uint64
}

// ReaderAtSource adapts an io.ReaderAt, such as an *os.File, to Reader.
//
// The context is checked before each read but cannot interrupt a ReadAt that
// is already blocked: that depends on the underlying object.
type ReaderAtSource struct {
	R   io.ReaderAt
	Len uint64
}

// NewReaderAtSource returns a Reader over the first size bytes of r.
func NewReaderAtSource(r io.ReaderAt, size uint64) *ReaderAtSource {
	return &ReaderAtSource{R: r, Len: size}
}

func (s *ReaderAtSource) ReadAtContext(ctx context.Context, p []byte, off int64) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	return s.R.ReadAt(p, off)
}

func (s *ReaderAtSource) Size() uint64 { return s.Len }
