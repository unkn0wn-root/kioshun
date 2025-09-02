package cluster

import (
	"bufio"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	cbor "github.com/fxamacker/cbor/v2"
)

const (
	penaltyBase   = 2 * time.Second // first timeout → 2s
	penaltyMax    = 8 * time.Second // cap the penalty
	backoffWindow = 5 * time.Second // time window to keep growing the streak
)

type peerConn struct {
	addr         string
	self         string
	conn         net.Conn
	r            *bufio.Reader
	w            *bufio.Writer
	mu           sync.Mutex
	pend         sync.Map // reqID -> chan []byte
	closed       chan struct{}
	maxFrame     int
	readTO       time.Duration
	writeTO      time.Duration
	idleTO       time.Duration
	inflightCh   chan struct{}
	token        string
	penaltyUntil int64
	lastTimeout  int64
	toStreak     uint32
}

// dialPeer establishes a TCP/TLS connection, performs an optional Hello auth,
// and starts a read loop that dispatches responses by request ID via pend map.
func dialPeer(self string, addr string, tlsConf *tls.Config, maxFrame int, readTO, writeTO, idleTO time.Duration, inflight int, token string) (*peerConn, error) {
	d := &net.Dialer{
		Timeout:   readTO,
		KeepAlive: 45 * time.Second,
		Control: func(network, address string, c syscall.RawConn) error {
			var ctrlErr error
			_ = c.Control(func(fd uintptr) {
				_ = syscall.SetsockoptInt(int(fd), syscall.IPPROTO_TCP, syscall.TCP_NODELAY, 1)
			})
			return ctrlErr
		},
	}

	var c net.Conn
	var err error
	if tlsConf != nil {
		c, err = tls.DialWithDialer(d, "tcp", addr, tlsConf)
	} else {
		c, err = d.Dial("tcp", addr)
	}
	if err != nil {
		return nil, err
	}

	if tlsConf == nil {
		if tc, ok := c.(*net.TCPConn); ok {
			_ = tc.SetNoDelay(true)
			_ = tc.SetKeepAlive(true)
			_ = tc.SetKeepAlivePeriod(45 * time.Second)
		}
	}

	pc := &peerConn{
		addr:       addr,
		self:       self,
		conn:       c,
		r:          bufio.NewReaderSize(c, 64<<10),
		w:          bufio.NewWriterSize(c, 64<<10),
		closed:     make(chan struct{}),
		maxFrame:   maxFrame,
		readTO:     readTO,
		writeTO:    writeTO,
		idleTO:     idleTO,
		inflightCh: make(chan struct{}, inflight),
		token:      token,
	}
	if token != "" {
		if err := pc.hello(); err != nil {
			_ = c.Close()
			return nil, err
		}
	}
	// start the demultiplexing reader: one goroutine reads frames and routes
	// them to the waiting requester channel keyed by Base.ID.
	go pc.readLoop()
	return pc, nil
}

func (p *peerConn) hello() error {
	id := uint64(time.Now().UnixNano())
	msg := &MsgHello{Base: Base{T: MTHello, ID: id}, From: p.self, Token: p.token}
	raw, err := cbor.Marshal(msg)
	if err != nil {
		return err
	}
	if err := p.writeFrame(raw); err != nil {
		return err
	}

	respRaw, err := p.readFrame()
	if err != nil {
		return err
	}

	var base Base
	if err := cbor.Unmarshal(respRaw, &base); err != nil {
		return err
	}
	if base.T != MTHelloResp {
		return errors.New("bad hello resp")
	}

	var hr MsgHelloResp
	if err := cbor.Unmarshal(respRaw, &hr); err != nil {
		return err
	}
	if !hr.OK {
		if hr.Err == "" {
			hr.Err = "unauthorized"
		}
		return errors.New(hr.Err)
	}
	return nil
}

func (p *peerConn) close() {
	_ = p.conn.Close()
	select {
	case <-p.closed:
	default:
		close(p.closed)
	}
}

func (p *peerConn) failAll(err error) {
	// notify all pending requests that the connection failed.
	p.pend.Range(func(_, chAny any) bool {
		if ch, ok := chAny.(chan []byte); ok {
			// close channel so request() unblocks and returns "peer closed".
			close(ch)
		}
		return true
	})
	p.close()
}

