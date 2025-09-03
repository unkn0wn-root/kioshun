package cluster

import (
	"reflect"
	"testing"
)

func TestBytesCodecPassAndCopyOnDecode(t *testing.T) {
	var bc BytesCodec

	// encode should be pass-through
	v := []byte{1, 2, 3}
	enc, err := bc.Encode(v)
	if err != nil {
		t.Fatalf("encode error: %v", err)
	}
	v[0] = 9
	if enc[0] != 9 {
		t.Fatalf("encode not pass-through: got %v", enc)
	}

	// decode should return a copy detached from input.
	in := []byte{4, 5, 6}
	out, err := bc.Decode(in)
	if err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if !reflect.DeepEqual(out, in) {
		t.Fatalf("decode mismatch: got %v want %v", out, in)
	}
	in[0] = 7
	if out[0] == in[0] {
		t.Fatalf("decode did not copy. Out mutated: %v vs %v", out, in)
	}
}

func TestCBORCodecRoundTrip(t *testing.T) {
	type S struct {
		A int
		B string
	}
	var c CBORCodec[S]
	orig := S{A: 42, B: "x"}
	b, err := c.Encode(orig)
	if err != nil {
		t.Fatalf("encode error: %v", err)
	}
	got, err := c.Decode(b)
	if err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if !reflect.DeepEqual(got, orig) {
		t.Fatalf("round-trip mismatch: got %+v want %+v", got, orig)
	}
}
