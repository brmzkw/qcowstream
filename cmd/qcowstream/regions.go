package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/brmzkw/qcowstream"
)

// maxRegionsFile bounds the regions file. It is a test aid; real bitmaps
// come from NBD.
const maxRegionsFile = 16 << 20

// regionsFile is an ExtentSource reading
//
//	{"virtual_size": N, "extents": [{"offset": N, "length": N}, ...]}
//
// with a streaming decoder on every walk, so the list is never held in
// memory. The library validates order, bounds and replay.
type regionsFile struct {
	path        string
	virtualSize uint64
}

func openRegions(path string) (*regionsFile, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if fi.Size() > maxRegionsFile {
		return nil, fmt.Errorf("regions file %s is %d bytes, the limit is %d", path, fi.Size(), maxRegionsFile)
	}
	r := &regionsFile{path: path}
	if err := r.Walk(context.Background(), func(qcowstream.Extent) error { return nil }); err != nil {
		return nil, err
	}
	return r, nil
}

func (r *regionsFile) Walk(ctx context.Context, yield func(qcowstream.Extent) error) error {
	f, err := os.Open(r.path)
	if err != nil {
		return err
	}
	defer f.Close()
	dec := json.NewDecoder(io.LimitReader(f, maxRegionsFile))
	fail := func(what string) error { return fmt.Errorf("regions file %s: %s", r.path, what) }
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return fail("expected an object")
	}
	var sawSize, sawExtents bool
	for dec.More() {
		t, err := dec.Token()
		if err != nil {
			return fail(err.Error())
		}
		switch t {
		case "virtual_size":
			if err := dec.Decode(&r.virtualSize); err != nil {
				return fail("virtual_size: " + err.Error())
			}
			sawSize = true
		case "extents":
			if t, err := dec.Token(); err != nil || t != json.Delim('[') {
				return fail("extents must be an array")
			}
			for dec.More() {
				if err := ctx.Err(); err != nil {
					return err
				}
				var e struct {
					Offset *uint64 `json:"offset"`
					Length *uint64 `json:"length"`
				}
				if err := dec.Decode(&e); err != nil {
					return fail("extent: " + err.Error())
				}
				if e.Offset == nil || e.Length == nil {
					return fail("each extent needs offset and length")
				}
				if err := yield(qcowstream.Extent{Offset: *e.Offset, Length: *e.Length}); err != nil {
					return err
				}
			}
			if _, err := dec.Token(); err != nil {
				return fail(err.Error())
			}
			sawExtents = true
		default:
			return fail(fmt.Sprintf("unknown key %v", t))
		}
	}
	if !sawSize || !sawExtents {
		return fail("virtual_size and extents are required")
	}
	return nil
}
