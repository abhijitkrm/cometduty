// Package consensus contains helpers for deriving CometBFT consensus addresses
// from the many pubkey encodings used by Cosmos-SDK and Cosmos-EVM chains.
package consensus

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"

	"github.com/abhijitkrm/cometduty/internal/protowire"
)

// Pubkey type URLs seen in the wild. All of these are `message PubKey { bytes
// key = 1; }` at the wire level, so a single generic decode covers them —
// including future variants we have not listed.
var knownPubkeyPrefixes = []string{
	"/cosmos.crypto.ed25519.PubKey",
	"/cosmos.crypto.secp256k1.PubKey",
	"/cosmos.crypto.secp256r1.PubKey",
	"/cosmos.evm.crypto.v1.ethsecp256k1.PubKey",
	"/cosmos.evm.crypto.v1alpha1.ethsecp256k1.PubKey",
	"/ethermint.crypto.v1.ethsecp256k1.PubKey",
	"/ethermint.crypto.v1alpha1.ethsecp256k1.PubKey",
	"/injective.crypto.v1beta1.ethsecp256k1.PubKey",
	"/stratos.crypto.v1.ethsecp256k1.PubKey",
	"/dymension.crypto.ethsecp256k1.PubKey",
}

// IsKnownPubkey reports whether the type URL looks like a supported consensus
// pubkey. Unknown types are still attempted with the single-`key`-field decode
// since virtually all consensus pubkey types share that wire shape; this list is
// only used to warn early.
func IsKnownPubkey(typeURL string) bool {
	for _, p := range knownPubkeyPrefixes {
		if p == typeURL {
			return true
		}
	}
	return false
}

// AnyToPubkeyBytes decodes a google.protobuf.Any carrying a PubKey and returns
// the raw public key bytes.
func AnyToPubkeyBytes(typeURL string, value []byte) ([]byte, error) {
	if typeURL == "" {
		return nil, errors.New("empty pubkey type url")
	}
	// multisig (LegacyAminoPubKey) cannot be reduced to a single address.
	if strings.Contains(typeURL, "multisig") || strings.Contains(typeURL, "MultiSig") {
		return nil, fmt.Errorf("multisig consensus keys are not supported (%s)", typeURL)
	}
	fields, err := protowire.Parse(value)
	if err != nil {
		return nil, fmt.Errorf("decoding %s: %w", typeURL, err)
	}
	f, ok := protowire.Get(fields, 1)
	if !ok || f.Wire != protowire.WireBytes || len(f.Bytes) == 0 {
		return nil, fmt.Errorf("no key bytes found in %s", typeURL)
	}
	return f.Bytes, nil
}

// PubkeyToAddress converts raw public key bytes to a CometBFT consensus
// address. CometBFT uses tmhash.SumTruncated (SHA-256 truncated to 20 bytes) for
// every key type including the ethsecp256k1 family — the EVM keccak address is
// only used for account addresses, never for the validator set.
func PubkeyToAddress(pubkey []byte) []byte {
	sum := sha256.Sum256(pubkey)
	out := make([]byte, 20)
	copy(out, sum[:20])
	return out
}

// AnyToAddress is the composition of AnyToPubkeyBytes and PubkeyToAddress.
func AnyToAddress(typeURL string, value []byte) ([]byte, error) {
	pk, err := AnyToPubkeyBytes(typeURL, value)
	if err != nil {
		return nil, err
	}
	return PubkeyToAddress(pk), nil
}

// AddressHex returns the upper-case hex form used in block signatures and vote
// events.
func AddressHex(addr []byte) string {
	return strings.ToUpper(fmt.Sprintf("%X", addr))
}
