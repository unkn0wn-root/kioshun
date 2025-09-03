package cluster

import (
	"encoding/binary"
	"testing"

	xxhash "github.com/cespare/xxhash/v2"
)

func TestStringKeyCodec(t *testing.T) {
	var kc StringKeyCodec[string]
	k := "hello"
	b := kc.EncodeKey(k)
	if string(b) != k {
		t.Fatalf("encode mismatch: %q", string(b))
	}

	dk, err := kc.DecodeKey(b)
	if err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if dk != k {
		t.Fatalf("decode mismatch: %q != %q", dk, k)
	}
	if kc.Hash64(k) != xxhash.Sum64String(k) {
		t.Fatalf("hash mismatch")
	}
}

func TestBytesKeyCodec(t *testing.T) {
	var kc BytesKeyCodec[[]byte]
	in := []byte{1, 2, 3}
	b := kc.EncodeKey(in)
	if string(b) != string(in) {
		t.Fatalf("encode mismatch")
	}

	dk, err := kc.DecodeKey(b)
	if err != nil {
		t.Fatalf("decode error: %v", err)
	}

	// decode should copy to detach from caller buffer.
	b[0] = 9
	if dk[0] == b[0] {
		t.Fatalf("decode did not copy")
	}

	if kc.Hash64(in) != xxhash.Sum64(in) {
		t.Fatalf("hash mismatch")
	}
}

func TestInt64KeyCodec(t *testing.T) {
	var kc Int64KeyCodec[int64]
	k := int64(-1234567890)
	b := kc.EncodeKey(k)
	if len(b) != 8 {
		t.Fatalf("encode length: %d", len(b))
	}
	got, err := kc.DecodeKey(b)
	if err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if got != k {
		t.Fatalf("round-trip mismatch: %d != %d", got, k)
	}

	// check big-endian layout
	if binary.BigEndian.Uint64(b) != uint64(k) {
		t.Fatalf("big-endian mismatch")
	}

	if _, err := kc.DecodeKey([]byte{1, 2}); err == nil {
		t.Fatalf("expected length error")
	}

	if kc.Hash64(k) != mix64(uint64(k)) {
		t.Fatalf("hash mismatch")
	}
}

func TestUint64KeyCodec(t *testing.T) {
	var kc Uint64KeyCodec[uint64]
	k := uint64(0xdeadbeefcafebabe)
	b := kc.EncodeKey(k)
	if len(b) != 8 {
		t.Fatalf("encode length: %d", len(b))
	}

	got, err := kc.DecodeKey(b)
	if err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if got != k {
		t.Fatalf("round-trip mismatch: %d != %d", got, k)
	}

	if _, err := kc.DecodeKey([]byte{1, 2, 3}); err == nil {
		t.Fatalf("expected length error")
	}

	if kc.Hash64(k) != mix64(k) {
		t.Fatalf("hash mismatch")
	}
}
