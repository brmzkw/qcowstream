// Package testutil holds helpers shared by the tests of every package and of
// the integration module.
package testutil

import (
	"bytes"
	"encoding/binary"
	"math/rand/v2"
	"runtime"

	"github.com/brmzkw/qcowstream"
)

// RandomDisk returns size deterministic pseudo-random bytes.
func RandomDisk(size, seed uint64) []byte {
	var key [32]byte
	binary.LittleEndian.PutUint64(key[:], seed)
	b := make([]byte, size)
	_, _ = rand.NewChaCha8(key).Read(b)
	return b
}

// Change overwrites a region with fresh random bytes, or with zeros.
type Change struct {
	Offset, Length uint64
	Zero           bool
}

// Apply returns a copy of parent with the changes applied, and the changed
// extents in order.
func Apply(parent []byte, seed uint64, changes ...Change) ([]byte, qcowstream.SliceExtents) {
	child := bytes.Clone(parent)
	var ext qcowstream.SliceExtents
	for i, c := range changes {
		region := child[c.Offset : c.Offset+c.Length]
		if c.Zero {
			clear(region)
		} else {
			copy(region, RandomDisk(c.Length, seed+uint64(i)))
		}
		ext = append(ext, qcowstream.Extent{Offset: c.Offset, Length: c.Length})
	}
	return child, ext
}

// Allocated returns the heap bytes allocated while f runs.
func Allocated(f func()) uint64 {
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	f()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}
