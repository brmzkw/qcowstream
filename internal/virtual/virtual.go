// Package virtual provides deterministic disks and region maps of any size,
// computed on the fly, for testing large volumes without storing them.
package virtual

import (
	"context"

	"github.com/brmzkw/qcowstream"
)

// Disk is a virtual disk whose content is a function of the offset: each
// MiB holds one repeated byte, Pattern(off). It makes reads cheap and lets
// QEMU check content with qemu-io "read -P".
type Disk struct {
	Len uint64
}

// Pattern returns the byte stored at off.
func Pattern(off uint64) byte { return byte((off>>20)*7 + 3) }

func (d Disk) Size() uint64 { return d.Len }

func (d Disk) ReadAtContext(ctx context.Context, p []byte, off int64) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	pos := uint64(off)
	for i := 0; i < len(p); {
		b := Pattern(pos)
		next := min((pos>>20+1)<<20-pos, uint64(len(p)-i))
		for j := range next {
			p[i+int(j)] = b
		}
		i += int(next)
		pos += next
	}
	return len(p), nil
}

// Strided yields Count extents of Length bytes, one every Stride bytes from
// Start. It replays identically and holds no list.
type Strided struct {
	Start, Stride, Length, Count uint64
}

func (s Strided) Walk(ctx context.Context, yield func(qcowstream.Extent) error) error {
	for i := range s.Count {
		if i%4096 == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		if err := yield(qcowstream.Extent{Offset: s.Start + i*s.Stride, Length: s.Length}); err != nil {
			return err
		}
	}
	return nil
}
