package cluster

import (
	"bufio"
	"compress/gzip"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"io"
	"net"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	cache "github.com/unkn0wn-root/kioshun"

	"github.com/cespare/xxhash/v2"
	cbor "github.com/fxamacker/cbor/v2"
)

const ErrNotFound = "notfound"

var readBufPool = newBufPool([]int{
	1 << 10,  // 1 KiB
	2 << 10,  // 2 KiB
	4 << 10,  // 4 KiB
	8 << 10,  // 8 KiB
	16 << 10, // 16 KiB
	32 << 10, // 32 KiB
	64 << 10, // 64 KiB
})

var (
	cborEnc cbor.EncMode
	cborDec cbor.DecMode
)

func init() {
	em, _ := cbor.CanonicalEncOptions().EncMode()
	dm, _ := (cbor.DecOptions{}).DecMode()
	cborEnc, cborDec = em, dm
}

type tcpKeepAliveListener struct{ *net.TCPListener }

type Node[K comparable, V any] struct {
	cfg           Config
	kc            KeyCodec[K]
	codec         Codec[V]
	local         *cache.InMemoryCache[K, V]
	Loader        func(context.Context, K) (V, time.Duration, error)
	leaseLimiter  *rateLimiter
	leases        *leaseTable
	heat          *heat
	repl          *replicator[K, V]
	mem           *membership
	ring          atomic.Value
	hh            *handoff[K, V]
	hotMu         sync.RWMutex
	hotSet        map[string]int64
	verMu         sync.RWMutex
	version       map[string]uint64
	peersMu       sync.RWMutex
	peers         map[string]*peerConn
	tlsServerConf *tls.Config
	tlsClientConf *tls.Config
	reqID         uint64
	handshakeGate chan struct{}
	clock         *hlc
	stop          chan struct{}
}

func NewNode[K comparable, V any](cfg Config, keyc KeyCodec[K], c *cache.InMemoryCache[K, V], codec Codec[V]) *Node[K, V] {
	cfg.Handoff.FillDefaults()

	n := &Node[K, V]{
		cfg:     cfg,
		kc:      keyc,
		codec:   codec,
		local:   c,
		leases:  newLeaseTable(cfg.LeaseTTL),
		heat:    newHeat(4, 4096, cfg.HotsetSize, 16),
		mem:     newMembership(),
		peers:   make(map[string]*peerConn),
		stop:    make(chan struct{}),
		hotSet:  make(map[string]int64),
		version: make(map[string]uint64),
		clock:   newHLC(),
	}

	r := newRing(cfg.ReplicationFactor)
	n.ring.Store(r)
	n.repl = &replicator[K, V]{node: n}

	if cfg.Sec.LeaseLoadQPS > 0 {
		n.leaseLimiter = newRateLimiter(cfg.Sec.LeaseLoadQPS, time.Second)
	}
	if cfg.Sec.TLS.Enable {
		n.initTLS()
		lim := runtime.NumCPU() * 32
		if lim < 64 {
			lim = 64
		}
		n.handshakeGate = make(chan struct{}, lim)
	}

	if cfg.Handoff.IsEnabled() {
		n.hh = newHandoff[K, V](n)
	}

	return n
}

func (n *Node[K, V]) Start() error {
	ln, err := net.Listen("tcp", n.cfg.BindAddr)
	if err != nil {
		return err
	}
	go n.acceptLoop(ln)

	// proactively connect to seeds to accelerate gossip and ring formation.
	for _, s := range n.cfg.Seeds {
		if s != n.cfg.PublicURL {
			_ = n.ensurePeer(s)
		}
	}

	// fire!
	go n.gossipLoop()
	go n.weightLoop()
	go n.rebalancerLoop()
	go n.backfillLoop()

	return nil
}

func (n *Node[K, V]) Stop() {
	close(n.stop)
	if n.leaseLimiter != nil {
		n.leaseLimiter.Stop()
	}
	if n.leases != nil {
		n.leases.Stop()
	}
	if n.hh != nil {
		n.hh.Stop()
	}
	n.closePeers()
}

