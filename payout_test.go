package main

import (
	"math"
	"testing"
)

const grin = uint64(1e9)          // nanogrin in one grin
const blockReward = 60 * grin     // consensus.rs: REWARD = BLOCK_TIME_SEC * GRIN_BASE

func round(shares map[string]uint64, reward uint64) *Round {
	return &Round{Height: 4_018_000, Hash: "abc", Finder: "a", Shares: shares, Reward: reward}
}

func sum(cs []Credit) uint64 {
	var t uint64
	for _, c := range cs {
		t += c.Amount
	}
	return t
}

// The bug this file exists for. Upstream computed `shares / totalShare * totalRevenue` in
// uint64 arithmetic, which divides first: for any miner holding less than the entire share
// table that is 0 * revenue. Two miners splitting a block were both paid nothing.
func TestAMinorityShareIsNotPaidZero(t *testing.T) {
	credits, _, err := SplitRound(round(map[string]uint64{"a": 1, "b": 999}, blockReward), 0)
	if err != nil {
		t.Fatal(err)
	}
	byMiner := map[string]uint64{}
	for _, c := range credits {
		byMiner[c.Miner] = c.Amount
	}
	if byMiner["a"] == 0 {
		t.Error("the 0.1% miner was paid nothing; integer division happened before the multiply")
	}
	if byMiner["b"] == 0 {
		t.Error("the 99.9% miner was paid nothing")
	}
}

// Conservation. Whatever the split, credits plus fee must equal the reward exactly: no
// nanogrin invented, none quietly left behind in the pool wallet.
func TestCreditsPlusFeeAlwaysEqualTheReward(t *testing.T) {
	cases := []map[string]uint64{
		{"a": 1},
		{"a": 1, "b": 1},
		{"a": 1, "b": 2, "c": 3},
		{"a": 7, "b": 11, "c": 13, "d": 17, "e": 19},      // coprime, forces remainders
		{"a": 1, "b": 1, "c": 1, "d": 1, "e": 1, "f": 1},  // 60e9 / 6 divides exactly
		{"a": 999_999_937, "b": 1},                        // large, near uint64 overflow if naive
	}
	for _, feeRate := range []float64{0, 0.005, 0.01, 0.5, 0.999} {
		for i, shares := range cases {
			credits, fee, err := SplitRound(round(shares, blockReward), feeRate)
			if err != nil {
				t.Fatalf("case %d fee %v: %v", i, feeRate, err)
			}
			if got := sum(credits) + fee; got != blockReward {
				t.Errorf("case %d fee %v: credits+fee = %d, want %d (leak of %d)",
					i, feeRate, got, blockReward, int64(blockReward)-int64(got))
			}
		}
	}
}

// The advertised fee must be the fee taken, to within the rounding remainder. A pool that
// quietly takes more than its posted rate is the thing miners check for.
func TestTheFeeTakenMatchesTheFeeAdvertised(t *testing.T) {
	shares := map[string]uint64{"a": 3, "b": 5, "c": 7}
	for _, feeRate := range []float64{0, 0.005, 0.01, 0.02} {
		_, fee, err := SplitRound(round(shares, blockReward), feeRate)
		if err != nil {
			t.Fatal(err)
		}
		want := float64(blockReward) * feeRate
		// One nanogrin of rounding per miner is the most the largest-remainder pass can move.
		if math.Abs(float64(fee)-want) > float64(len(shares)+1) {
			t.Errorf("fee %v: took %d nanogrin, advertised %.0f", feeRate, fee, want)
		}
	}
}

// Proportionality: double the shares, double the pay.
func TestPayIsProportionalToShares(t *testing.T) {
	credits, _, err := SplitRound(round(map[string]uint64{"a": 100, "b": 200}, blockReward), 0)
	if err != nil {
		t.Fatal(err)
	}
	byMiner := map[string]uint64{}
	for _, c := range credits {
		byMiner[c.Miner] = c.Amount
	}
	ratio := float64(byMiner["b"]) / float64(byMiner["a"])
	if math.Abs(ratio-2.0) > 1e-6 {
		t.Errorf("b holds twice a's shares but was paid %.6fx", ratio)
	}
}

// Determinism. Map iteration order is randomised in Go; two runs over the same round must
// still hand the odd nanogrin to the same miner, or replaying a round would disagree.
func TestTheSplitIsDeterministic(t *testing.T) {
	shares := map[string]uint64{"a": 7, "b": 11, "c": 13, "d": 17}
	first, firstFee, err := SplitRound(round(shares, blockReward), 0.005)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 50; i++ {
		got, fee, err := SplitRound(round(shares, blockReward), 0.005)
		if err != nil {
			t.Fatal(err)
		}
		if fee != firstFee || len(got) != len(first) {
			t.Fatalf("run %d disagreed on totals", i)
		}
		for j := range got {
			if got[j] != first[j] {
				t.Fatalf("run %d disagreed: %+v vs %+v", i, got[j], first[j])
			}
		}
	}
}

// A round with no shares is refused rather than crashing or paying someone arbitrary.
// Upstream divided by totalShare without checking, so an empty table panicked.
func TestAnEmptyRoundIsRefusedNotDividedByZero(t *testing.T) {
	if _, _, err := SplitRound(round(map[string]uint64{}, blockReward), 0); err != ErrNoShares {
		t.Errorf("want ErrNoShares, got %v", err)
	}
	if _, _, err := SplitRound(round(map[string]uint64{"a": 0}, blockReward), 0); err != ErrNoShares {
		t.Errorf("all-zero shares should be ErrNoShares, got %v", err)
	}
}

// A fee outside [0,1) is a typo, not a policy. 5 instead of 0.05 would pay everyone
// nothing and bank the block.
func TestAnImpossibleFeeIsRefused(t *testing.T) {
	for _, bad := range []float64{-0.01, 1.0, 5} {
		if _, _, err := SplitRound(round(map[string]uint64{"a": 1}, blockReward), bad); err != ErrFeeOutOfRange {
			t.Errorf("fee %v: want ErrFeeOutOfRange, got %v", bad, err)
		}
	}
}

// Maturity is a consensus rule, not a preference: grin will not let the coinbase be spent
// before 1440 blocks, so crediting earlier promises a balance the pool cannot pay.
func TestARoundIsNotMatureBeforeGrinSaysSo(t *testing.T) {
	const h = 4_018_000
	if IsMature(h, h) || IsMature(h, h+CoinbaseMaturity-1) {
		t.Error("a round matured before COINBASE_MATURITY")
	}
	if !IsMature(h, h+CoinbaseMaturity) {
		t.Error("a round failed to mature at exactly COINBASE_MATURITY")
	}
	if CoinbaseMaturity != 1440 {
		t.Errorf("COINBASE_MATURITY is DAY_HEIGHT = 1440 in consensus.rs, got %d", CoinbaseMaturity)
	}
}

// A share count big enough that reward*shares overflows uint64 must still split correctly.
// 60e9 * 3.1e8 already exceeds 2^64, so this is reachable with real share weights.
func TestALargeShareCountDoesNotOverflow(t *testing.T) {
	shares := map[string]uint64{"a": 1 << 40, "b": 1 << 40}
	credits, fee, err := SplitRound(round(shares, blockReward), 0.005)
	if err != nil {
		t.Fatal(err)
	}
	if sum(credits)+fee != blockReward {
		t.Errorf("overflowed: credits+fee = %d, want %d", sum(credits)+fee, blockReward)
	}
	if len(credits) != 2 || credits[0].Amount == 0 || credits[1].Amount == 0 {
		t.Errorf("a miner was paid nothing at large share counts: %+v", credits)
	}
}
