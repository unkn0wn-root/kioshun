package cluster

// Digest for a key-hash prefix bucket; used to detect divergent buckets
// without transferring all keys.
type BucketDigest struct {
	Prefix []byte `cbor:"p"` // first Depth bytes of key-hash (big-endian)
	Count  uint32 `cbor:"c"`
	Hash64 uint64 `cbor:"h"`
}

type MsgBackfillDigestReq struct {
	Base
	Target string `cbor:"t"` // joiner PublicURL
	Depth  uint8  `cbor:"d"` // bytes of prefix (1..8)
}

type MsgBackfillDigestResp struct {
	Base
	Depth     uint8          `cbor:"d"`
	Buckets   []BucketDigest `cbor:"b"`
	NotInRing bool           `cbor:"nr,omitempty"`
}

type MsgBackfillKeysReq struct {
	Base
	Target string `cbor:"t"` // joiner PublicURL
	Prefix []byte `cbor:"p"` // len == Depth
	Limit  int    `cbor:"l"` // page size
	Cursor []byte `cbor:"u"` // last 8B key-hash (big-endian) for pagination
}

type MsgBackfillKeysResp struct {
	Base
	Items      []KV   `cbor:"i"`
	NextCursor []byte `cbor:"u"` // nil when done
	Done       bool   `cbor:"o"`
	NotInRing  bool   `cbor:"nr,omitempty"`
}