func (n *Node[K, V]) initTLS() {
	loadCertPool := func(p string) *x509.CertPool {
		if p == "" {
			return nil
		}
		ca := x509.NewCertPool()
		if pem, err := os.ReadFile(p); err == nil {
			ca.AppendCertsFromPEM(pem)
			return ca
		}
		return nil
	}

	if cert, err := tls.LoadX509KeyPair(n.cfg.Sec.TLS.CertFile, n.cfg.Sec.TLS.KeyFile); err == nil {
		tc := &tls.Config{
			Certificates:             []tls.Certificate{cert},
			MinVersion:               tls.VersionTLS12,
			PreferServerCipherSuites: true,
			CipherSuites: []uint16{
				tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
				tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
				tls.TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305,
				tls.TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305,
			},
			CurvePreferences: []tls.CurveID{tls.X25519, tls.CurveP256},
		}
		if n.cfg.Sec.TLS.RequireClientCert {
			tc.ClientAuth = tls.RequireAndVerifyClientCert
			tc.ClientCAs = loadCertPool(n.cfg.Sec.TLS.CAFile)
		}
		n.tlsServerConf = tc
	}

	cc := &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: loadCertPool(n.cfg.Sec.TLS.CAFile)}
	if n.cfg.Sec.TLS.CertFile != "" && n.cfg.Sec.TLS.KeyFile != "" {
		if cert, err := tls.LoadX509KeyPair(n.cfg.Sec.TLS.CertFile, n.cfg.Sec.TLS.KeyFile); err == nil {
			cc.Certificates = []tls.Certificate{cert}
		}
	}
	n.tlsClientConf = cc
}

func (n *Node[K, V]) nextReqID() uint64 {
	return atomic.AddUint64(&n.reqID, 1)
}

func (n *Node[K, V]) closePeers() {
	n.peersMu.Lock()
	defer n.peersMu.Unlock()
	for _, p := range n.peers {
		p.close()
	}
	n.peers = make(map[string]*peerConn)
}

func (n *Node[K, V]) getPeer(addr string) *peerConn {
	n.peersMu.RLock()
	p := n.peers[addr]
	n.peersMu.RUnlock()
	return p
}

func (n *Node[K, V]) acceptLoop(ln net.Listener) {
	tune := func(tc *net.TCPConn) {
		_ = tc.SetNoDelay(true)
		_ = tc.SetKeepAlive(true)
		_ = tc.SetKeepAlivePeriod(45 * time.Second)
	}

	for {
		c, err := ln.Accept()
		if err != nil {
			select {
			case <-n.stop:
				return
			default:
				continue
			}
		}
		if tc, ok := c.(*net.TCPConn); ok {
			tune(tc)
		}
		go n.serveConn(c)
	}
}

