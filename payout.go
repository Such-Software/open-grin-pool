package main

import (
	"errors"
	"math/big"
	"sort"
)

// Payout accounting.
//
// Upstream tied payouts to a wallet balance delta read on a daily timer: ask the owner API
// what is spendable, subtract a reserve, split it across whoever holds shares right now.
// That is a proxy for "blocks matured", and the two come apart in ways that lose money:
//
//   - anything else arriving in the pool wallet is distributed to miners as if it were
//     block reward;
//   - a block found on Monday is paid to whoever happens to hold shares on Tuesday,
//     because Grin's COINBASE_MATURITY is 1440 blocks and the reward only becomes
//     spendable a day later;
//   - a block that is reorged out is still paid, because nothing rechecks it.
//
// So accounting here is per round. A round is opened by a block we found, carries the share
// table as it stood at that moment, and is credited only once the chain both matured it and
// still agrees the block is ours.

// CoinbaseMaturity is grin's consensus COINBASE_MATURITY: DAY_HEIGHT, 1440 blocks of 60
// seconds. core/src/consensus.rs. A coinbase output is not spendable before then, so a
// round cannot be credited before then either.
const CoinbaseMaturity = 1440

// BlockReward is grin's flat block reward in nanogrin: consensus.rs sets
// REWARD = BLOCK_TIME_SEC * GRIN_BASE, sixty seconds times 10^9, and there is no halving.
// A block's actual coinbase is this plus the fees of the transactions it carried.
const BlockReward uint64 = 60 * 1_000_000_000

// Round is one block we found and the shares that earned it.
type Round struct {
	Height uint64            // the block height we solved
	Hash   string            // the block hash, rechecked at maturity against the chain
	Finder string            // the miner whose submission closed it, for the record
	Shares map[string]uint64 // miner identity to share weight, as of the moment it was found
	Reward uint64            // total coinbase in nanogrin, reward plus fees
}

// Credit is one miner's share of one round.
type Credit struct {
	Miner  string
	Amount uint64 // nanogrin
}

var (
	// ErrNoShares is a round nobody can be paid for. It is not a crash: a block found with
	// an empty share table is possible if the table was just reset, and the reward stays in
	// the pool wallet rather than being silently handed to whoever appears next.
	ErrNoShares = errors.New("round has no shares to credit")
	// ErrFeeOutOfRange refuses a fee that is not a fraction. A typo of 5 instead of 0.05
	// would otherwise pay every miner nothing and bank the lot.
	ErrFeeOutOfRange = errors.New("fee must be in [0, 1)")
)

// SplitRound divides a round's reward across its miners in proportion to shares, after the
// pool fee, and returns the credits plus the fee actually taken.
//
// Two properties hold by construction and are asserted in the tests:
//
//	sum(credits) + fee == round.Reward        nothing is created or lost
//	fee >= floor(reward * feeRate)            the advertised fee is never exceeded by more
//	                                          than the rounding remainder
//
// Upstream computed `shares / totalShare * totalRevenue` in uint64, which divides first.
// For every miner holding less than the whole share table that expression is 0 * revenue,
// so every miner was paid exactly nothing unless they were the only miner in the pool.
// Multiplying first overflows uint64 for a large enough share count, so the arithmetic runs
// in big.Int and comes back down once the quotient is known to fit.
func SplitRound(r *Round, feeRate float64) ([]Credit, uint64, error) {
	if feeRate < 0 || feeRate >= 1 {
		return nil, 0, ErrFeeOutOfRange
	}

	var total uint64
	miners := make([]string, 0, len(r.Shares))
	for m, s := range r.Shares {
		if s == 0 {
			continue
		}
		miners = append(miners, m)
		total += s
	}
	if total == 0 {
		return nil, 0, ErrNoShares
	}
	// Deterministic order. Map iteration is randomised in Go, and the largest-remainder
	// pass below has to break ties the same way on every run or two nodes replaying the
	// same round would disagree about who got the odd nanogrin.
	sort.Strings(miners)

	// The pot is what miners divide; the fee is whatever is left after they have taken it.
	// Deriving the fee as a remainder rather than computing it separately is what makes
	// the conservation property hold without a reconciliation step.
	pot := feeFloor(r.Reward, feeRate)

	bigPot := new(big.Int).SetUint64(pot)
	bigTotal := new(big.Int).SetUint64(total)

	type part struct {
		miner     string
		amount    uint64
		remainder *big.Int
	}
	parts := make([]part, 0, len(miners))
	var assigned uint64

	for _, m := range miners {
		num := new(big.Int).Mul(bigPot, new(big.Int).SetUint64(r.Shares[m]))
		q, rem := new(big.Int).QuoRem(num, bigTotal, new(big.Int))
		amt := q.Uint64()
		assigned += amt
		parts = append(parts, part{miner: m, amount: amt, remainder: rem})
	}

	// Largest remainder. Handing the leftover nanogrin to the biggest fractional parts is
	// what makes sum(credits) exactly equal the pot, so no dust accumulates unaccounted in
	// the pool wallet the way it would if we simply truncated and moved on.
	leftover := pot - assigned
	if leftover > 0 {
		order := make([]int, len(parts))
		for i := range order {
			order[i] = i
		}
		sort.SliceStable(order, func(a, b int) bool {
			c := parts[order[a]].remainder.Cmp(parts[order[b]].remainder)
			if c != 0 {
				return c > 0
			}
			return parts[order[a]].miner < parts[order[b]].miner
		})
		for i := uint64(0); i < leftover; i++ {
			parts[order[int(i)%len(order)]].amount++
		}
	}

	credits := make([]Credit, 0, len(parts))
	var paid uint64
	for _, p := range parts {
		if p.amount == 0 {
			continue
		}
		credits = append(credits, Credit{Miner: p.miner, Amount: p.amount})
		paid += p.amount
	}

	return credits, r.Reward - paid, nil
}

// feeFloor returns the miners' share of a reward: the reward less the fee, rounded so the
// pool never takes more than the advertised rate plus one nanogrin of rounding.
func feeFloor(reward uint64, feeRate float64) uint64 {
	if feeRate == 0 {
		return reward
	}
	// Scale to parts per billion and stay in integers from there. float64 multiplication
	// against a nanogrin reward is exact at these magnitudes, but the rate itself is the
	// only float in the calculation and it is converted once.
	ppb := uint64(feeRate*1e9 + 0.5)
	if ppb >= 1e9 {
		return 0
	}
	r := new(big.Int).SetUint64(reward)
	r.Mul(r, new(big.Int).SetUint64(1e9-ppb))
	r.Div(r, big.NewInt(1e9))
	return r.Uint64()
}

// IsMature reports whether a round found at height may be credited, given the chain tip.
// Grin will not let the coinbase be spent before this, so crediting earlier would promise
// miners a balance the pool cannot pay.
func IsMature(roundHeight, tipHeight uint64) bool {
	return tipHeight >= roundHeight+CoinbaseMaturity
}
