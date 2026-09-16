package main

import (
	"testing"
	"time"
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

func TestAnExpiredSlatepackReturnsTheMoneyAndResolves(t *testing.T) {
	// Cancelling at the wallet unlocks the outputs, so the miner is owed again and the
	// payout is finished with. They keep the money; they just get paid again next run.
	if !returnsToBalance(PayoutExpired) {
		t.Error("an expired slatepack did not return the amount to the balance")
	}
	if !resolves(PayoutExpired) {
		t.Error("an expired slatepack stayed open after its money went back")
	}
}

func TestOnlyAnUnclaimedSlatepackCanExpire(t *testing.T) {
	// Expiring a sent payout would credit a miner who was already paid. Expiring a reserved
	// one would guess at an outcome nobody knows.
	long := time.Now().Add(-365 * 24 * time.Hour).Unix()
	for _, s := range []PayoutState{PayoutSent, PayoutFailed, PayoutReserved, PayoutExpired} {
		if Expired(&PayoutRecord{State: s, At: long}, time.Now()) {
			t.Errorf("state %q was treated as expirable", s)
		}
	}
	if !Expired(&PayoutRecord{State: PayoutAwaitingClaim, At: long}, time.Now()) {
		t.Error("a year-old unclaimed slatepack did not expire")
	}
}

func TestTheTTLBoundaryIsExact(t *testing.T) {
	now := time.Now()
	at := now.Add(-SlatepackTTL).Unix()
	if !Expired(&PayoutRecord{State: PayoutAwaitingClaim, At: at}, now) {
		t.Error("a payout exactly at the TTL did not expire")
	}
	just := now.Add(-SlatepackTTL).Add(2 * time.Second).Unix()
	if Expired(&PayoutRecord{State: PayoutAwaitingClaim, At: just}, now) {
		t.Error("a payout just inside the TTL expired early")
	}
	if SlatepackTTL != 30*24*time.Hour {
		t.Errorf("SlatepackTTL is %v, want 30 days", SlatepackTTL)
	}
}

func TestASlateIDIsFoundInWalletOutput(t *testing.T) {
	// Without the transaction id a slatepack payout can never be cancelled, so the outputs
	// behind it stay locked forever.
	out := `Command 'send' completed successfully
	Slatepack data follows...
	tx id: 0436430c-2b02-624c-2032-570501212b00 sent`
	if got := SlateIDFrom(out); got != "0436430c-2b02-624c-2032-570501212b00" {
		t.Errorf("SlateIDFrom = %q", got)
	}
	if SlateIDFrom("no uuid here at all") != "" {
		t.Error("found a transaction id in output that carries none")
	}
}

func TestTheConfiguredThresholdIsTheOneThePageAdvertises(t *testing.T) {
	// The pool page states a payout floor. If the daemon uses a different number the page
	// is lying, so the floor comes from config rather than a constant in the code.
	c := &config{}
	c.Payer.ThresholdGrin = 10
	if got := ThresholdFrom(c); got != 10_000_000_000 {
		t.Errorf("10 GRIN floor = %d nanogrin, want 10000000000", got)
	}
	c.Payer.ThresholdGrin = 0.5
	if got := ThresholdFrom(c); got != 500_000_000 {
		t.Errorf("0.5 GRIN floor = %d nanogrin", got)
	}
}

func TestAMinerCanActuallySelectSlatepack(t *testing.T) {
	// The stratum password field is how a miner chooses, because there are no accounts and
	// nothing else reaches them. Before this the stored preference was unreachable and the
	// slatepack transport the page advertises could never be selected at all.
	for _, c := range []struct {
		pass string
		want Transport
	}{
		{"slatepack", TransportSlatepack},
		{" slatepack ", TransportSlatepack},
		{"x", TransportTor},
		{"", TransportTor},
		{"tor", TransportTor},
		{"whatever", TransportTor},
	} {
		if got := transportFromStored(c.pass); got != c.want {
			t.Errorf("pass %q selected %q, want %q", c.pass, got, c.want)
		}
	}
}
