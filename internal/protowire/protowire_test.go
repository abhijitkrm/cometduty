package protowire

import (
	"bytes"
	"testing"
)

func TestParseVarintAndBytes(t *testing.T) {
	// field 1 varint = 150; field 2 bytes = "hello"
	msg := []byte{0x08, 0x96, 0x01, 0x12, 0x05, 'h', 'e', 'l', 'l', 'o'}
	fields, err := Parse(msg)
	if err != nil {
		t.Fatal(err)
	}
	if len(fields) != 2 {
		t.Fatalf("got %d fields", len(fields))
	}
	if fields[0].Num != 1 || fields[0].Wire != WireVarint || fields[0].Value != 150 {
		t.Errorf("field0: %+v", fields[0])
	}
	if fields[1].Num != 2 || fields[1].Wire != WireBytes || string(fields[1].Bytes) != "hello" {
		t.Errorf("field1: %+v", fields[1])
	}
}

func TestParseTruncated(t *testing.T) {
	for _, bad := range [][]byte{
		{0x08},                 // varint tag with no value
		{0x12, 0x05, 'h', 'e'}, // declared 5 bytes, gave 2
		{0x80},                 // truncated tag
		{0x08, 0x80},           // truncated varint
		{0x00, 0x01},           // zero tag
		{0x0b},                 // wire type 3 (group) unsupported
	} {
		if _, err := Parse(bad); err == nil {
			t.Errorf("bad input %x parsed without error", bad)
		}
	}
}

func TestEncodeStringRoundTrip(t *testing.T) {
	enc := EncodeString(1, "some-key-material")
	fields, err := Parse(enc)
	if err != nil {
		t.Fatal(err)
	}
	f, ok := Get(fields, 1)
	if !ok {
		t.Fatal("field 1 missing")
	}
	if string(f.Bytes) != "some-key-material" {
		t.Errorf("got %q", f.Bytes)
	}
}

func TestGetMissing(t *testing.T) {
	if _, ok := Get(nil, 7); ok {
		t.Error("Get on empty fields returned true")
	}
}

func TestParseSkipsWellformedUnknown(t *testing.T) {
	// two bytes fields; Get returns first
	msg := append(EncodeString(3, "a"), EncodeString(3, "b")...)
	fields, err := Parse(msg)
	if err != nil {
		t.Fatal(err)
	}
	f, ok := Get(fields, 3)
	if !ok || !bytes.Equal(f.Bytes, []byte("a")) {
		t.Errorf("want first field 'a', got %q", f.Bytes)
	}
}