func (n *Node[K, V]) serveConn(c net.Conn) {
	defer c.Close()

	if n.cfg.Sec.TLS.Enable && n.tlsServerConf != nil {
		if n.handshakeGate != nil {
			n.handshakeGate <- struct{}{}
			defer func() { <-n.handshakeGate }()
		}

		t := tls.Server(c, n.tlsServerConf)
		if rt := n.cfg.Sec.ReadTimeout; rt > 0 {
			_ = t.SetDeadline(time.Now().Add(rt))
		}

		if err := t.Handshake(); err != nil {
			_ = t.Close()
			return
		}
		_ = t.SetDeadline(time.Time{})
		c = t
	}

	rb := n.cfg.Sec.ReadBufSize
	if rb <= 0 {
		rb = 32 << 10
	}

	wb := n.cfg.Sec.WriteBufSize
	if wb <= 0 {
		wb = 32 << 10
	}

	r := bufio.NewReaderSize(c, rb)
	w := bufio.NewWriterSize(c, wb)

	if n.cfg.Sec.AuthToken != "" {
		if rt := n.cfg.Sec.ReadTimeout; rt > 0 {
			_ = c.SetReadDeadline(time.Now().Add(rt))
		}

		var hdr [4]byte
		if _, err := io.ReadFull(r, hdr[:]); err != nil {
			return
		}

		nbytes := int(binary.BigEndian.Uint32(hdr[:]))
		if n.cfg.Sec.MaxFrameSize > 0 && nbytes > n.cfg.Sec.MaxFrameSize {
			return
		}

		buf := readBufPool.get(nbytes)
		if _, err := io.ReadFull(r, buf[:nbytes]); err != nil {
			readBufPool.put(buf)
			return
		}

		if idle := n.cfg.Sec.IdleTimeout; idle > 0 {
			_ = c.SetReadDeadline(time.Now().Add(idle))
		} else {
			_ = c.SetReadDeadline(time.Time{})
		}

		var base Base
		if err := cborDec.Unmarshal(buf[:nbytes], &base); err != nil || base.T != MTHello {
			readBufPool.put(buf)
			return
		}

		var h MsgHello
		authOK := cborDec.Unmarshal(buf[:nbytes], &h) == nil && h.Token == n.cfg.Sec.AuthToken
		readBufPool.put(buf)

		ack := MsgHelloResp{Base: Base{T: MTHelloResp, ID: base.ID}, OK: authOK}
		if !authOK {
			ack.Err = "unauthorized"
		}

		raw, _ := cborEnc.Marshal(&ack)
		if wt := n.cfg.Sec.WriteTimeout; wt > 0 {
			_ = c.SetWriteDeadline(time.Now().Add(wt))
		}

		if err := writeFrameBuf(w, raw); err != nil || !authOK {
			return
		}
	}

	// Per-connection worker pool - incoming frames are queued and processed
	// concurrently up to PerConnWorkers with backpressure on the channel.
	workers := n.cfg.PerConnWorkers
	if workers <= 0 {
		workers = 64
	}

	qlen := n.cfg.PerConnQueue
	if qlen <= 0 {
		qlen = workers * 2
	}

	jobQ := make(chan []byte, qlen)
	defer close(jobQ)

	var writeMu sync.Mutex
	writeResp := func(payload []byte) {
		if payload == nil {
			return
		}
		if wt := n.cfg.Sec.WriteTimeout; wt > 0 {
			_ = c.SetWriteDeadline(time.Now().Add(wt))
		}
		writeMu.Lock()
		_ = writeFrameBuf(w, payload)
		writeMu.Unlock()
	}

	// workers: decode → handle → encode → write → recycle buf
	for i := 0; i < workers; i++ {
		go func() {
			for buf := range jobQ {
				// buf length is exactly the frame body
				var base Base
				if err := cborDec.Unmarshal(buf, &base); err != nil {
					readBufPool.put(buf)
					continue
				}

				send := func(v any) {
					out, _ := cborEnc.Marshal(v)
					writeResp(out)
				}

				switch base.T {
				case MTGet:
					var g MsgGet
					if cborDec.Unmarshal(buf, &g) == nil {
						send(n.rpcGet(g))
					}
				case MTGetBulk:
					var g MsgGetBulk
					if cborDec.Unmarshal(buf, &g) == nil {
						send(n.rpcGetBulk(g))
					}
				case MTSet:
					var s MsgSet
					if cborDec.Unmarshal(buf, &s) == nil {
						send(n.rpcSet(s))
					}
				case MTSetBulk:
					var sb MsgSetBulk
					if cborDec.Unmarshal(buf, &sb) == nil {
						send(n.rpcSetBulk(sb))
					}
				case MTDelete:
					var d MsgDel
					if cborDec.Unmarshal(buf, &d) == nil {
						send(n.rpcDel(d))
					}
				case MTLeaseLoad:
					var ll MsgLeaseLoad
					if cborDec.Unmarshal(buf, &ll) == nil {
						send(n.rpcLeaseLoad(ll))
					}
				case MTGossip:
					var g MsgGossip
					if cborDec.Unmarshal(buf, &g) == nil {
						n.ingestGossip(&g)
						ack := MsgGossip{Base: Base{T: MTGossip, ID: g.ID}}
						send(&ack)
					}
				case MTBackfillDigestReq:
					var q MsgBackfillDigestReq
					if cborDec.Unmarshal(buf, &q) == nil {
						send(n.rpcBackfillDigest(q))
					}
				case MTBackfillKeysReq:
					var q MsgBackfillKeysReq
					if cborDec.Unmarshal(buf, &q) == nil {
						send(n.rpcBackfillKeys(q))
					}
				}
				readBufPool.put(buf)
			}
		}()
	}

	idle := n.cfg.Sec.IdleTimeout
	if idle <= 0 {
		idle = n.cfg.Sec.ReadTimeout
	}

	for {
		if idle > 0 {
			_ = c.SetReadDeadline(time.Now().Add(idle)) // waiting for next frame
		}

		var hdr [4]byte
		if _, err := io.ReadFull(r, hdr[:]); err != nil {
			return
		}

		nbytes := int(binary.BigEndian.Uint32(hdr[:]))
		if n.cfg.Sec.MaxFrameSize > 0 && nbytes > n.cfg.Sec.MaxFrameSize {
			return
		}

		if rt := n.cfg.Sec.ReadTimeout; rt > 0 {
			_ = c.SetReadDeadline(time.Now().Add(rt)) // active body read
		}

		buf := readBufPool.get(nbytes)
		if _, err := io.ReadFull(r, buf[:nbytes]); err != nil {
			readBufPool.put(buf)
			return
		}

		if idle > 0 {
			_ = c.SetReadDeadline(time.Now().Add(idle)) // back to idle window
		}

		// backpressure: enqueue for workers (blocks when saturated so TCP
		// naturally applies flow control to the client).
		jobQ <- buf[:nbytes]
	}
}

