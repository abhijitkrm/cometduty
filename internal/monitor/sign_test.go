package monitor

import (
	"strconv"
	"testing"
)

const (
	addrA = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	addrB = "BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB"
	addrC = "CCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCC"
)

// blockJSON builds a NewBlock event payload.
func blockJSON(t *testing.T, height int64, proposer string, sigs string) []byte {
	t.Helper()
	return []byte(`{"block":{"header":{"height":"` + itoa(height) + `","proposer_address":"` + proposer + `"},
"last_commit":{"height":"` + itoa(height-1) + `","signatures":[` + sigs + `]}}}`)
}

func itoa(v int64) string { return strconv.FormatInt(v, 10) }

func intStr(v int) string { return strconv.Itoa(v) }

func TestClassifyProposed(t *testing.T) {
	raw := blockJSON(t, 100, addrA, `{"block_id_flag":2,"validator_address":"`+addrB+`","signature":"AA=="}`)
	b, err := decodeBlock(raw)
	if err != nil {
		t.Fatal(err)
	}
	st, _ := classify(b, addrA, newVoteTracker())
	if st != SignProposed {
		t.Errorf("got %s want proposed", st)
	}
}

func TestClassifySigned(t *testing.T) {
	raw := blockJSON(t, 100, addrA, `{"block_id_flag":2,"validator_address":"`+addrB+`","signature":"AA=="}`)
	b, _ := decodeBlock(raw)
	st, _ := classify(b, addrB, newVoteTracker())
	if st != SignSigned {
		t.Errorf("got %s want signed", st)
	}
}

func TestClassifyMissed(t *testing.T) {
	raw := blockJSON(t, 100, addrA, `{"block_id_flag":2,"validator_address":"`+addrB+`","signature":"AA=="}`)
	b, _ := decodeBlock(raw)
	st, _ := classify(b, addrC, newVoteTracker())
	if st != SignMissed {
		t.Errorf("got %s want missed", st)
	}
}

// block_id_flag=1 (ABSENT) entries still carry the validator address on some
// chains — only flag=2 counts as signed. This is the fix for v2's reliance on
// empty validator_address.
func TestAbsentFlagIsMissed(t *testing.T) {
	raw := blockJSON(t, 100, addrA,
		`{"block_id_flag":1,"validator_address":"`+addrB+`","signature":""},
		 {"block_id_flag":2,"validator_address":"`+addrC+`","signature":"AA=="}`)
	b, _ := decodeBlock(raw)
	st, _ := classify(b, addrB, newVoteTracker())
	if st != SignMissed {
		t.Errorf("absent-flag sig counted as signed: %s", st)
	}
}

// flag=3 (nil) also must not count.
func TestNilFlagIsMissed(t *testing.T) {
	raw := blockJSON(t, 100, addrA,
		`{"block_id_flag":3,"validator_address":"`+addrB+`","signature":"AA=="}`)
	b, _ := decodeBlock(raw)
	st, _ := classify(b, addrB, newVoteTracker())
	if st != SignMissed {
		t.Errorf("nil-flag sig counted as signed: %s", st)
	}
}

// A precommit vote observed for the signed height upgrades missed → precommit.
func TestVoteUpgradesMissedToPrecommit(t *testing.T) {
	vt := newVoteTracker()
	vt.observe(99, SignPrecommit)
	raw := blockJSON(t, 100, addrA, `{"block_id_flag":2,"validator_address":"`+addrC+`","signature":"AA=="}`)
	b, _ := decodeBlock(raw)
	st, _ := classify(b, addrB, vt)
	if st != SignPrecommit {
		t.Errorf("got %s want precommit", st)
	}
}

func TestVoteUpgradesToPrevote(t *testing.T) {
	vt := newVoteTracker()
	vt.observe(99, SignPrevote)
	raw := blockJSON(t, 100, addrA, `{"block_id_flag":2,"validator_address":"`+addrC+`","signature":"AA=="}`)
	b, _ := decodeBlock(raw)
	st, _ := classify(b, addrB, vt)
	if st != SignPrevote {
		t.Errorf("got %s want prevote", st)
	}
}

// A vote for a LATER height must not contaminate this block's classification,
// and finalized heights get pruned.
func TestVotesAreHeightScoped(t *testing.T) {
	vt := newVoteTracker()
	vt.observe(150, SignPrecommit) // far-future vote
	raw := blockJSON(t, 100, addrA, `{"block_id_flag":2,"validator_address":"`+addrC+`","signature":"AA=="}`)
	b, _ := decodeBlock(raw)
	st, _ := classify(b, addrB, vt)
	if st != SignMissed {
		t.Errorf("future vote leaked into height 99: %s", st)
	}
	if _, ok := vt.pending[150]; !ok {
		t.Error("future vote wrongly pruned")
	}
	// now resolve past 150 — should prune
	raw2 := blockJSON(t, 151, addrA, `{"block_id_flag":2,"validator_address":"`+addrC+`","signature":"AA=="}`)
	b2, _ := decodeBlock(raw2)
	classify(b2, addrB, vt)
	if _, ok := vt.pending[150]; ok {
		t.Error("finalized vote not pruned")
	}
}

// Participation ratio: 1 of 2 signatures present → 0.5.
func TestSignatureRatio(t *testing.T) {
	raw := blockJSON(t, 100, addrA,
		`{"block_id_flag":2,"validator_address":"`+addrB+`","signature":"AA=="},
		 {"block_id_flag":1,"validator_address":"`+addrC+`","signature":""}`)
	b, _ := decodeBlock(raw)
	_, ratio := b.signed(addrB)
	if ratio != 0.5 {
		t.Errorf("ratio: got %v want 0.5", ratio)
	}
}

func TestDecodeVoteTypes(t *testing.T) {
	for _, tc := range []struct {
		typ  int
		want SignState
	}{{1, SignPrevote}, {2, SignPrecommit}, {32, SignProposed}, {0, SignUnknown}, {99, SignUnknown}} {
		v, err := decodeVote([]byte(`{"Vote":{"type":` + intStr(tc.typ) + `,"height":"100","round":"0","validator_address":"` + addrB + `"}}`))
		if err != nil {
			t.Fatal(err)
		}
		if v.state() != tc.want {
			t.Errorf("type %d: got %s want %s", tc.typ, v.state(), tc.want)
		}
	}
}

func TestSignStateString(t *testing.T) {
	if SignMissed.String() != "missed" || SignProposed.String() != "proposed" || SignUnknown.String() != "unknown" {
		t.Error("bad String()")
	}
}
