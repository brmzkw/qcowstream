package nbdsource

import (
	"context"
	"errors"
	"fmt"
	"sync"

	nbd "github.com/digitalocean/go-nbd"
	"github.com/digitalocean/go-nbd/nbdmeta"

	"github.com/brmzkw/qcowstream"
)

// statusWindow is the length of one BLOCK_STATUS request. Replies are
// bounded by the client's scratch buffer whatever the window.
const statusWindow = 1 << 30

// statusConn is a connection with one negotiated meta context, used for
// BLOCK_STATUS only. A failed or interrupted walk closes it; the next walk
// dials again.
type statusConn struct {
	s       *Source
	context string
	mu      sync.Mutex
	c       *nbd.Conn
	id      uint32
}

// negotiate dials a connection granting ctxName, or fails with
// ErrMissingContext.
func (s *Source) negotiate(ctx context.Context, ctxName string) (*nbd.Conn, uint32, error) {
	var id uint32
	found := false
	c, _, err := s.dial(ctx, statusBufferSize, func(c *nbd.Conn) error {
		if err := c.StructuredReplies(); err != nil {
			return fmt.Errorf("structured replies: %w", err)
		}
		err := c.SetMetaContext(s.opts.Export, []string{ctxName}, func(m nbd.MetaContext) error {
			// Match by name: IDs are arbitrary and replies unordered.
			if m.Name == ctxName {
				id, found = m.ID, true
			}
			return nil
		})
		var ne *nbd.NegotiationError
		if errors.As(err, &ne) {
			return fmt.Errorf("%w: %q refused: %w", ErrMissingContext, ctxName, err)
		}
		if err != nil {
			return err
		}
		if !found {
			return fmt.Errorf("%w: server did not grant %q", ErrMissingContext, ctxName)
		}
		return nil
	})
	return c, id, err
}

func (sc *statusConn) acquire(ctx context.Context) error {
	if sc.c != nil {
		return nil
	}
	c, id, err := sc.s.negotiate(ctx, sc.context)
	if err != nil {
		return err
	}
	sc.c, sc.id = c, id
	return nil
}

func (sc *statusConn) drop() {
	if sc.c != nil {
		sc.c.Close()
		sc.c = nil
	}
}

func (sc *statusConn) close() {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	if sc.c != nil {
		_ = sc.c.Disconnect()
		sc.drop()
	}
}

// walk queries [lo, hi) and calls fn with consecutive runs and their flags.
// Replies may be short, fragmented into several chunks, or overrun the
// request with their last extent; every run passed to fn lies in [lo, hi).
func (sc *statusConn) walk(ctx context.Context, lo, hi uint64, fn func(off, length uint64, flags uint32) error) (err error) {
	if err := sc.acquire(ctx); err != nil {
		return err
	}
	stop := interruptOnCancel(ctx, sc.c)
	defer stop()
	defer func() {
		if err != nil {
			sc.drop() // unconsumed replies or a deadline leave it unusable
		}
	}()
	for pos := lo; pos < hi; {
		end := pos + statusWindowFor(hi-pos, sc.s.minBlock)
		covered := pos
		err := sc.c.BlockStatus(pos, uint32(end-pos), func(st nbd.BlockStatus) error {
			if st.ID != sc.id {
				return fmt.Errorf("reply for meta context %d, negotiated %d for %q", st.ID, sc.id, sc.context)
			}
			for _, d := range st.Descriptors {
				if d.Length == 0 {
					return fmt.Errorf("zero-length extent at %d", covered)
				}
				if covered >= end {
					return fmt.Errorf("extent at %d after the request end %d", covered, end)
				}
				n := uint64(d.Length)
				if covered+n > end {
					n = end - covered // the last extent may overrun the request
				}
				if err := fn(covered, n, d.Status); err != nil {
					return err
				}
				covered += n
			}
			return nil
		}, 0)
		if err != nil {
			if ctx.Err() != nil {
				return sc.s.connErr(ctx, "block status", err)
			}
			return err
		}
		if covered == pos {
			return fmt.Errorf("nbdsource: block status at %d made no progress", pos)
		}
		pos = covered // continue after a short reply
	}
	return nil
}

func statusWindowFor(remaining, minBlock uint64) uint64 {
	w := min(remaining, statusWindow)
	if w > minBlock {
		w -= w % minBlock
	}
	return w
}

// dirtyRegions enumerates the dirty extents of a QEMU dirty bitmap.
type dirtyRegions struct{ sc *statusConn }

func (d *dirtyRegions) Walk(ctx context.Context, yield func(qcowstream.Extent) error) error {
	d.sc.mu.Lock()
	defer d.sc.mu.Unlock()
	return d.sc.walk(ctx, 0, d.sc.s.size, func(off, n uint64, flags uint32) error {
		if !nbdmeta.DirtyBitmapFlags(flags).Dirty() {
			return nil
		}
		return yield(qcowstream.Extent{Offset: off, Length: n})
	})
}
