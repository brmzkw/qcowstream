// Package nbdtest is a minimal NBD server for tests. It speaks fixed
// newstyle negotiation, structured replies, meta contexts and BLOCK_STATUS,
// and enforces the access rules of a Proxmox backup export: with a dirty
// bitmap, any READ or allocation query outside the dirty regions is refused
// and recorded as a violation.
package nbdtest

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
)

// Protocol constants, from the NBD protocol specification.
const (
	nbdMagic      = 0x4e42444d41474943
	ihaveopt      = 0x49484156454F5054
	optReplyMagic = 0x0003e889045565a9
	requestMagic  = 0x25609513
	simpleMagic   = 0x67446698
	structMagic   = 0x668e33ef

	flagFixedNewstyle = 1 << 0
	flagNoZeroes      = 1 << 1

	optAbort          = 2
	optInfo           = 6
	optGo             = 7
	optStructured     = 8
	optSetMetaContext = 10

	repAck         = 1
	repInfo        = 3
	repMetaContext = 4
	repErrUnsup    = 1<<31 + 1
	repErrPolicy   = 1<<31 + 2
	repErrUnknown  = 1<<31 + 6

	infoExport    = 0
	infoBlockSize = 3

	txHasFlags   = 1 << 0
	txReadOnly   = 1 << 1
	txMultiConn  = 1 << 8
	cmdRead      = 0
	cmdWrite     = 1
	cmdDisc      = 2
	cmdTrim      = 4
	cmdWriteZero = 6
	cmdBlockStat = 7

	replyFlagDone   = 1 << 0
	replyOffsetData = 1
	replyBlockStat  = 5
	replyError      = 1<<15 + 1

	errPerm  = 1
	errInval = 22

	// Base allocation flags.
	StateHole = 1 << 0
	StateZero = 1 << 1
	// Dirty bitmap flag.
	StateDirty = 1 << 0
)

// Extent is a byte range with status flags.
type Extent struct {
	Offset, Length uint64
}

// Server serves one export from memory.
type Server struct {
	Export string
	Data   []byte
	// Size overrides len(Data); bytes past Data read as zeros.
	Size uint64

	// Block size constraints; MaxBlock 0 sends no NBD_INFO_BLOCK_SIZE.
	MinBlock, PrefBlock, MaxBlock uint32

	// Bitmap names the dirty bitmap context. With a bitmap, reads and
	// base:allocation queries are allowed only inside Dirty.
	Bitmap string
	Dirty  []Extent

	// BaseAllocation offers base:allocation; Zero lists regions reported
	// with the ZERO flag, Hole regions reported as HOLE without ZERO.
	BaseAllocation bool
	Zero, Hole     []Extent

	// RefuseMetaContext answers SET_META_CONTEXT with a policy error.
	RefuseMetaContext bool
	// ExtentsPerChunk splits BLOCK_STATUS replies into chunks of this many
	// extents; 0 sends one chunk.
	ExtentsPerChunk int
	// StatusLimit, if set, answers BLOCK_STATUS for at most this many bytes,
	// a short reply the client must continue from.
	StatusLimit uint64
	// WrongContextID sends BLOCK_STATUS chunks with an unknown context ID.
	WrongContextID bool
	// ZeroLengthExtent sends a zero-length extent in BLOCK_STATUS replies.
	ZeroLengthExtent bool
	// Stall makes the server stop answering requests.
	Stall atomic.Bool

	mu         sync.Mutex
	violations []string
	reads      []Extent
	statusReqs []Extent
	conns      atomic.Int32
	maxConns   atomic.Int32
	listener   net.Listener
}

// Start listens on a Unix socket in a temporary directory and returns its
// path. The server stops when the test ends.
func (s *Server) Start(t testing.TB) string {
	// Unix socket paths are short (104 bytes on macOS): t.TempDir is too deep.
	dir, err := os.MkdirTemp("", "nbd")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "s")
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	s.listener = l
	t.Cleanup(func() {
		l.Close()
		os.RemoveAll(dir)
	})
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			n := s.conns.Add(1)
			for {
				m := s.maxConns.Load()
				if n <= m || s.maxConns.CompareAndSwap(m, n) {
					break
				}
			}
			go func() {
				defer s.conns.Add(-1)
				defer c.Close()
				_ = s.serve(c)
			}()
		}
	}()
	return path
}

// Violations returns the forbidden accesses the client attempted.
func (s *Server) Violations() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.violations...)
}

