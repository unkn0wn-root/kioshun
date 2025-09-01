package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	cache "github.com/unkn0wn-root/kioshun"
	"github.com/unkn0wn-root/kioshun/cluster"
)

func main() {
	var (
		bind   = flag.String("bind", ":5011", "listen address, e.g. 0.0.0.0:5011")
		public = flag.String("public", "localhost:5011", "public address peers use to reach this node")
		seeds  = flag.String("seeds", "", "comma-separated seed peers (host:port)")
		rf     = flag.Int("rf", 2, "replication factor")
		wc     = flag.Int("wc", 1, "write concern (1..rf)")

		// security & limits
		authTok  = flag.String("auth", "", "optional shared token for peer handshake")
		maxFrame = flag.Int("maxframe", 4<<20, "max frame bytes")
		maxKey   = flag.Int("maxkey", 128<<10, "max key bytes")
		maxVal   = flag.Int("maxval", 2<<20, "max value bytes")
		readTO   = flag.Duration("readto", 3*time.Second, "read timeout per frame")
		writeTO  = flag.Duration("writeto", 3*time.Second, "write timeout per frame")
		idleTO   = flag.Duration("idleto", 10*time.Second, "idle timeout")
		inflight = flag.Int("inflight", 256, "max inflight per peer")
		compThr  = flag.Int("comp", 64<<10, "compression threshold (0=off)")
		llQPS    = flag.Int("llqps", 0, "lease-load QPS (0=unlimited)")

		// TLS
		tlsEnable = flag.Bool("tls", false, "enable TLS")
		tlsCert   = flag.String("tlscert", "", "server cert PEM")
		tlsKey    = flag.String("tlskey", "", "server key PEM")
		tlsCA     = flag.String("tlsca", "", "CA PEM for client verify / client dial")
		mtls      = flag.Bool("mtls", false, "require client cert (mTLS)")

		// gossip/lease/rebalance
		gossip     = flag.Duration("gossip", 500*time.Millisecond, "gossip interval")
		suspicion  = flag.Duration("suspect", 2*time.Second, "suspect after")
		tombstone  = flag.Duration("tomb", 30*time.Second, "tombstone prune after")
		leaseTTL   = flag.Duration("lease", 300*time.Millisecond, "lease-get TTL")
		hotset     = flag.Int("hotset", 1024, "top-k heat size")
		mirrorTTL  = flag.Duration("mirror", 30*time.Second, "hot-key extra-owner TTL")
		rebalance  = flag.Duration("rebalance", 2*time.Second, "rebalance interval")
		rebalanceN = flag.Int("rebalanceN", 500, "max keys checked per rebalance tick")

		// cluster execution
		connWorkers = flag.Int("conn-workers", 64, "per-connection worker goroutines")
		connQueue   = flag.Int("conn-queue", 128, "per-connection inbound queue length")
		lww         = flag.Bool("lww", true, "enable last-write-wins (HLC)")

		// hinted handoff
		hoffEnable   = flag.Bool("handoff", true, "enable hinted handoff")
		hoffPause    = flag.Bool("handoff-pause", false, "start with handoff paused (manual resume)")
		hoffRPS      = flag.Int("handoff-rps", 20000, "handoff replay rate (items/sec)")
		hoffTTL      = flag.Duration("handoff-ttl", 10*time.Minute, "handoff hint TTL")
		hoffMaxItems = flag.Int("handoff-max-items", 500000, "global max hinted items")
		hoffMaxBytes = flag.Int64("handoff-max-bytes", int64(2<<30), "global max hinted bytes")
		hoffPeerCap  = flag.Int("handoff-perpeer-items", 50000, "per-peer hinted items cap")
		hoffPeerBy   = flag.Int64("handoff-perpeer-bytes", int64(512<<20), "per-peer hinted bytes cap")
		hoffDrop     = flag.String("handoff-drop", "oldest", "drop policy when full: oldest|newest|none")
		hoffAutoI    = flag.Int("handoff-autopause-items", 0, "auto-pause backlog items threshold (0=derived)")
		hoffAutoB    = flag.Int64("handoff-autopause-bytes", 0, "auto-pause backlog bytes threshold (0=derived)")

		// loader
		enableLoader = flag.Bool("enable-loader", false, "enable example loader for missing keys (for testing)")
		loaderTTL    = flag.Duration("loader-ttl", 20*time.Second, "TTL used by example loader when enabled")
	)
	flag.Parse()

	cfg := cluster.Default()
	cfg.BindAddr = *bind
	cfg.PublicURL = *public
	if *seeds != "" {
		cfg.Seeds = splitCSV(*seeds)
	}
	cfg.ReplicationFactor = *rf
	cfg.WriteConcern = *wc

	cfg.GossipInterval = *gossip
	cfg.SuspicionAfter = *suspicion
	cfg.TombstoneAfter = *tombstone
	cfg.LeaseTTL = *leaseTTL
	cfg.HotsetSize = *hotset
	cfg.MirrorTTL = *mirrorTTL
	cfg.RebalanceInterval = *rebalance
	cfg.RebalanceLimit = *rebalanceN
	cfg.ID = cluster.NodeID(cfg.PublicURL)

	// execution
	cfg.PerConnWorkers = *connWorkers
	cfg.PerConnQueue = *connQueue
	cfg.LWWEnabled = *lww

	cfg.Sec.AuthToken = *authTok
	cfg.Sec.MaxFrameSize = *maxFrame
	cfg.Sec.MaxKeySize = *maxKey
	cfg.Sec.MaxValueSize = *maxVal
	cfg.Sec.ReadTimeout = *readTO
	cfg.Sec.WriteTimeout = *writeTO
	cfg.Sec.IdleTimeout = *idleTO
	cfg.Sec.MaxInflightPerPeer = *inflight
	cfg.Sec.CompressionThreshold = *compThr
	cfg.Sec.LeaseLoadQPS = *llQPS
	cfg.Sec.TLS.Enable = *tlsEnable
	cfg.Sec.TLS.CertFile = *tlsCert
	cfg.Sec.TLS.KeyFile = *tlsKey
	cfg.Sec.TLS.CAFile = *tlsCA
	cfg.Sec.TLS.RequireClientCert = *mtls

	if *tlsEnable && (*tlsCert == "" || *tlsKey == "") {
		log.Printf("[warn] TLS enabled but missing cert/key; inbound TLS will be disabled")
	}

	local := cache.NewWithDefaults[string, []byte]()

	kc := cluster.StringKeyCodec[string]{}
	vc := cluster.BytesCodec{}

	node := cluster.NewNode[string, []byte](cfg, kc, local, vc)

	if *enableLoader {
		log.Printf("[info] example Loader enabled (for testing only)")
		node.Loader = func(ctx context.Context, k string) ([]byte, time.Duration, error) {
			return []byte("value-for-" + k), *loaderTTL, nil
		}
	}

	// configure hinted handoff
	cfg.Handoff.Enable = cluster.BoolPtr(*hoffEnable)
	cfg.Handoff.Pause = *hoffPause
	cfg.Handoff.ReplayRPS = *hoffRPS
	cfg.Handoff.TTL = *hoffTTL
	cfg.Handoff.MaxItems = *hoffMaxItems
	cfg.Handoff.MaxBytes = *hoffMaxBytes
	cfg.Handoff.PerPeerCap = *hoffPeerCap
	cfg.Handoff.PerPeerBytes = *hoffPeerBy
	switch strings.ToLower(*hoffDrop) {
	case "oldest":
		cfg.Handoff.DropPolicy = cluster.DropOldest
	case "newest":
		cfg.Handoff.DropPolicy = cluster.DropNewest
	case "none":
		cfg.Handoff.DropPolicy = cluster.DropNone
	default:
		log.Printf("[warn] unknown handoff-drop=%q; defaulting to oldest", *hoffDrop)
		cfg.Handoff.DropPolicy = cluster.DropOldest
	}
	if *hoffAutoI > 0 {
		cfg.Handoff.AutopauseItems = *hoffAutoI
	}
	if *hoffAutoB > 0 {
		cfg.Handoff.AutopauseBytes = *hoffAutoB
	}

	if err := node.Start(); err != nil {
		log.Fatalf("start: %v", err)
	}

	log.Printf("Kioshun node up at %s (bind %s) | rf=%d wc=%d lww=%v tls=%v mtls=%v handoff=%v rps=%d",
		cfg.PublicURL, cfg.BindAddr, cfg.ReplicationFactor, cfg.WriteConcern, cfg.LWWEnabled, cfg.Sec.TLS.Enable, cfg.Sec.TLS.RequireClientCert, cfg.Handoff.IsEnabled(), cfg.Handoff.ReplayRPS)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh
	log.Println("shutting down...")
	node.Stop()
	log.Println("bye.")
}

func splitCSV(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
