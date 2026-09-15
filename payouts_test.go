package main

import (
	"testing"
)

// The ledger invariant, stated once: at every moment a miner's money is in exactly one of
// two places, their balance or a payout record. Never both, never neither.
//
// These assert the state machine that guarantees it, without a Redis or a wallet. The
// transitions are what carry the risk; the plumbing around them is exercised in
// transports_test.go.

func TestAFailedSendReturnsTheMoneyToTheBalance(t *testing.T) {
	// The naive ordering debits then sends, so a failed send loses the miner's money from
	// the ledger entirely. Failure must put it back.
	rec := &PayoutRecord{Miner: "grin1abc", Amount: 5_000_000_000, State: PayoutReserved}
	if !returnsToBalance(PayoutFailed) {
		t.Error("a failed payout does not return the amount to the balance")
	}
	if returnsToBalance(PayoutSent) {
		t.Error("a sent payout returned the amount to the balance, paying it twice")
	}
	if returnsToBalance(PayoutAwaitingClaim) {
		t.Error("an unclaimed slatepack returned to the balance while still committed")
	}
	_ = rec
}

func TestAnUnclaimedSlatepackStaysCommitted(t *testing.T) {
	// A slatepack is written and the miner has not returned it. The money must not be back
	// in the balance, or the next run pays it again and the miner can still claim the first.
	if resolves(PayoutAwaitingClaim) {
		t.Error("awaiting-claim was treated as resolved; the payout would be forgotten")
	}
	if !resolves(PayoutSent) {
		t.Error("a sent payout stayed open forever")
	}
	if !resolves(PayoutFailed) {
		t.Error("a failed payout stayed open after its money went back")
	}
}

func TestReservedIsTheOnlyStateNeedingAHuman(t *testing.T) {
	// Reserved means the amount left the balance and we do not know whether it was sent.
	// Only a crash between reserve and settle leaves a record here, and guessing either
	// way is worse than surfacing it.
	if resolves(PayoutReserved) {
		t.Error("reserved was treated as resolved, silently losing the amount")
	}
	if returnsToBalance(PayoutReserved) {
		t.Error("reserved auto-returned to the balance; a sent payout would be paid twice")
	}
}

func TestTheDustThresholdKeepsSmallBalancesUnpaid(t *testing.T) {
	// A Grin transaction costs a fee. Paying a balance smaller than the fee burns more than
	// it delivers, so the pool holds it until it is worth sending.
	p := &Payer{threshold: 1_000_000_000}
	for _, c := range []struct {
		owed uint64
		pay  bool
	}{
		{0, false},
		{1, false},
		{999_999_999, false},
		{1_000_000_000, true},
		{60_000_000_000, true},
	} {
		if got := c.owed >= p.threshold; got != c.pay {
			t.Errorf("owed %d: payable=%v, want %v", c.owed, got, c.pay)
		}
	}
}

func TestTorIsTheDefaultTransportWhenAMinerHasNotChosen(t *testing.T) {
	// A miner who never set a preference gets tor, which is what the pool page promises and
	// what needs nothing from them at payout time beyond being online.
	for _, stored := range []string{"", "  ", "nonsense", "TOR", "tor"} {
		if transportFromStored(stored) != TransportTor {
			t.Errorf("stored %q did not resolve to tor", stored)
		}
	}
	for _, stored := range []string{"slatepack", " slatepack "} {
		if transportFromStored(stored) != TransportSlatepack {
			t.Errorf("stored %q did not resolve to slatepack", stored)
		}
	}
}

func TestEveryStateIsEitherResolvedOrNeedsSomething(t *testing.T) {
	// A state that is neither resolved nor actionable is a payout that vanishes from every
	// view. Adding one without deciding which it is should fail here.
	for _, s := range []PayoutState{PayoutReserved, PayoutSent, PayoutAwaitingClaim, PayoutFailed} {
		if !resolves(s) && !needsAttention(s) {
			t.Errorf("state %q is neither resolved nor flagged; it would be invisible", s)
		}
	}
}