// Reads returns the READ requests served.
func (s *Server) Reads() []Extent {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Extent(nil), s.reads...)
}

// MaxConns returns the highest number of simultaneous connections.
func (s *Server) MaxConns() int { return int(s.maxConns.Load()) }

func (s *Server) violate(format string, args ...any) {
	s.mu.Lock()
	s.violations = append(s.violations, fmt.Sprintf(format, args...))
	s.mu.Unlock()
}

var be = binary.BigEndian

type conn struct {
	s          *Server
	c          net.Conn
	structured bool
	ctxBitmap  uint32 // 0: not negotiated
	ctxAlloc   uint32
}

func (s *Server) serve(c net.Conn) error {
	cn := &conn{s: s, c: c}
	var hello [18]byte
	be.PutUint64(hello[0:], nbdMagic)
	be.PutUint64(hello[8:], ihaveopt)
	be.PutUint16(hello[16:], flagFixedNewstyle|flagNoZeroes)
	if _, err := c.Write(hello[:]); err != nil {
		return err
	}
	var clientFlags uint32
	if err := binary.Read(c, be, &clientFlags); err != nil {
		return err
	}
	if done, err := cn.options(); err != nil || done {
		return err
	}
	return cn.transmission()
}

func (cn *conn) optReply(opt, typ uint32, data []byte) error {
	b := make([]byte, 20+len(data))
	be.PutUint64(b, optReplyMagic)
	be.PutUint32(b[8:], opt)
	be.PutUint32(b[12:], typ)
	be.PutUint32(b[16:], uint32(len(data)))
	copy(b[20:], data)
	_, err := cn.c.Write(b)
	return err
}

// options runs the negotiation phase. It returns done when the client
// aborted.
func (cn *conn) options() (done bool, err error) {
	s := cn.s
	for nextID := uint32(1); ; {
		var hdr [16]byte
		if _, err := io.ReadFull(cn.c, hdr[:]); err != nil {
			return true, err
		}
		if be.Uint64(hdr[:]) != ihaveopt {
			return true, errors.New("bad option magic")
		}
		opt, n := be.Uint32(hdr[8:]), be.Uint32(hdr[12:])
		data := make([]byte, n)
		if _, err := io.ReadFull(cn.c, data); err != nil {
			return true, err
		}
		switch opt {
		case optAbort:
			return true, cn.optReply(opt, repAck, nil)
		case optStructured:
			cn.structured = true
			err = cn.optReply(opt, repAck, nil)
		case optSetMetaContext:
			if s.RefuseMetaContext {
				err = cn.optReply(opt, repErrPolicy, nil)
				break
			}
			name, rest := readString(data)
			if name != s.Export || len(rest) < 4 {
				err = cn.optReply(opt, repErrUnknown, nil)
				break
			}
			count := be.Uint32(rest)
			rest = rest[4:]
			cn.ctxBitmap, cn.ctxAlloc = 0, 0
			for range count {
				var q string
				q, rest = readString(rest)
				var id *uint32
				switch {
				case s.Bitmap != "" && q == "qemu:dirty-bitmap:"+s.Bitmap:
					id = &cn.ctxBitmap
				case s.BaseAllocation && q == "base:allocation":
					id = &cn.ctxAlloc
				default:
					continue
				}
				// IDs are not in query order, to catch clients assuming it.
				nextID += 7
				*id = nextID
				reply := make([]byte, 4+len(q))
				be.PutUint32(reply, *id)
				copy(reply[4:], q)
				if err = cn.optReply(opt, repMetaContext, reply); err != nil {
					return true, err
				}
			}
			err = cn.optReply(opt, repAck, nil)
		case optInfo, optGo:
			name, _ := readString(data)
			if name != s.Export {
				err = cn.optReply(opt, repErrUnknown, []byte("no such export"))
				break
			}
			var info [12]byte
			be.PutUint16(info[0:], infoExport)
			be.PutUint64(info[2:], s.size())
			be.PutUint16(info[10:], txHasFlags|txReadOnly|txMultiConn)
			if err = cn.optReply(opt, repInfo, info[:]); err != nil {
				return true, err
			}
			if s.MaxBlock != 0 {
				var bs [14]byte
				be.PutUint16(bs[0:], infoBlockSize)
				be.PutUint32(bs[2:], s.MinBlock)
				be.PutUint32(bs[6:], s.PrefBlock)
				be.PutUint32(bs[10:], s.MaxBlock)
				if err = cn.optReply(opt, repInfo, bs[:]); err != nil {
					return true, err
				}
			}
			if err = cn.optReply(opt, repAck, nil); err != nil || opt == optGo {
				return false, err
			}
		default:
			err = cn.optReply(opt, repErrUnsup, nil)
		}
		if err != nil {
			return true, err
		}
	}
}

