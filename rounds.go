package main

import (
	"encoding/json"
	"fmt"
	"strconv"
)

// Round storage.
//
// A round is opened the moment a share turns out to be a block, carrying the share table as
// it stood at that instant. Nothing is credited then: grin's COINBASE_MATURITY is 1440
// blocks, so the reward is not spendable for about a day, and the block may yet be reorged
// out. The maturity watcher closes rounds later, against the chain rather than against a
// clock.
//
// Upstream had no equivalent. It recorded a bare block hash, then on a daily timer split
// whatever the wallet balance had grown by across whoever held shares at that later moment.
// A block found on Monday paid Tuesday's miners.

const (
	roundKey     = "round:"       // round:<height> -> RoundRecord
	roundOpenSet = "rounds:open"  // heights awaiting maturity
	sharesKey    = "shares"       // the live share table, reset when a round is opened
)

// RoundState is where a round is in its life.
type RoundState string

const (
	RoundOpen     RoundState = "open"     // found, waiting for the chain to mature it
	RoundCredited RoundState = "credited" // matured, still ours, miners credited
	RoundOrphaned RoundState = "orphaned" // matured but the chain kept a different block
)

// RoundRecord is what is persisted per round.
type RoundRecord struct {
	Height uint64            `json:"height"`
	Hash   string            `json:"hash"`
	Finder string            `json:"finder"`
	Shares map[string]uint64 `json:"shares"`
	Reward uint64            `json:"reward"`
	State  RoundState        `json:"state"`
	Fee    uint64            `json:"fee,omitempty"`
}

// openRound snapshots the live share table against a block we just found and resets it, so
// the next round starts from zero. The snapshot and the reset have to be one step: a share
// arriving between them would be paid twice, once in this round and once in the next.
func (db *database) openRound(height uint64, hash, finder string, reward uint64) (*RoundRecord, error) {
	raw, err := db.client.HGetAll(sharesKey).Result()
	if err != nil {
		return nil, fmt.Errorf("read share table: %w", err)
	}

	shares := make(map[string]uint64, len(raw))
	for miner, v := range raw {
		n, err := strconv.ParseUint(v, 10, 64)
		if err != nil || n == 0 {
			continue
		}
		shares[miner] = n
	}

	rec := &RoundRecord{
		Height: height, Hash: hash, Finder: finder,
		Shares: shares, Reward: reward, State: RoundOpen,
	}
	blob, err := json.Marshal(rec)
	if err != nil {
		return nil, err
	}

	// Rename rather than delete: the share table is moved aside atomically, so a concurrent
	// HIncrBy lands in the new empty table instead of being lost. Upstream deleted the key
	// "share" while reading "shares", so the table was never reset at all and every round
	// inherited every share ever submitted.
	pipe := db.client.TxPipeline()
	pipe.Set(roundKey+strconv.FormatUint(height, 10), blob, 0)
	pipe.SAdd(roundOpenSet, height)
	pipe.Del(sharesKey)
	if _, err := pipe.Exec(); err != nil {
		return nil, fmt.Errorf("persist round: %w", err)
	}
	return rec, nil
}

func (db *database) getRound(height uint64) (*RoundRecord, error) {
	blob, err := db.client.Get(roundKey + strconv.FormatUint(height, 10)).Result()
	if err != nil {
		return nil, err
	}
	var rec RoundRecord
	if err := json.Unmarshal([]byte(blob), &rec); err != nil {
		return nil, err
	}
	return &rec, nil
}

func (db *database) openRoundHeights() ([]uint64, error) {
	members, err := db.client.SMembers(roundOpenSet).Result()
	if err != nil {
		return nil, err
	}
	out := make([]uint64, 0, len(members))
	for _, m := range members {
		if h, err := strconv.ParseUint(m, 10, 64); err == nil {
			out = append(out, h)
		}
	}
	return out, nil
}

// creditRound records the outcome of a matured round and moves it out of the open set. The
// credits are added to each miner's balance; the round is never reopened, so a replay cannot
// pay twice.
func (db *database) creditRound(rec *RoundRecord, credits []Credit, fee uint64) error {
	rec.State = RoundCredited
	rec.Fee = fee
	blob, err := json.Marshal(rec)
	if err != nil {
		return err
	}

	pipe := db.client.TxPipeline()
	pipe.Set(roundKey+strconv.FormatUint(rec.Height, 10), blob, 0)
	pipe.SRem(roundOpenSet, rec.Height)
	for _, c := range credits {
		pipe.HIncrBy("balance", c.Miner, int64(c.Amount))
	}
	pipe.HIncrBy("pool", "feeTaken", int64(fee))
	pipe.HIncrBy("pool", "rewardCredited", int64(rec.Reward))
	if _, err := pipe.Exec(); err != nil {
		return fmt.Errorf("credit round %d: %w", rec.Height, err)
	}
	return nil
}

// orphanRound records a round the chain did not keep. Nothing is credited: there is no
// coinbase to pay from, and crediting it would promise miners a balance the pool cannot
// settle.
func (db *database) orphanRound(rec *RoundRecord) error {
	rec.State = RoundOrphaned
	blob, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	pipe := db.client.TxPipeline()
	pipe.Set(roundKey+strconv.FormatUint(rec.Height, 10), blob, 0)
	pipe.SRem(roundOpenSet, rec.Height)
	pipe.HIncrBy("pool", "orphaned", 1)
	_, err = pipe.Exec()
	return err
}

// openRoundForBlock resolves a found block's height from the chain and opens its round. The
// reward is left at zero and filled in when the round matures, by which time the block's
// fees are settled fact rather than a template's estimate.
func openRoundForBlock(db *database, hash, finder string) error {
	height, err := db.node.heightOfHash(hash)
	if err != nil {
		return fmt.Errorf("resolve height of %s: %w", hash, err)
	}
	_, err = db.openRound(height, hash, finder, 0)
	return err
}