func (n *Node[K, V]) ownersFor(key K) []*nodeMeta {
	var keyHash uint64
	if kh, ok := any(n.kc).(KeyHasher[K]); ok {
		keyHash = kh.Hash64(key)
	} else {
		keyHash = xxhash.Sum64(n.kc.EncodeKey(key))
	}

	r := n.ring.Load().(*ring)
	owners := r.ownersFromKeyHash(keyHash)

	bk := n.kc.EncodeKey(key)
	n.hotMu.RLock()
	expAt, hot := n.hotSet[string(bk)]
	n.hotMu.RUnlock()
	if hot && time.Now().UnixNano() < expAt {
		cands := r.ownersTopNFromKeyHash(keyHash, r.rf+3)
	outer:
		for _, cand := range cands {
			for _, ex := range owners {
				if ex.Addr == cand.Addr {
					continue outer
				}
			}
			owners = append(owners, cand)
			break
		}
	}
	return owners
}

type bytesBuffer struct{ b []byte }

func (bb *bytesBuffer) Write(p []byte) (int, error) { bb.b = append(bb.b, p...); return len(p), nil }
func (bb *bytesBuffer) Bytes() []byte               { return bb.b }
func (bb *bytesBuffer) Len() int                    { return len(bb.b) }
func (bb *bytesBuffer) ReadFrom(r io.Reader) (int64, error) {
	var total int64
	var tmp [32 << 10]byte
	for {
		n, err := r.Read(tmp[:])
		if n > 0 {
			bb.b = append(bb.b, tmp[:n]...)
			total += int64(n)
		}
		if err != nil {
			if err == io.EOF {
				return total, nil
			}
			return total, err
		}
	}
}

type bytesReader struct {
	b []byte
	i int
}

func (br *bytesReader) Read(p []byte) (int, error) {
	if br.i >= len(br.b) {
		return 0, io.EOF
	}
	n := copy(p, br.b[br.i:])
	br.i += n
	return n, nil
}

