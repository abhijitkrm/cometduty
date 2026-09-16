package monitor

import (
	"encoding/json"
	"strconv"
	"strings"
)

// SignState is how a validator's signature appears (or doesn't) in a block.
// Ordered by "how far the validator got": a missed block where a precommit was
// gossiped is less alarming than one where nothing was seen at all.
type SignState int

const (
	SignMissed SignState = iota
	SignPrevote
	SignPrecommit
	SignSigned
	SignProposed
	SignUnknown SignState = -1
)

func (s SignState) String() string {
	switch s {
	case SignMissed:
		return "missed"
	case SignPrevote:
		return "prevote"
	case SignPrecommit:
		return "precommit"
	case SignSigned:
		return "signed"
	case SignProposed:
		return "proposed"
	default:
		return "unknown"
	}
}

// strInt64 handles CometBFT's numbers-as-strings JSON convention.
type strInt64 int64

func (si *strInt64) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	if s == "" {
		*si = 0
		return nil
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return err
	}
	*si = strInt64(v)
	return nil
}

// commitSig is one entry in a block's last_commit.signatures.
type commitSig struct {
	BlockIDFlag      int    `json:"block_id_flag"`
	ValidatorAddress string `json:"validator_address"`
	Signature        string `json:"signature"`
}

// blockEvent is the trimmed shape of a tendermint/event/NewBlock payload.
type blockEvent struct {
	Block struct {
		Header struct {
			Height          strInt64 `json:"height"`
			ProposerAddress string   `json:"proposer_address"`
		} `json:"header"`
		LastCommit struct {
			Height     strInt64    `json:"height"`
			Signatures []commitSig `json:"signatures"`
		} `json:"last_commit"`
	} `json:"block"`
}

// height is the block's own height; the signatures describe the PREVIOUS block's
// commit. (last_commit.height = header.height - 1 on live chains.)
func (b blockEvent) height() int64 { return int64(b.Block.Header.Height) }

// blockIdFlagCommit is the flag value for a real commit signature.
const blockIdFlagCommit = 2

// proposer returns the proposer's upper-case hex address.
func (b blockEvent) proposer() string { return b.Block.Header.ProposerAddress }

// signed reports whether addr has a COMMIT signature in last_commit, and the
// network-wide ratio of signed entries (voting-power participation proxy).
func (b blockEvent) signed(addr string) (ok bool, ratio float64) {
	total, signed := 0, 0
	for _, s := range b.Block.LastCommit.Signatures {
		total++
		if s.BlockIDFlag == blockIdFlagCommit && s.Signature != "" {
			signed++
			if strings.EqualFold(s.ValidatorAddress, addr) {
				ok = true
			}
		}
	}
	if total > 0 {
		ratio = float64(signed) / float64(total)
	}
	return ok, ratio
}

// voteEvent is the trimmed shape of a tendermint/event/Vote payload.
// Note the capital "Vote" key — that is what the wire actually sends.
type voteEvent struct {
	Vote struct {
		Type             int      `json:"type"`
		Height           strInt64 `json:"height"`
		Round            strInt64 `json:"round"`
		ValidatorAddress string   `json:"validator_address"`
	} `json:"Vote"`
}

// cometBFT signed message types (values are stable across versions).
const (
	msgTypePrevote   = 1
	msgTypePrecommit = 2
	msgTypeProposal  = 32
)

// decodeBlock parses a NewBlock event value.
func decodeBlock(raw []byte) (*blockEvent, error) {
	b := &blockEvent{}
	if err := json.Unmarshal(raw, b); err != nil {
		return nil, err
	}
	return b, nil
}

// decodeVote parses a Vote event value.
func decodeVote(raw []byte) (*voteEvent, error) {
	v := &voteEvent{}
	if err := json.Unmarshal(raw, v); err != nil {
		return nil, err
	}
	return v, nil
}

// voteState maps the wire type to our SignState ladder.
func (v voteEvent) state() SignState {
	switch v.Vote.Type {
	case msgTypePrevote:
		return SignPrevote
	case msgTypePrecommit:
		return SignPrecommit
	case msgTypeProposal:
		return SignProposed
	default:
		return SignUnknown
	}
}

// voteTracker accumulates per-height vote observations for one validator.
// Votes can arrive before OR after the block they belong to; we merge them by
// height so a late vote can never contaminate the next block's classification
// (a subtle v2 bug).
type voteTracker struct {
	pending map[int64]SignState
}

func newVoteTracker() *voteTracker {
	return &voteTracker{pending: map[int64]SignState{}}
}

// observe records a vote for height h.
func (vt *voteTracker) observe(h int64, s SignState) {
	if s > vt.pending[h] {
		vt.pending[h] = s
	}
}

// resolve computes the final classification for a committed block: the max of
// what the block's own commit data says and what votes we observed for the
// signed height (block N's commit describes height N-1).
func (vt *voteTracker) resolve(signedHeight int64, blockSays SignState) SignState {
	best := blockSays
	if v, ok := vt.pending[signedHeight]; ok && v > best {
		best = v
	}
	// prune everything at or below the finalized height
	for h := range vt.pending {
		if h <= signedHeight {
			delete(vt.pending, h)
		}
	}
	return best
}

// classify determines the sign state for a validator given a new block event.
// addr is the validator's consensus address in upper-case hex.
func classify(b *blockEvent, addr string, vt *voteTracker) (SignState, float64) {
	signed, ratio := b.signed(addr)
	var blockSays SignState
	switch {
	case b.proposer() == addr:
		blockSays = SignProposed
	case signed:
		blockSays = SignSigned
	default:
		blockSays = SignMissed
	}
	// signatures in block N's last_commit attest to height N-1
	signedHeight := b.height() - 1
	if int64(b.Block.LastCommit.Height) > 0 {
		signedHeight = int64(b.Block.LastCommit.Height)
	}
	return vt.resolve(signedHeight, blockSays), ratio
}