func readString(b []byte) (string, []byte) {
	if len(b) < 4 {
		return "", nil
	}
	n := be.Uint32(b)
	if uint64(n) > uint64(len(b)-4) {
		return "", nil
	}
	return string(b[4 : 4+n]), b[4+n:]
}

func (cn *conn) transmission() error {
	s := cn.s
	for {
		var hdr [28]byte
		if _, err := io.ReadFull(cn.c, hdr[:]); err != nil {
			return err
		}
		if be.Uint32(hdr[:]) != requestMagic {
			return errors.New("bad request magic")
		}
		typ, cookie := be.Uint16(hdr[6:]), be.Uint64(hdr[8:])
		off, length := be.Uint64(hdr[16:]), uint64(be.Uint32(hdr[24:]))
		for s.Stall.Load() {
			// Never answer: the client must give up on its own.
			if _, err := cn.c.Read(make([]byte, 1)); err != nil {
				return err
			}
		}
		var err error
		switch typ {
		case cmdDisc:
			return nil
		case cmdRead:
			err = cn.read(cookie, off, length)
		case cmdBlockStat:
			err = cn.blockStatus(cookie, off, length)
		case cmdWrite, cmdTrim, cmdWriteZero:
			s.violate("command %d at [%d, +%d) on a read-only source", typ, off, length)
			err = cn.fail(cookie, errPerm)
		default:
			err = cn.fail(cookie, errInval)
		}
		if err != nil {
			return err
		}
	}
}

func (cn *conn) fail(cookie uint64, code uint32) error {
	if !cn.structured {
		var b [16]byte
		be.PutUint32(b[0:], simpleMagic)
		be.PutUint32(b[4:], code)
		be.PutUint64(b[8:], cookie)
		_, err := cn.c.Write(b[:])
		return err
	}
	payload := make([]byte, 6)
	be.PutUint32(payload, code)
	return cn.chunk(cookie, replyFlagDone, replyError, payload)
}

func (cn *conn) chunk(cookie uint64, flags, typ uint16, payload []byte) error {
	b := make([]byte, 20+len(payload))
	be.PutUint32(b[0:], structMagic)
	be.PutUint16(b[4:], flags)
	be.PutUint16(b[6:], typ)
	be.PutUint64(b[8:], cookie)
	be.PutUint32(b[16:], uint32(len(payload)))
	copy(b[20:], payload)
	_, err := cn.c.Write(b)
	return err
}

func (s *Server) size() uint64 {
	if s.Size != 0 {
		return s.Size
	}
	return uint64(len(s.Data))
}

// bytes returns the content of [off, off+n), zeros past Data.
func (s *Server) bytes(off, n uint64) []byte {
	b := make([]byte, n)
	if off < uint64(len(s.Data)) {
		copy(b, s.Data[off:])
	}
	return b
}

// allowed reports whether [off, off+length) lies inside the dirty regions,
// or anywhere when there is no bitmap.
func (s *Server) allowed(off, length uint64) bool {
	if s.Bitmap == "" {
		return true
	}
	for _, d := range s.Dirty {
		if off >= d.Offset && off+length <= d.Offset+d.Length {
			return true
		}
	}
	return false
}

