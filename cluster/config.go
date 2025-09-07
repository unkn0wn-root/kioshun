package cluster

import (
	"crypto/tls"
	"fmt"
	"time"

	"github.com/cespare/xxhash/v2"
)

type NodeID string

type TLSMode struct {
	Enable                   bool
	CertFile                 string
	KeyFile                  string
	CAFile                   string
	RequireClientCert        bool
	MinVersion               uint16
	PreferServerCipherSuites bool
	CipherSuites             []uint16
	CurvePreferences         []tls.CurveID
}

type Security struct {
	AuthToken                   string
	TLS                         TLSMode
	MaxFrameSize                int
	MaxKeySize                  int
	MaxValueSize                int
	ReadTimeout                 time.Duration
	WriteTimeout                time.Duration
	IdleTimeout                 time.Duration
	MaxInflightPerPeer          int
	CompressionThreshold        int
	LeaseLoadQPS                int
	ReadBufSize                 int
	WriteBufSize                int
	AllowUnauthenticatedClients bool
}

type DropPolicy uint8

const (
	DropOldest DropPolicy = iota
	DropNewest
	DropNone
)

type HandoffConfig struct {
	Enable         *bool
	Pause          bool
	MaxItems       int
	MaxBytes       int64
	PerPeerCap     int
	PerPeerBytes   int64
	TTL            time.Duration
	ReplayRPS      int
	DropPolicy     DropPolicy
	AutopauseItems int
	AutopauseBytes int64
}

func (h *HandoffConfig) IsEnabled() bool {
	return h.Enable == nil || *h.Enable
}

func (h *HandoffConfig) FillDefaults() {
	if h.Enable == nil {
		b := true
		h.Enable = &b
	}
	if !*h.Enable {
		return
	}
	if h.DropPolicy == 0 {
		h.DropPolicy = DropOldest
	}
	if h.MaxItems == 0 {
		h.MaxItems = 500_000
	}
	if h.MaxBytes == 0 {
		h.MaxBytes = 2 << 30 // ~2 GiB
	}
	if h.PerPeerCap == 0 {
		h.PerPeerCap = 50_000
	}
	if h.PerPeerBytes == 0 {
		h.PerPeerBytes = 512 << 20 // ~512 MiB
	}
	if h.TTL <= 0 {
		h.TTL = 10 * time.Minute
	}
	if h.ReplayRPS <= 0 {
		h.ReplayRPS = 20_000
	}
	if h.AutopauseItems == 0 && h.MaxItems > 0 {
		h.AutopauseItems = h.MaxItems * 9 / 10
	}
	if h.AutopauseBytes == 0 && h.MaxBytes > 0 {
		h.AutopauseBytes = int64(h.MaxBytes * 9 / 10)
	}
}

func BoolPtr(b bool) *bool { return &b }

type Config struct {
	ID                NodeID
	BindAddr          string
	PublicURL         string
	Seeds             []string
	ReplicationFactor int
	WriteConcern      int
	// Client read tuning
	ReadMaxFanout     int
	ReadHedgeDelay    time.Duration
	ReadHedgeInterval time.Duration
	ReadPerTryTimeout time.Duration
	GossipInterval    time.Duration
	SuspicionAfter    time.Duration
	TombstoneAfter    time.Duration
	WeightUpdate      time.Duration
	HotsetPeriod      time.Duration
	HotsetSize        int
	MirrorTTL         time.Duration
	LeaseTTL          time.Duration
	RebalanceInterval time.Duration
	BackfillInterval  time.Duration
	RebalanceLimit    int
	Sec               Security
	LWWEnabled        bool
	PerConnWorkers    int
	PerConnQueue      int

	Handoff HandoffConfig
}

func Default() Config {
	return Config{
		ReplicationFactor: 2,
		WriteConcern:      1,
		ReadMaxFanout:     2,
		ReadHedgeDelay:    3 * time.Millisecond,
		ReadHedgeInterval: 3 * time.Millisecond,
		ReadPerTryTimeout: 200 * time.Millisecond,
		GossipInterval:    500 * time.Millisecond,
		SuspicionAfter:    2 * time.Second,
		TombstoneAfter:    30 * time.Second,
		WeightUpdate:      1 * time.Second,
		HotsetPeriod:      2 * time.Second,
		HotsetSize:        1024,
		MirrorTTL:         30 * time.Second,
		LeaseTTL:          300 * time.Millisecond,
		RebalanceInterval: 2 * time.Second,
		BackfillInterval:  30 * time.Second,
		RebalanceLimit:    500,
		Sec: Security{
			MaxFrameSize:         4 << 20,
			MaxKeySize:           128 << 10,
			MaxValueSize:         2 << 20,
			ReadTimeout:          3 * time.Second,
			WriteTimeout:         3 * time.Second,
			IdleTimeout:          10 * time.Second,
			MaxInflightPerPeer:   256,
			CompressionThreshold: 64 << 10,
			LeaseLoadQPS:         0,
			ReadBufSize:          32 << 10,
			WriteBufSize:         32 << 10,
			TLS: TLSMode{
				PreferServerCipherSuites: true,
			},
			AllowUnauthenticatedClients: true,
		},
		PerConnWorkers: 64,
		PerConnQueue:   128,
		LWWEnabled:     true,

		Handoff: HandoffConfig{
			Enable:         BoolPtr(true),
			Pause:          false,
			MaxItems:       500_000,
			MaxBytes:       2 << 30,
			PerPeerCap:     50_000,
			PerPeerBytes:   512 << 20,
			TTL:            10 * time.Minute,
			ReplayRPS:      20_000,
			DropPolicy:     DropOldest,
			AutopauseItems: 500_000 * 9 / 10,
			AutopauseBytes: int64((2 << 30) * 9 / 10),
		},
	}
}

// EnsureID assigns a stable ID when not provided.
// Default: 16-hex digest of PublicURL.
func (c *Config) EnsureID() {
	if c.ID != "" {
		return
	}
	sum := xxhash.Sum64String(c.PublicURL)
	c.ID = NodeID(fmt.Sprintf("%016x", sum))
}
