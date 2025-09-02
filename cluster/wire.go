package cluster

// CBOR-based wire protocol: frames carry a CBOR-encoded Base{T,ID} header
// followed by message-specific fields. Keys/values are byte slices; values
// may be gzip-compressed (Cp=true). LWW uses Ver and HLC.

type MsgType uint8

const (
	MTHello MsgType = iota + 1
	MTHelloResp
	MTGet
	MTGetResp
	MTGetBulk
	MTGetBulkResp
	MTSet
	MTSetResp
	MTSetBulk
	MTSetBulkResp
	MTDelete
	MTDeleteResp
	MTLeaseLoad
	MTLeaseLoadResp
	MTMigratePull
	MTGossip
	MTBackfillDigestReq  MsgType = 200
	MTBackfillDigestResp MsgType = 201
	MTBackfillKeysReq    MsgType = 202
	MTBackfillKeysResp   MsgType = 203
)

type Base struct {
	T  MsgType `cbor:"t"`
	ID uint64  `cbor:"id"`
}

type MsgHello struct {
	Base
	From  string `cbor:"f"`
	Token string `cbor:"tok"`
}
type MsgHelloResp struct {
	Base
	OK  bool   `cbor:"ok"`
	Err string `cbor:"err,omitempty"`
}

type MsgGet struct {
	Base
	Key []byte `cbor:"k"`
}
type MsgGetResp struct {
	Base
	Found bool   `cbor:"f"`
	Val   []byte `cbor:"v"`
	Exp   int64  `cbor:"e"`
	Cp    bool   `cbor:"cp"`
	Err   string `cbor:"err,omitempty"`
}

type MsgGetBulk struct {
	Base
	Keys [][]byte `cbor:"ks"`
}
type MsgGetBulkResp struct {
	Base
	Hits []bool   `cbor:"h"`
	Vals [][]byte `cbor:"vs"`
	Exps []int64  `cbor:"es"`
	Cps  []bool   `cbor:"cps"`
	Err  string   `cbor:"err,omitempty"`
}

type MsgSet struct {
	Base
	Key []byte `cbor:"k"`
	Val []byte `cbor:"v"`
	Exp int64  `cbor:"e"`
	Ver uint64 `cbor:"ver"`
	Cp  bool   `cbor:"cp"`
}
type MsgSetResp struct {
	Base
	OK  bool   `cbor:"ok"`
	Err string `cbor:"err,omitempty"`
}

type KV struct {
	K   []byte `cbor:"k"`
	V   []byte `cbor:"v"`
	E   int64  `cbor:"e"`
	Ver uint64 `cbor:"ver"`
	Cp  bool   `cbor:"cp"`
}
type MsgSetBulk struct {
	Base
	Items []KV `cbor:"items"`
}
type MsgSetBulkResp struct {
	Base
	OK  bool   `cbor:"ok"`
	Err string `cbor:"err,omitempty"`
}

type MsgDel struct {
	Base
	Key []byte `cbor:"k"`
	Ver uint64 `cbor:"ver"`
}
type MsgDelResp struct {
	Base
	OK  bool   `cbor:"ok"`
	Err string `cbor:"err,omitempty"`
}

type MsgLeaseLoad struct {
	Base
	Key []byte `cbor:"k"`
}
type MsgLeaseLoadResp struct {
	Base
	Found bool   `cbor:"f"`
	Val   []byte `cbor:"v"`
	Exp   int64  `cbor:"e"`
	Cp    bool   `cbor:"cp"`
	Err   string `cbor:"err,omitempty"`
}

type MsgGossip struct {
	Base
	From  string           `cbor:"f"`
	Seen  map[string]int64 `cbor:"sn"`
	Peers []string         `cbor:"pe"`
	Load  NodeLoad         `cbor:"ld"`
	TopK  []HotKey         `cbor:"hh"`
	Epoch uint64           `cbor:"ep"`
}

type NodeLoad struct {
	Size         int64  `cbor:"sz"`
	Evictions    int64  `cbor:"ev"`
	FreeMemBytes uint64 `cbor:"fm"`
	CPUu16       uint16 `cbor:"cpu"`
}

type HotKey struct {
	K []byte `cbor:"k"`
	C uint64 `cbor:"c"`
}