func (n *Node[K, V]) maybeCompress(b []byte) (out []byte, cp bool) {
	thr := n.cfg.Sec.CompressionThreshold
	if thr <= 0 || len(b) < thr {
		return b, false
	}

	var buf bytesBuffer
	zw, _ := gzip.NewWriterLevel(&buf, gzip.BestSpeed)
	_, _ = zw.Write(b)
	_ = zw.Close()

	if buf.Len() >= len(b) {
		return b, false
	}
	return buf.Bytes(), true
}
func (n *Node[K, V]) maybeDecompress(b []byte, cp bool) ([]byte, error) {
	if !cp {
		return b, nil
	}

	r, err := gzip.NewReader(&bytesReader{b: b})
	if err != nil {
		return nil, err
	}

	defer r.Close()
	var out bytesBuffer
	if _, err := out.ReadFrom(r); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func (n *Node[K, V]) rpcGet(g MsgGet) MsgGetResp {
	k, err := n.kc.DecodeKey(g.Key)
	if err != nil {
		return MsgGetResp{Base: Base{T: MTGetResp, ID: g.ID}, Err: err.Error()}
	}

	if v, ok := n.local.Get(k); ok {
		b, err := n.codec.Encode(v)
		if err != nil {
			return MsgGetResp{Base: Base{T: MTGetResp, ID: g.ID}, Err: err.Error()}
		}

		if n.cfg.Sec.MaxValueSize > 0 && len(b) > n.cfg.Sec.MaxValueSize {
			return MsgGetResp{Base: Base{T: MTGetResp, ID: g.ID}, Err: "value too large"}
		}
		b2, cp := n.maybeCompress(b)
		return MsgGetResp{Base: Base{T: MTGetResp, ID: g.ID}, Found: true, Val: b2, Cp: cp, Exp: 0}
	}
	return MsgGetResp{Base: Base{T: MTGetResp, ID: g.ID}, Found: false, Err: ErrNotFound}
}

func (n *Node[K, V]) rpcGetBulk(g MsgGetBulk) MsgGetBulkResp {
	vals := make([][]byte, len(g.Keys))
	hits := make([]bool, len(g.Keys))
	exps := make([]int64, len(g.Keys))
	cps := make([]bool, len(g.Keys))
	for i, kb := range g.Keys {
		k, err := n.kc.DecodeKey(kb)
		if err != nil {
			continue
		}
		if v, ok := n.local.Get(k); ok {
			b, _ := n.codec.Encode(v)
			if n.cfg.Sec.MaxValueSize > 0 && len(b) > n.cfg.Sec.MaxValueSize {
				continue
			}

			b2, cp := n.maybeCompress(b)
			vals[i], cps[i], hits[i], exps[i] = b2, cp, true, 0
		}
	}
	return MsgGetBulkResp{Base: Base{T: MTGetBulkResp, ID: g.ID}, Hits: hits, Vals: vals, Exps: exps, Cps: cps}
}

func (n *Node[K, V]) rpcSet(s MsgSet) MsgSetResp {
	if n.cfg.Sec.MaxKeySize > 0 && len(s.Key) > n.cfg.Sec.MaxKeySize {
		return MsgSetResp{Base: Base{T: MTSetResp, ID: s.ID}, OK: false, Err: "key too large"}
	}

	if n.cfg.Sec.MaxValueSize > 0 && len(s.Val) > n.cfg.Sec.MaxValueSize {
		return MsgSetResp{Base: Base{T: MTSetResp, ID: s.ID}, OK: false, Err: "value too large"}
	}

	vbytes, err := n.maybeDecompress(s.Val, s.Cp)
	if err != nil {
		return MsgSetResp{Base: Base{T: MTSetResp, ID: s.ID}, OK: false, Err: err.Error()}
	}

	if n.cfg.Sec.MaxValueSize > 0 && len(vbytes) > n.cfg.Sec.MaxValueSize {
		return MsgSetResp{Base: Base{T: MTSetResp, ID: s.ID}, OK: false, Err: "value too large"}
	}

	k, err := n.kc.DecodeKey(s.Key)
	if err != nil {
		return MsgSetResp{Base: Base{T: MTSetResp, ID: s.ID}, OK: false, Err: err.Error()}
	}

	if n.cfg.LWWEnabled {
		keyStr := string(s.Key)
		n.verMu.RLock()
		old := n.version[keyStr]
		n.verMu.RUnlock()
		// Ddrop older versions (LWW); equal version is idempotent.
		if old > s.Ver {
			return MsgSetResp{Base: Base{T: MTSetResp, ID: s.ID}, OK: true}
		}
	}

	val, err := n.codec.Decode(vbytes)
	if err != nil {
		return MsgSetResp{Base: Base{T: MTSetResp, ID: s.ID}, OK: false, Err: err.Error()}
	}

	n.local.Import([]cache.Item[K, V]{{
		Key:       k,
		Val:       val,
		ExpireAbs: s.Exp,
		Version:   s.Ver,
	}})

	if n.cfg.LWWEnabled {
		n.verMu.Lock()
		n.version[string(s.Key)] = s.Ver
		n.verMu.Unlock()
		n.clock.Observe(s.Ver)
	}
	return MsgSetResp{Base: Base{T: MTSetResp, ID: s.ID}, OK: true}
}

func (n *Node[K, V]) rpcSetBulk(sb MsgSetBulk) MsgSetBulkResp {
	items := make([]cache.Item[K, V], 0, len(sb.Items))
	for _, kv := range sb.Items {
		if n.cfg.Sec.MaxKeySize > 0 && len(kv.K) > n.cfg.Sec.MaxKeySize {
			return MsgSetBulkResp{Base: Base{T: MTSetBulkResp, ID: sb.ID}, OK: false, Err: "key too large"}
		}

		if n.cfg.Sec.MaxValueSize > 0 && len(kv.V) > n.cfg.Sec.MaxValueSize {
			return MsgSetBulkResp{Base: Base{T: MTSetBulkResp, ID: sb.ID}, OK: false, Err: "value too large"}
		}

		k, err := n.kc.DecodeKey(kv.K)
		if err != nil {
			return MsgSetBulkResp{Base: Base{T: MTSetBulkResp, ID: sb.ID}, OK: false, Err: err.Error()}
		}

		vbytes, err := n.maybeDecompress(kv.V, kv.Cp)
		if err != nil {
			return MsgSetBulkResp{Base: Base{T: MTSetBulkResp, ID: sb.ID}, OK: false, Err: err.Error()}
		}

		if n.cfg.Sec.MaxValueSize > 0 && len(vbytes) > n.cfg.Sec.MaxValueSize {
			return MsgSetBulkResp{Base: Base{T: MTSetBulkResp, ID: sb.ID}, OK: false, Err: "value too large"}
		}

		val, err := n.codec.Decode(vbytes)
		if err != nil {
			return MsgSetBulkResp{Base: Base{T: MTSetBulkResp, ID: sb.ID}, OK: false, Err: err.Error()}
		}

		if n.cfg.LWWEnabled {
			n.verMu.RLock()
			old := n.version[string(kv.K)]
			n.verMu.RUnlock()
			if old > kv.Ver {
				continue
			}
		}

		items = append(items, cache.Item[K, V]{Key: k, Val: val, ExpireAbs: kv.E, Version: kv.Ver})
		if n.cfg.LWWEnabled {
			n.verMu.Lock()
			n.version[string(kv.K)] = kv.Ver
			n.verMu.Unlock()
			n.clock.Observe(kv.Ver)
		}
	}
	n.local.Import(items)
	return MsgSetBulkResp{Base: Base{T: MTSetBulkResp, ID: sb.ID}, OK: true}
}

func (n *Node[K, V]) rpcDel(d MsgDel) MsgDelResp {
	if n.cfg.Sec.MaxKeySize > 0 && len(d.Key) > n.cfg.Sec.MaxKeySize {
		return MsgDelResp{Base: Base{T: MTDeleteResp, ID: d.ID}, OK: false, Err: "key too large"}
	}

	k, err := n.kc.DecodeKey(d.Key)
	if err != nil {
		return MsgDelResp{Base: Base{T: MTDeleteResp, ID: d.ID}, OK: false, Err: err.Error()}
	}

	if n.cfg.LWWEnabled {
		n.verMu.RLock()
		old := n.version[string(d.Key)]
		n.verMu.RUnlock()
		if old > d.Ver {
			return MsgDelResp{Base: Base{T: MTDeleteResp, ID: d.ID}, OK: true}
		}
		n.verMu.Lock()
		n.version[string(d.Key)] = d.Ver
		n.verMu.Unlock()
		n.clock.Observe(d.Ver)
	}
	ok := n.local.Delete(k)
	return MsgDelResp{Base: Base{T: MTDeleteResp, ID: d.ID}, OK: ok}
}

func (n *Node[K, V]) rpcLeaseLoad(ll MsgLeaseLoad) MsgLeaseLoadResp {
	if n.leaseLimiter != nil && !n.leaseLimiter.Allow() {
		return MsgLeaseLoadResp{Base: Base{T: MTLeaseLoadResp, ID: ll.ID}, Err: "rate limited"}
	}

	k, err := n.kc.DecodeKey(ll.Key)
	if err != nil {
		return MsgLeaseLoadResp{Base: Base{T: MTLeaseLoadResp, ID: ll.ID}, Err: err.Error()}
	}

	if v, ok := n.local.Get(k); ok {
		b, _ := n.codec.Encode(v)
		b2, cp := n.maybeCompress(b)
		return MsgLeaseLoadResp{Base: Base{T: MTLeaseLoadResp, ID: ll.ID}, Found: true, Val: b2, Cp: cp, Exp: 0}
	}

	if n.Loader == nil {
		return MsgLeaseLoadResp{Base: Base{T: MTLeaseLoadResp, ID: ll.ID}, Err: ErrNoLoader.Error()}
	}

	keyStr := string(ll.Key)
	_, acquired := n.leases.acquire(keyStr)
	if acquired {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		v, ttl, err := n.Loader(ctx, k)
		if err == nil {
			_ = n.local.Set(k, v, ttl)
			owners := n.ownersFor(k)
			bv, _ := n.codec.Encode(v)
			exp := int64(0)
			if ttl > 0 {
				exp = time.Now().Add(ttl).UnixNano()
			}
			ver := n.clock.Next()
			_ = n.repl.replicateSet(ctx, ll.Key, bv, exp, ver, owners)
			if n.cfg.LWWEnabled {
				n.verMu.Lock()
				n.version[keyStr] = ver
				n.verMu.Unlock()
			}
		}

		n.leases.release(keyStr, err)
		if err != nil {
			return MsgLeaseLoadResp{Base: Base{T: MTLeaseLoadResp, ID: ll.ID}, Err: err.Error()}
		}

		b, _ := n.codec.Encode(v)
		b2, cp := n.maybeCompress(b)
		return MsgLeaseLoadResp{Base: Base{T: MTLeaseLoadResp, ID: ll.ID}, Found: true, Val: b2, Cp: cp, Exp: 0}
	}

	if err := n.leases.wait(context.Background(), keyStr); err != nil {
		return MsgLeaseLoadResp{Base: Base{T: MTLeaseLoadResp, ID: ll.ID}, Err: err.Error()}
	}

	if v, ok := n.local.Get(k); ok {
		b, _ := n.codec.Encode(v)
		b2, cp := n.maybeCompress(b)
		return MsgLeaseLoadResp{Base: Base{T: MTLeaseLoadResp, ID: ll.ID}, Found: true, Val: b2, Cp: cp, Exp: 0}
	}
	return MsgLeaseLoadResp{Base: Base{T: MTLeaseLoadResp, ID: ll.ID}, Found: false}
}

func (n *Node[K, V]) sampleLoad() NodeLoad {
	st := n.local.Stats()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	free := uint64(0)
	if ms.Sys > ms.HeapAlloc {
		free = uint64(ms.Sys - ms.HeapAlloc)
	}
	return NodeLoad{Size: st.Size, Evictions: st.Evictions, FreeMemBytes: free, CPUu16: 0}
}

func (n *Node[K, V]) gossipLoop() {
	ticker := time.NewTicker(n.cfg.GossipInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			n.sendGossip()
		case <-n.stop:
			return
		}
	}
}

