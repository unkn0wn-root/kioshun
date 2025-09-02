package cluster

import (
	cbor "github.com/fxamacker/cbor/v2"
)

// Codec abstracts value encoding for the wire. Must be
// deterministic and stable across nodes to allow backfill/replication.
type Codec[V any] interface {
	Encode(V) ([]byte, error)
	Decode([]byte) (V, error)
}

// BytesCodec: pass-through []byte (no copy on Encode; Decode returns a copy).
type BytesCodec struct{}

func (BytesCodec) Encode(v []byte) ([]byte, error) { return v, nil }
func (BytesCodec) Decode(b []byte) ([]byte, error) { out := append([]byte(nil), b...); return out, nil }

type CBORCodec[V any] struct{}

func (CBORCodec[V]) Encode(v V) ([]byte, error) { return cbor.Marshal(v) }
func (CBORCodec[V]) Decode(b []byte) (V, error) {
	var v V
	err := cbor.Unmarshal(b, &v)
	return v, err
}
