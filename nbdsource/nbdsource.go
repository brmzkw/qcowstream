// Package nbdsource reads a disk exported over NBD on a Unix socket, for
// qcowstream. It provides positioned reads and, for incremental images, the
// regions of a QEMU dirty bitmap.
//
// The export must be a stable view for the whole operation: reads and
// bitmap queries across all walks must see the same disk and the same
// bitmap. An NBD URI alone does not guarantee a snapshot; ensuring it is the
// caller's responsibility. The source never writes, trims or resets
// anything.
//
// The NBD client is github.com/digitalocean/go-nbd (Apache-2.0, pure Go). It
// serializes requests on a connection, so concurrent reads use a pool of
// connections to the same export.
package nbdsource

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"sync"
	"time"

	nbd "github.com/digitalocean/go-nbd"

	"github.com/brmzkw/qcowstream"
)

// ErrMissingContext reports that the server did not grant a required meta
// context, such as the dirty bitmap.
var ErrMissingContext = errors.New("nbdsource: meta context not available")

// defaultMaxBlock is the largest request the NBD specification lets a client
// send when the server states no limit.
const defaultMaxBlock = 32 << 20

// Options configures a Source.
type Options struct {
	Socket string // Unix socket path
	Export string // export name

	// Connections is the number of read connections, for concurrent reads.
	// Default 1.
	Connections int

	// Bitmap names a QEMU dirty bitmap exported as the meta context
	// qemu:dirty-bitmap:<Bitmap>. It enables Changed.
	Bitmap string

	// DetectZeros negotiates base:allocation so Zeros can report regions
	// that read as zeros. A server without it gives no zero information.
	DetectZeros bool
}

// Source is an NBD export opened for reading.
type Source struct {
	opts     Options
	size     uint64
	minBlock uint64
	maxBlock uint64 // largest read request, a multiple of minBlock

	pool chan *nbd.Conn // read connections; nil entries are redialed

	dirty *statusConn
	zeros *zeroRegions
}

// Open connects to the export and checks it.
func Open(ctx context.Context, opts Options) (*Source, error) {
	if opts.Socket == "" || opts.Export == "" {
		return nil, fmt.Errorf("nbdsource: socket and export name are required")
	}
	if opts.Connections == 0 {
		opts.Connections = 1
	}
	if opts.Connections < 0 || opts.Connections > qcowstream.MaxConcurrency {
		return nil, fmt.Errorf("nbdsource: %d connections, want 1 to %d", opts.Connections, qcowstream.MaxConcurrency)
	}
	s := &Source{opts: opts, pool: make(chan *nbd.Conn, opts.Connections)}
	c, info, err := s.dial(ctx, readBufferSize, nil)
	if err != nil {
		return nil, err
	}
	s.size = info.Size
	s.minBlock, s.maxBlock = 1, defaultMaxBlock
	if info.ValidBlockSize {
		s.minBlock = uint64(max(info.MinBlockSize, 1))
		if info.MaxBlockSize != 0 {
			s.maxBlock = uint64(info.MaxBlockSize)
		}
	}
	s.maxBlock = s.maxBlock / s.minBlock * s.minBlock
	if s.maxBlock == 0 {
		c.Close()
		return nil, fmt.Errorf("nbdsource: unusable block sizes: minimum %d, maximum %d", info.MinBlockSize, info.MaxBlockSize)
	}
	s.pool <- c
	for range opts.Connections - 1 {
		s.pool <- nil // dialed on first use
	}
	if opts.Bitmap != "" {
		// Negotiate now so a missing bitmap fails before any planning.
		s.dirty = &statusConn{s: s, context: "qemu:dirty-bitmap:" + opts.Bitmap}
		if err := s.dirty.acquire(ctx); err != nil {
			s.Close()
			return nil, err
		}
	}
	if opts.DetectZeros {
		if err := s.openZeros(ctx); err != nil {
			s.Close()
			return nil, err
		}
	}
	return s, nil
}

// Changed returns the dirty regions of the bitmap, read with
// NBD_CMD_BLOCK_STATUS, or nil without a bitmap. Each walk queries the whole
// export again; the bitmap must not change between walks.
func (s *Source) Changed() qcowstream.ExtentSource {
	if s.dirty == nil {
		return nil
	}
	return &dirtyRegions{sc: s.dirty}
}

