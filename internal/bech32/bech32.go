// Package bech32 implements the BIP-0173 bech32 encoding used by Cosmos-style
// addresses. It is self-contained to keep the dependency tree small.
package bech32

import (
	"errors"
	"fmt"
	"strings"
)

const charset = "qpzry9x8gf2tvdw0s3jn54khce6mua7l"

var charsetRev = [128]int8{}

func init() {
	for i := range charsetRev {
		charsetRev[i] = -1
	}
	for i, c := range charset {
		charsetRev[c] = int8(i)
	}
}

func polymod(values []byte) uint32 {
	chk := uint32(1)
	for _, v := range values {
		top := chk >> 25
		chk = (chk&0x1ffffff)<<5 ^ uint32(v)
		for i := 0; i < 5; i++ {
			if (top>>i)&1 == 1 {
				chk ^= [...]uint32{0x3b6a57b2, 0x26508e6d, 0x1ea119fa, 0x3d4233dd, 0x2a1462b3}[i]
			}
		}
	}
	return chk
}

func hrpExpand(hrp string) []byte {
	out := make([]byte, 0, len(hrp)*2+1)
	for _, c := range hrp {
		out = append(out, byte(c>>5))
	}
	out = append(out, 0)
	for _, c := range hrp {
		out = append(out, byte(c&31))
	}
	return out
}

func createChecksum(hrp string, data []byte) []byte {
	values := append(hrpExpand(hrp), data...)
	values = append(values, 0, 0, 0, 0, 0, 0)
	mod := polymod(values) ^ 1
	out := make([]byte, 6)
	for i := 0; i < 6; i++ {
		out[i] = byte(mod >> uint(5*(5-i)) & 31)
	}
	return out
}

func verifyChecksum(hrp string, data []byte) bool {
	return polymod(append(hrpExpand(hrp), data...)) == 1
}

// convertBits converts a byte slice from one bits-per-character group to another,
// e.g. 8-bit bytes to the 5-bit groups bech32 uses.
func convertBits(data []byte, from, to uint8, pad bool) ([]byte, error) {
	acc := uint32(0)
	bits := uint8(0)
	maxv := uint32(1<<to) - 1
	maxAcc := uint32(1<<(from+to-1)) - 1
	out := make([]byte, 0, len(data)*int(from)/int(to))
	for _, b := range data {
		if uint32(b)>>from != 0 {
			return nil, errors.New("bech32: input out of range")
		}
		acc = ((acc << from) | uint32(b)) & maxAcc
		bits += from
		for bits >= to {
			bits -= to
			out = append(out, byte((acc>>bits)&maxv))
		}
	}
	if pad {
		if bits > 0 {
			out = append(out, byte((acc<<(to-bits))&maxv))
		}
	} else if bits >= from || byte((acc<<(to-bits))&maxv) != 0 {
		return nil, errors.New("bech32: invalid padding")
	}
	return out, nil
}

// Encode converts raw bytes into a bech32 string with the given human readable prefix.
func Encode(hrp string, data []byte) (string, error) {
	if len(hrp) == 0 || len(hrp) > 83 {
		return "", errors.New("bech32: invalid hrp length")
	}
	for _, c := range hrp {
		if c < 33 || c > 126 {
			return "", errors.New("bech32: invalid hrp character")
		}
	}
	hrp = strings.ToLower(hrp)
	fiveBit, err := convertBits(data, 8, 5, true)
	if err != nil {
		return "", err
	}
	combined := append(fiveBit, createChecksum(hrp, fiveBit)...)
	var sb strings.Builder
	sb.WriteString(hrp)
	sb.WriteByte('1')
	for _, b := range combined {
		sb.WriteByte(charset[b])
	}
	return sb.String(), nil
}

// Decode parses a bech32 string, returning the human readable prefix and raw data.
func Decode(s string) (string, []byte, error) {
	if len(s) < 8 || len(s) > 90 {
		return "", nil, fmt.Errorf("bech32: invalid length %d", len(s))
	}
	if strings.ToLower(s) != s && strings.ToUpper(s) != s {
		return "", nil, errors.New("bech32: mixed case")
	}
	s = strings.ToLower(s)
	idx := strings.LastIndexByte(s, '1')
	if idx < 1 || idx+7 > len(s) {
		return "", nil, errors.New("bech32: missing or misplaced separator")
	}
	hrp := s[:idx]
	for _, c := range hrp {
		if c < 33 || c > 126 {
			return "", nil, fmt.Errorf("bech32: invalid hrp character %q", c)
		}
	}
	data := make([]byte, 0, len(s)-idx-1)
	for _, c := range s[idx+1:] {
		if c > 127 || charsetRev[c] == -1 {
			return "", nil, fmt.Errorf("bech32: invalid character %q", c)
		}
		data = append(data, byte(charsetRev[c]))
	}
	if !verifyChecksum(hrp, data) {
		return "", nil, errors.New("bech32: checksum mismatch")
	}
	decoded, err := convertBits(data[:len(data)-6], 5, 8, false)
	if err != nil {
		return "", nil, err
	}
	return hrp, decoded, nil
}

// HRP returns just the human readable prefix of a bech32 string.
func HRP(s string) (string, error) {
	idx := strings.LastIndexByte(s, '1')
	if idx < 1 {
		return "", errors.New("bech32: missing separator")
	}
	return s[:idx], nil
}