func (n *Node[K, V]) sendGossip() {
	_, seen, epoch := n.mem.snapshot()
	seenStr := make(map[string]int64, len(seen))
	for id, ts := range seen {
		seenStr[string(id)] = ts
	}
	ld := n.sampleLoad()
	top := n.heat.exportTopK()
	msg := &MsgGossip{Base: Base{
		T:  MTGossip,
		ID: n.nextReqID()},
		From:  n.cfg.PublicURL,
		Seen:  seenStr,
		Peers: n.peerAddrs(),
		Load:  ld,
		TopK:  top,
		Epoch: epoch,
	}

	n.peersMu.RLock()
	defer n.peersMu.RUnlock()
	for addr, pc := range n.peers {
		if pc == nil || addr == n.cfg.PublicURL {
			continue
		}
		_, _ = pc.request(msg, msg.ID, 1500*time.Millisecond)
	}
}

func (n *Node[K, V]) peerAddrs() []string {
	n.peersMu.RLock()
	defer n.peersMu.RUnlock()
	out := make([]string, 0, len(n.peers))
	for a := range n.peers {
		out = append(out, a)
	}
	return out
}

func (n *Node[K, V]) ingestGossip(g *MsgGossip) {
	now := time.Now().UnixNano()
	n.mem.ensure(NodeID(g.From), g.From)
	n.mem.integrate(NodeID(g.From), g.From, g.Peers, g.Seen, g.Epoch, now)
	if meta, ok := n.mem.peers[NodeID(g.From)]; ok {
		atomic.StoreUint64(&meta.weight, computeWeight(g.Load))
	}

	if n.cfg.MirrorTTL > 0 {
		exp := now + n.cfg.MirrorTTL.Nanoseconds()
		n.hotMu.Lock()
		for _, hk := range g.TopK {
			n.hotSet[string(hk.K)] = exp
		}
		n.hotMu.Unlock()
	}
}