func (cn *conn) read(cookie, off, length uint64) error {
	s := cn.s
	switch {
	case off+length > s.size() || length == 0:
		s.violate("READ [%d, +%d) outside the export", off, length)
		return cn.fail(cookie, errInval)
	case s.MaxBlock != 0 && length > uint64(s.MaxBlock):
		s.violate("READ of %d bytes over the %d-byte maximum", length, s.MaxBlock)
		return cn.fail(cookie, errInval)
	case s.MinBlock > 1 && (off%uint64(s.MinBlock) != 0 || length%uint64(s.MinBlock) != 0):
		s.violate("READ [%d, +%d) not aligned to %d", off, length, s.MinBlock)
		return cn.fail(cookie, errInval)
	case !s.allowed(off, length):
		s.violate("READ [%d, +%d) outside the dirty regions", off, length)
		return cn.fail(cookie, errPerm)
	}
	s.mu.Lock()
	s.reads = append(s.reads, Extent{off, length})
	s.mu.Unlock()
	data := s.bytes(off, length)
	if !cn.structured {
		b := make([]byte, 16+len(data))
		be.PutUint32(b[0:], simpleMagic)
		be.PutUint64(b[8:], cookie)
		copy(b[16:], data)
		_, err := cn.c.Write(b)
		return err
	}
	// Two chunks, second half first: the client must place them by offset.
	half := length / 2
	for i, part := range []struct{ off, n uint64 }{{off + half, length - half}, {off, half}} {
		if part.n == 0 {
			continue
		}
		payload := make([]byte, 8+part.n)
		be.PutUint64(payload, part.off)
		copy(payload[8:], s.bytes(part.off, part.n))
		var flags uint16
		if i == 1 || half == 0 {
			flags = replyFlagDone
		}
		if err := cn.chunk(cookie, flags, replyOffsetData, payload); err != nil {
			return err
		}
	}
	return nil
}

type status struct {
	length uint64
	flags  uint32
}

// statuses describes [off, off+length) as runs of flags, where in(x) gives
// the flags at byte x and changes only at the boundaries of regions.
func statuses(off, length uint64, regions []Extent, flag uint32) []status {
	var out []status
	end := off + length
	for pos := off; pos < end; {
		next, flags := end, uint32(0)
		for _, r := range regions {
			rEnd := r.Offset + r.Length
			switch {
			case pos >= r.Offset && pos < rEnd:
				flags, next = flag, min(next, rEnd)
			case r.Offset > pos:
				next = min(next, r.Offset)
			}
		}
		out = append(out, status{next - pos, flags})
		pos = next
	}
	return out
}

func (cn *conn) blockStatus(cookie, off, length uint64) error {
	s := cn.s
	if !cn.structured || (cn.ctxBitmap == 0 && cn.ctxAlloc == 0) {
		return cn.fail(cookie, errInval)
	}
	if off+length > s.size() || length == 0 {
		s.violate("BLOCK_STATUS [%d, +%d) outside the export", off, length)
		return cn.fail(cookie, errInval)
	}
	if cn.ctxAlloc != 0 && !s.allowed(off, length) {
		// Like Proxmox: allocation status outside dirty parts is refused.
		s.violate("base:allocation query [%d, +%d) outside the dirty regions", off, length)
		return cn.fail(cookie, errPerm)
	}
	s.mu.Lock()
	s.statusReqs = append(s.statusReqs, Extent{off, length})
	s.mu.Unlock()
	if s.StatusLimit != 0 && length > s.StatusLimit {
		length = s.StatusLimit
	}
	type ctxReply struct {
		id  uint32
		sts []status
	}
	var replies []ctxReply
	if cn.ctxBitmap != 0 {
		replies = append(replies, ctxReply{cn.ctxBitmap, statuses(off, length, s.Dirty, StateDirty)})
	}
	if cn.ctxAlloc != 0 {
		zero := statuses(off, length, s.Zero, StateZero|StateHole)
		var merged []status
		// Overlay holes without the ZERO flag.
		pos := off
		for _, z := range zero {
			if z.flags != 0 {
				merged = append(merged, z)
			} else {
				for _, h := range statuses(pos, z.length, s.Hole, StateHole) {
					merged = append(merged, h)
				}
			}
			pos += z.length
		}
		replies = append(replies, ctxReply{cn.ctxAlloc, merged})
	}
	var chunks [][]byte
	for _, r := range replies {
		id := r.id
		if s.WrongContextID {
			id += 1000
		}
		per := s.ExtentsPerChunk
		if per == 0 {
			per = len(r.sts)
		}
		for i := 0; i < len(r.sts); i += per {
			part := r.sts[i:min(i+per, len(r.sts))]
			payload := make([]byte, 4, 4+8*len(part)+8)
			be.PutUint32(payload, id)
			for _, st := range part {
				payload = be.AppendUint32(payload, uint32(st.length))
				payload = be.AppendUint32(payload, st.flags)
			}
			if s.ZeroLengthExtent {
				payload = be.AppendUint32(payload, 0)
				payload = be.AppendUint32(payload, 0)
			}
			chunks = append(chunks, payload)
		}
	}
	for i, p := range chunks {
		var flags uint16
		if i == len(chunks)-1 {
			flags = replyFlagDone
		}
		if err := cn.chunk(cookie, flags, replyBlockStat, p); err != nil {
			return err
		}
	}
	return nil
}
