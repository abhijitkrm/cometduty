// Package protowire provides a minimal protobuf wire-format reader. It exists so
// that simple well-known messages (google.protobuf.Any, the various
// single-field PubKey types) can be decoded without generated types.
//
// Complex messages should still use the generated types from cosmossdk.io/api;
// this package is deliberately small and only supports what we need.
package protowire

import (
	"errors"
	"fmt"
)

// Field is one decoded protobuf field.
type Field struct {
	Num   int
	Wire  int
	Value uint64 // valid when Wire == 0 (varint)
	Bytes []byte // valid when Wire == 2 (length-delimited)
}

const (
	WireVarint = 0
	WireBytes  = 2
)

var ErrTruncated = errors.New("protowire: truncated input")

// Parse decodes all fields in a protobuf message. It only understands varint and
// length-delimited wire types, which covers the messages this project decodes
// by hand.
func Parse(b []byte) ([]Field, error) {
	var fields []Field
	for len(b) > 0 {
		num, wire, n := consumeTag(b)
		if n < 0 {
			return nil, ErrTruncated
		}
		b = b[n:]
		switch wire {
		case WireVarint:
			v, m := consumeVarint(b)
			if m < 0 {
				return nil, ErrTruncated
			}
			fields = append(fields, Field{Num: num, Wire: wire, Value: v})
			b = b[m:]
		case WireBytes:
			l, m := consumeVarint(b)
			if m < 0 || len(b[m:]) < int(l) {
				return nil, ErrTruncated
			}
			b = b[m:]
			fields = append(fields, Field{Num: num, Wire: wire, Bytes: b[:l]})
			b = b[l:]
		default:
			return nil, fmt.Errorf("protowire: unsupported wire type %d", wire)
		}
	}
	return fields, nil
}

// Get returns the first field with the given number.
func Get(fields []Field, num int) (Field, bool) {
	for _, f := range fields {
		if f.Num == num {
			return f, true
		}
	}
	return Field{}, false
}

// EncodeString produces the wire encoding of a single string field.
func EncodeString(num int, s string) []byte {
	out := appendVarint(nil, uint64(num<<3|WireBytes))
	out = appendVarint(out, uint64(len(s)))
	return append(out, s...)
}

// appendVarint appends a uvarint to b.
func appendVarint(b []byte, v uint64) []byte {
	for v >= 0x80 {
		b = append(b, byte(v)|0x80)
		v >>= 7
	}
	return append(b, byte(v))
}

func consumeVarint(b []byte) (uint64, int) {
	var v uint64
	for i := 0; i < len(b) && i < 10; i++ {
		v |= uint64(b[i]&0x7f) << uint(7*i)
		if b[i] < 0x80 {
			return v, i + 1
		}
	}
	return 0, -1
}

func consumeTag(b []byte) (num, wire, n int) {
	v, n := consumeVarint(b)
	if n < 0 || v == 0 {
		return 0, 0, -1
	}
	return int(v >> 3), int(v & 7), n
}
