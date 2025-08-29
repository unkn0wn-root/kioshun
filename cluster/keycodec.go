package cluster

import (
	"encoding/binary"
	"errors"

	xxhash "github.com/cespare/xxhash/v2"
)

// KeyCodec maps K <-> []byte for wire/hashing. Should be
// stable across nodes. Optional KeyHasher allows zero-copy hash fast-paths.
type KeyCodec[K any] interface {
	EncodeKey(K) []byte
	DecodeKey([]byte) (K, error)
}

// KeyHasher optional fast-path (zero-copy hash of K).
type KeyHasher[K any] interface {
	Hash64(K) uint64
}

// String keys: encode to raw bytes; xxhash for hashing.
type StringKeyCodec[K ~string] struct{}

func (StringKeyCodec[K]) EncodeKey(k K) []byte          { return []byte(string(k)) }
func (StringKeyCodec[K]) DecodeKey(b []byte) (K, error) { return K(string(b)), nil }
func (StringKeyCodec[K]) Hash64(k K) uint64             { return xxhash.Sum64String(string(k)) }

// Bytes keys: returns underlying slice. Decode copies to detach from caller.
type BytesKeyCodec[K ~[]byte] struct{}

func (BytesKeyCodec[K]) EncodeKey(k K) []byte          { return []byte(k) }
func (BytesKeyCodec[K]) DecodeKey(b []byte) (K, error) { return K(append([]byte(nil), b...)), nil }
func (BytesKeyCodec[K]) Hash64(k K) uint64             { return xxhash.Sum64([]byte(k)) }

type Int64KeyCodec[K ~int64] struct{}

func (Int64KeyCodec[K]) EncodeKey(k K) []byte {
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], uint64(k))
	return buf[:]
}

func (Int64KeyCodec[K]) DecodeKey(b []byte) (K, error) {
	if len(b) != 8 {
		return *new(K), errors.New("invalid int64 key length")
	}
	return K(int64(binary.BigEndian.Uint64(b))), nil
}

func (Int64KeyCodec[K]) Hash64(k K) uint64 {
	return mix64(uint64(k))
}

type Uint64KeyCodec[K ~uint64] struct{}

func (Uint64KeyCodec[K]) EncodeKey(k K) []byte {
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], uint64(k))
	return buf[:]
}

func (Uint64KeyCodec[K]) DecodeKey(b []byte) (K, error) {
	if len(b) != 8 {
		return *new(K), errors.New("invalid uint64 key length")
	}
	return K(binary.BigEndian.Uint64(b)), nil
}

func (Uint64KeyCodec[K]) Hash64(k K) uint64 {
	return mix64(uint64(k))
}