// dial connects, negotiates and enters transmission. setup runs between
// the handshake and NBD_OPT_GO, to negotiate options.
func (s *Source) dial(ctx context.Context, bufSize int, setup func(*nbd.Conn) error) (*nbd.Conn, nbd.ExportInfo, error) {
	var info nbd.ExportInfo
	uri, err := nbd.ParseURI("nbd+unix:///?socket=" + url.QueryEscape(s.opts.Socket))
	if err != nil {
		return nil, info, err
	}
	var d nbd.Dialer
	c, err := d.Dial(ctx, uri, nbd.WithBuffer(make([]byte, bufSize)))
	if err != nil {
		return nil, info, fmt.Errorf("nbdsource: %w", err)
	}
	ok := false
	defer func() {
		if !ok {
			c.Close()
		}
	}()
	stop := interruptOnCancel(ctx, c)
	defer stop()
	if err := c.Connect(); err != nil {
		return nil, info, s.connErr(ctx, "handshake", err)
	}
	if setup != nil {
		if err := setup(c); err != nil {
			return nil, info, s.connErr(ctx, "negotiation", err)
		}
	}
	err = c.Go(s.opts.Export, []nbd.InfoRequest{nbd.InfoRequestExport, nbd.InfoRequestBlockSize}, func(i nbd.ExportInfo) error {
		info = i
		return nil
	})
	if err != nil {
		return nil, info, s.connErr(ctx, fmt.Sprintf("export %q", s.opts.Export), err)
	}
	if !info.ValidExport {
		return nil, info, fmt.Errorf("nbdsource: export %q: server sent no size", s.opts.Export)
	}
	ok = true
	return c, info, nil
}

// Scratch buffers bound one reply chunk the client accepts, other than read
// data, which goes straight to the caller's buffer. Read connections only
// need room for error messages; QEMU sends BLOCK_STATUS chunks of up to
// 1 MiB of extents.
const (
	readBufferSize   = 8 << 10
	statusBufferSize = 1<<20 + 4096
)

// MemoryUse returns the most scratch memory the source holds with n read
// connections and its status connections, outside the qcowstream budget.
func MemoryUse(n int) uint64 { return uint64(n)*readBufferSize + 3*statusBufferSize }

// interruptOnCancel makes blocked I/O on c fail when ctx is done. The
// connection is unusable afterwards.
func interruptOnCancel(ctx context.Context, c *nbd.Conn) func() bool {
	return context.AfterFunc(ctx, func() { _ = c.SetDeadline(time.Unix(1, 0)) })
}

func (s *Source) connErr(ctx context.Context, what string, err error) error {
	if ctx.Err() != nil {
		return fmt.Errorf("nbdsource: %s: %w: %w", what, qcowstream.ErrCanceled, context.Cause(ctx))
	}
	return fmt.Errorf("nbdsource: %s: %w", what, err)
}

// Size returns the export size in bytes.
func (s *Source) Size() uint64 { return s.size }

// ReadAtContext reads len(p) bytes at off, in requests no larger than the
// server allows. Cancelling ctx interrupts a request waiting on the network.
func (s *Source) ReadAtContext(ctx context.Context, p []byte, off int64) (int, error) {
	if off < 0 || uint64(off)+uint64(len(p)) > s.size {
		return 0, fmt.Errorf("nbdsource: read [%d, +%d) outside the %d-byte export", off, len(p), s.size)
	}
	if uint64(off)%s.minBlock != 0 || uint64(len(p))%s.minBlock != 0 {
		return 0, fmt.Errorf("nbdsource: read [%d, +%d) is not aligned to the server's %d-byte minimum block size",
			off, len(p), s.minBlock)
	}
	var c *nbd.Conn
	select {
	case c = <-s.pool:
	case <-ctx.Done():
		return 0, ctx.Err()
	}
	healthy := false
	defer func() {
		if !healthy && c != nil {
			c.Close()
			c = nil
		}
		s.pool <- c
	}()
	if c == nil {
		var err error
		if c, _, err = s.dial(ctx, readBufferSize, nil); err != nil {
			return 0, err
		}
	}
	stop := interruptOnCancel(ctx, c)
	defer stop()
	var done int
	for done < len(p) {
		n := min(uint64(len(p)-done), s.maxBlock)
		got, err := c.Read(p[done:done+int(n)], uint64(off)+uint64(done), 0)
		if err == nil && uint64(got) != n {
			err = fmt.Errorf("server returned %d of %d bytes", got, n)
		}
		if err != nil {
			return done, s.connErr(ctx, fmt.Sprintf("read [%d, +%d)", uint64(off)+uint64(done), n), err)
		}
		done += int(n)
	}
	healthy = true
	return done, nil
}

// Close closes every connection.
func (s *Source) Close() error {
	if s.dirty != nil {
		s.dirty.close()
	}
	if s.zeros != nil {
		s.zeros.alloc.close()
		if s.zeros.dirty != nil {
			s.zeros.dirty.close()
		}
	}
	var wg sync.WaitGroup
	for range s.opts.Connections {
		c := <-s.pool
		if c == nil {
			continue
		}
		wg.Go(func() {
			_ = c.Disconnect()
			c.Close()
		})
	}
	wg.Wait()
	return nil
}
