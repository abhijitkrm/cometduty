package consensus

import (
	"bytes"
	"crypto/sha256"
	"testing"

	"github.com/abhijitkrm/cometduty/internal/protowire"
)

var testKey = []byte{
	0x00, 0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77,
	0x88, 0x99, 0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0xff,
	0x10, 0x21, 0x32, 0x43, 0x54, 0x65, 0x76, 0x87,
	0x98, 0xa9, 0xba, 0xcb, 0xdc, 0xed, 0xfe, 0x0f,
}

// Any encoding of PubKey{key: testKey}.
func anyValue(t *testing.T) []byte {
	t.Helper()
	return protowire.EncodeString(1, string(testKey))
}

func TestAnyToPubkeyBytes(t *testing.T) {
	urls := []string{
		"/cosmos.crypto.ed25519.PubKey",
		"/cosmos.crypto.secp256k1.PubKey",
		"/cosmos.evm.crypto.v1.ethsecp256k1.PubKey",
		"/ethermint.crypto.v1.ethsecp256k1.PubKey",
		"/injective.crypto.v1beta1.ethsecp256k1.PubKey",
		"/some.future.chain.v9.weirdkey.PubKey", // unknown but same wire shape
	}
	for _, u := range urls {
		pk, err := AnyToPubkeyBytes(u, anyValue(t))
		if err != nil {
			t.Errorf("%s: %v", u, err)
			continue
		}
		if !bytes.Equal(pk, testKey) {
			t.Errorf("%s: got %x want %x", u, pk, testKey)
		}
	}
}

func TestMultisigRejected(t *testing.T) {
	if _, err := AnyToPubkeyBytes("/cosmos.crypto.multisig.LegacyAminoPubKey", anyValue(t)); err == nil {
		t.Error("multisig pubkey accepted")
	}
}

func TestPubkeyToAddressIsSHA256Truncated(t *testing.T) {
	// CometBFT uses tmhash.SumTruncated = sha256(pk)[:20] for ALL key types,
	// including ethsecp256k1 (keccak is only for account addresses).
	addr := PubkeyToAddress(testKey)
	sum := sha256.Sum256(testKey)
	if !bytes.Equal(addr, sum[:20]) {
		t.Errorf("got %x want %x", addr, sum[:20])
	}
	if len(addr) != 20 {
		t.Errorf("address length %d", len(addr))
	}
}

func TestAnyToAddress(t *testing.T) {
	addr, err := AnyToAddress("/cosmos.evm.crypto.v1.ethsecp256k1.PubKey", anyValue(t))
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(testKey)
	if !bytes.Equal(addr, sum[:20]) {
		t.Errorf("got %x want %x", addr, sum[:20])
	}
}

func TestAddressHex(t *testing.T) {
	got := AddressHex([]byte{0xab, 0xcd})
	if got != "ABCD" {
		t.Errorf("got %q", got)
	}
}

func TestBadInputs(t *testing.T) {
	if _, err := AnyToPubkeyBytes("", anyValue(t)); err == nil {
		t.Error("empty type url accepted")
	}
	if _, err := AnyToPubkeyBytes("/cosmos.crypto.ed25519.PubKey", nil); err == nil {
		t.Error("empty value accepted")
	}
	if _, err := AnyToPubkeyBytes("/cosmos.crypto.ed25519.PubKey", []byte{0xff, 0xff}); err == nil {
		t.Error("garbage value accepted")
	}
}
