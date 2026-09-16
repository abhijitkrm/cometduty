package bech32

import (
	"bytes"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	data := []byte{0x00, 0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88, 0x99,
		0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0xff, 0x10, 0x20, 0x30, 0x40}
	for _, hrp := range []string{"cosmosvalcons", "osmovalcons", "evmosvalcons", "x"} {
		enc, err := Encode(hrp, data)
		if err != nil {
			t.Fatalf("encode %s: %v", hrp, err)
		}
		gotHRP, gotData, err := Decode(enc)
		if err != nil {
			t.Fatalf("decode %s: %v", enc, err)
		}
		if gotHRP != hrp {
			t.Errorf("hrp: got %q want %q", gotHRP, hrp)
		}
		if !bytes.Equal(gotData, data) {
			t.Errorf("data: got %x want %x", gotData, data)
		}
	}
}

// BIP-0173 reference vectors. "a12uel5l" is the canonical empty-data vector;
// its checksum correctness is also covered by TestEncodeEmpty.
func TestReferenceVectors(t *testing.T) {
	for _, v := range []string{"a12uel5l", "A12UEL5L"} {
		if _, _, err := Decode(v); err != nil {
			t.Errorf("valid vector %q rejected: %v", v, err)
		}
	}
	invalid := []string{
		" 1nwldj5",      // hrp char out of range (valid checksum, bad hrp)
		"pzry9x0s0muk",  // no separator
		"1pzry9x0s0muk", // empty hrp
		"x1b4n0q5v",     // invalid data char 'b'
		"li1dgmt3",      // too short for a checksum
		"a12Uel5l",      // mixed case
		// a bech32m vector must NOT decode as bech32
		"abcdef1l7aum6echk45nj3s0wdvt2fg8x9yrzpqzd3ryx",
	}
	for _, v := range invalid {
		if _, _, err := Decode(v); err == nil {
			t.Errorf("invalid vector %q accepted", v)
		}
	}
}

func TestEncodeEmpty(t *testing.T) {
	enc, err := Encode("a", nil)
	if err != nil {
		t.Fatal(err)
	}
	if enc != "a12uel5l" {
		t.Errorf("got %q want a12uel5l", enc)
	}
}

func TestHRPErrors(t *testing.T) {
	if _, err := Encode("", []byte{1}); err == nil {
		t.Error("empty hrp accepted")
	}
	if _, err := HRP("noseparator"); err == nil {
		t.Error("missing separator accepted")
	}
	if h, err := HRP("abc1def"); err != nil || h != "abc" {
		t.Errorf("HRP: got %q, %v", h, err)
	}
}