func (n *Node[K, V]) weightLoop() {
	t := time.NewTicker(n.cfg.WeightUpdate)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			now := time.Now()
			alive := n.mem.alive(now.UnixNano(), n.cfg.SuspicionAfter)
			r := newRing(n.cfg.ReplicationFactor)
			r.nodes = alive
			n.ring.Store(r)
			for _, m := range alive {
				if m.Addr != "" && m.Addr != n.cfg.PublicURL {
					_ = n.ensurePeer(m.Addr)
				}
			}
			n.cleanupHot(now.UnixNano())
			n.mem.pruneTombstones(now.UnixNano(), n.cfg.TombstoneAfter)
		case <-n.stop:
			return
		}
	}
}

func (n *Node[K, V]) cleanupHot(now int64) {
	n.hotMu.Lock()
	for k, exp := range n.hotSet {
		if now >= exp {
			delete(n.hotSet, k)
		}
	}
	n.hotMu.Unlock()
}

func (n *Node[K, V]) ensurePeer(addr string) *peerConn {
	n.peersMu.RLock()
	p := n.peers[addr]
	n.peersMu.RUnlock()
	if p != nil {
		return p
	}

	n.peersMu.Lock()
	defer n.peersMu.Unlock()
	if p = n.peers[addr]; p != nil {
		return p
	}

	var tlsConf *tls.Config
	if n.cfg.Sec.TLS.Enable {
		tlsConf = n.tlsClientConf
	}
	// establish transport with auth/TLS and configure inflight limits.
	pc, err := dialPeer(
		n.cfg.PublicURL,
		addr,
		tlsConf,
		n.cfg.Sec.MaxFrameSize,
		n.cfg.Sec.ReadTimeout,
		n.cfg.Sec.WriteTimeout,
		n.cfg.Sec.IdleTimeout,
		n.cfg.Sec.MaxInflightPerPeer,
		n.cfg.Sec.AuthToken,
	)
	if err != nil {
		return nil
	}
	n.peers[addr] = pc
	n.mem.ensure(NodeID(addr), addr)
	return pc
}

func (n *Node[K, V]) resetPeer(addr string) {
	n.peersMu.Lock()
	if p, ok := n.peers[addr]; ok && p != nil {
		p.close()
		delete(n.peers, addr)
	}
	n.peersMu.Unlock()
}