// readLoop continuously reads frames and unblocks waiters with matching IDs.
func (p *peerConn) readLoop() {
	for {
		buf, err := p.readFrame()
		if err != nil {
			p.failAll(err)
			return
		}
		var base Base
		if err := cbor.Unmarshal(buf, &base); err != nil {
			continue
		}
		if chAny, ok := p.pend.Load(base.ID); ok {
			p.pend.Delete(base.ID)
			ch := chAny.(chan []byte)
			ch <- buf
			close(ch)
		}
	}
}

func (p *peerConn) readFrame() ([]byte, error) {
	_ = p.conn.SetReadDeadline(time.Now().Add(p.readTO))
	var hdr [4]byte
	if _, err := io.ReadFull(p.r, hdr[:]); err != nil {
		return nil, err
	}

	n := int(binary.BigEndian.Uint32(hdr[:]))
	if p.maxFrame > 0 && n > p.maxFrame {
		return nil, errors.New("frame too large")
	}

	buf := make([]byte, n)
	if _, err := io.ReadFull(p.r, buf); err != nil {
		return nil, err
	}
	_ = p.conn.SetReadDeadline(time.Now().Add(p.idleTO))
	return buf, nil
}

func (p *peerConn) writeFrame(payload []byte) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	_ = p.conn.SetWriteDeadline(time.Now().Add(p.writeTO))
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(payload)))
	if _, err := p.w.Write(hdr[:]); err != nil {
		return err
	}
	if _, err := p.w.Write(payload); err != nil {
		return err
	}
	return p.w.Flush()
}

func writeFrameBuf(w *bufio.Writer, payload []byte) error {
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(payload)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	if _, err := w.Write(payload); err != nil {
		return err
	}
	return w.Flush()
}

func writeFrame(w io.Writer, payload []byte) error {
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(payload)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	_, err := w.Write(payload)
	return err
}

func (p *peerConn) request(msg any, id uint64, timeout time.Duration) ([]byte, error) {
	select {
	case p.inflightCh <- struct{}{}:
	default:
		return nil, errors.New("peer inflight limit")
	}
	defer func() { <-p.inflightCh }()

	sel, err := cbor.Marshal(msg)
	if err != nil {
		return nil, err
	}
	// each request registers a one-shot channel under its ID; readLoop
	// delivers the response or request times out and cleans up the slot.
	ch := make(chan []byte, 1)
	p.pend.Store(id, ch)

	if err := p.writeFrame(sel); err != nil {
		p.pend.Delete(id)
		return nil, err
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case resp, ok := <-ch:
		if !ok {
			return nil, ErrPeerClosed
		}
		return resp, nil
	case <-timer.C:
		p.pend.Delete(id)
		p.penalizeTimeout() // backoff on repeated timeouts
		return nil, ErrTimeout
	}
}

// penalizeTimeout bumps a short penalty - repeated timeouts within backoffWindow
// grow the penalty (2s → 4s → 8s), capped by penaltyMax. O(1), timeout-path only.
func (p *peerConn) penalizeTimeout() {
	now := time.Now()
	last := time.Unix(0, atomic.LoadInt64(&p.lastTimeout))
	var streak uint32
	if now.Sub(last) > backoffWindow {
		// stale last-timeout: reset streak to 1
		atomic.StoreUint32(&p.toStreak, 1)
		streak = 1
	} else {
		// same window: increment
		streak = atomic.AddUint32(&p.toStreak, 1)
	}
	atomic.StoreInt64(&p.lastTimeout, now.UnixNano())

	// penalty = base << (streak-1), capped
	shift := streak - 1
	if shift > 3 { // 2s<<3 = 16s
		shift = 3
	}

	d := penaltyBase << shift
	if d > penaltyMax {
		d = penaltyMax
	}
	atomic.StoreInt64(&p.penaltyUntil, now.Add(d).UnixNano())
}

// penalized reports whether the peer is currently under penalty.
func (p *peerConn) penalized() bool {
	return time.Now().UnixNano() < atomic.LoadInt64(&p.penaltyUntil)
}
