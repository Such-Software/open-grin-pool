package main

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-redis/redis"
)

// Paying out a credited balance.
//
// The ordering here is the whole point. A naive loop debits the balance and then sends; if
// the send fails, or the process dies between the two, the miner's money is gone from the
// ledger and never arrived. Debiting after the send has the opposite failure: a crash after
// sending pays the same balance again on the next pass.
//
// So a payout is reserved first. The amount moves out of the miner's balance and into a
// payout record in one Redis transaction, and only then is anything sent. Every outcome
// leaves the ledger consistent:
//
//   send succeeds  -> record marked sent, amount stays out of the balance
//   send fails     -> amount returned to the balance, record marked failed
//   process dies   -> record sits in `reserved`, visible, and reconciled by hand
//
// A reserved record is the one state a human has to look at, which is correct: it means we
// do not know whether the money left, and guessing either way is worse than saying so.

const (
	balanceKey    = "balance"      // miner -> nanogrin owed
	payoutKey     = "payout:"      // payout:<id> -> PayoutRecord
	payoutIndex   = "payouts"      // sorted set of ids by time
	payoutPending = "payouts:open" // ids not yet resolved
)

// PayoutState is where a payout got to.
type PayoutState string

const (
	// PayoutReserved means the amount has left the balance and we do not yet know whether
	// it was sent. Only a crash leaves a record here; it needs a human.
	PayoutReserved PayoutState = "reserved"
	// PayoutSent means the wallet completed the transaction against the miner's listener.
	PayoutSent PayoutState = "sent"
	// PayoutAwaitingClaim means a slatepack was written and the miner has not returned it.
	// The amount stays out of the balance: it is committed to this payout.
	PayoutAwaitingClaim PayoutState = "awaiting-claim"
	// PayoutFailed means nothing was sent and the amount went back to the balance.
	PayoutFailed PayoutState = "failed"
)

// PayoutRecord is one attempt to pay one miner.
type PayoutRecord struct {
	ID        string      `json:"id"`
	Miner     string      `json:"miner"`
	Amount    uint64      `json:"amount"`
	Transport Transport   `json:"transport"`
	State     PayoutState `json:"state"`
	Slatepack string      `json:"slatepack,omitempty"`
	Detail    string      `json:"detail,omitempty"`
	At        int64       `json:"at"`
}

// Payer turns credited balances into sent transactions.
type Payer struct {
	db        *database
	send      *Sender
	threshold uint64 // nanogrin below which a miner is not paid, to avoid dust-fee payouts
	outDir    string // where slatepack blobs are written for claiming
	now       func() time.Time
}

func NewPayer(db *database, send *Sender, threshold uint64, outDir string) *Payer {
	return &Payer{db: db, send: send, threshold: threshold, outDir: outDir, now: time.Now}
}

// transportFor reads a miner's chosen transport. Tor is the default because it needs
// nothing from the miner at payout time beyond being online, and it is what the pool page
// tells people to expect.
func (p *Payer) transportFor(miner string) Transport {
	v, err := p.db.client.HGet("user:"+miner, "transport").Result()
	if err != nil {
		return TransportTor
	}
	return transportFromStored(v)
}

// due lists miners owed at least the threshold, in a deterministic order so two runs over
// the same ledger attempt the same payouts in the same sequence.
func (p *Payer) due() ([]string, map[string]uint64, error) {
	raw, err := p.db.client.HGetAll(balanceKey).Result()
	if err != nil {
		return nil, nil, fmt.Errorf("read balances: %w", err)
	}
	owed := make(map[string]uint64, len(raw))
	miners := make([]string, 0, len(raw))
	for m, v := range raw {
		n, err := strconv.ParseUint(v, 10, 64)
		if err != nil || n < p.threshold {
			continue
		}
		owed[m] = n
		miners = append(miners, m)
	}
	sort.Strings(miners)
	return miners, owed, nil
}

// reserve moves an amount out of a miner's balance into a payout record, atomically. After
// this returns the money is committed to this payout and is not in the balance.
func (p *Payer) reserve(miner string, amount uint64, t Transport) (*PayoutRecord, error) {
	rec := &PayoutRecord{
		ID:        fmt.Sprintf("%s-%d", miner, p.now().UnixNano()),
		Miner:     miner,
		Amount:    amount,
		Transport: t,
		State:     PayoutReserved,
		At:        p.now().Unix(),
	}
	blob, err := json.Marshal(rec)
	if err != nil {
		return nil, err
	}
	pipe := p.db.client.TxPipeline()
	pipe.HIncrBy(balanceKey, miner, -int64(amount))
	pipe.Set(payoutKey+rec.ID, blob, 0)
	pipe.SAdd(payoutPending, rec.ID)
	if _, err := pipe.Exec(); err != nil {
		return nil, fmt.Errorf("reserve payout for %s: %w", miner, err)
	}
	return rec, nil
}

// settle records the outcome. A failure returns the amount to the balance in the same
// transaction that marks the record, so the ledger is never briefly wrong in either
// direction.
func (p *Payer) settle(rec *PayoutRecord, state PayoutState, detail string) error {
	rec.State, rec.Detail = state, detail
	blob, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	pipe := p.db.client.TxPipeline()
	pipe.Set(payoutKey+rec.ID, blob, 0)
	pipe.ZAdd(payoutIndex, redis.Z{Score: float64(rec.At), Member: rec.ID})
	if state == PayoutFailed {
		pipe.HIncrBy(balanceKey, rec.Miner, int64(rec.Amount))
		pipe.SRem(payoutPending, rec.ID)
	}
	if state == PayoutSent {
		pipe.SRem(payoutPending, rec.ID)
		pipe.HIncrBy("pool", "paidOut", int64(rec.Amount))
	}
	// AwaitingClaim stays in payoutPending: it is not resolved until the miner returns it.
	_, err = pipe.Exec()
	return err
}

// Run pays everyone who is due. Errors on one miner never abandon the rest: a miner whose
// listener is down must not stop the miner behind them in the list from being paid.
func (p *Payer) Run(ctx context.Context) {
	miners, owed, err := p.due()
	if err != nil {
		log.Error("payer: ", err)
		return
	}
	for _, m := range miners {
		t := p.transportFor(m)
		rec, err := p.reserve(m, owed[m], t)
		if err != nil {
			log.Error("payer: ", err)
			continue
		}

		outfile := ""
		if t == TransportSlatepack {
			outfile = filepath.Join(p.outDir, rec.ID+".slatepack")
		}

		out, sendErr := p.send.Send(ctx, t, m, rec.Amount, outfile)
		switch {
		case sendErr != nil:
			log.Warning("payer: ", m, " not paid, amount returned to balance: ", sendErr)
			if err := p.settle(rec, PayoutFailed, sendErr.Error()); err != nil {
				// The money is out of the balance and was not sent. Say so loudly: this is
				// the one case that needs a person.
				log.Error("payer: RESERVED AND UNRESOLVED for ", m, " amount ",
					strconv.FormatUint(rec.Amount, 10), " nanogrin: ", err)
			}
		case t == TransportSlatepack:
			rec.Slatepack = outfile
			if err := p.settle(rec, PayoutAwaitingClaim, "slatepack written"); err != nil {
				log.Error("payer: ", err)
			}
			log.Warning("payer: slatepack for ", m, " awaiting claim at ", outfile)
		default:
			if err := p.settle(rec, PayoutSent, trimForLog(out)); err != nil {
				log.Error("payer: ", err)
			}
			log.Warning("payer: paid ", m, " ", FormatGrin(rec.Amount), " GRIN over tor")
		}
	}
}

func trimForLog(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 400 {
		return s[len(s)-400:]
	}
	return s
}

// The state machine, as predicates, so the money-handling rules are one thing to read and
// one thing to test rather than conditions scattered through settle().

// returnsToBalance reports whether reaching this state puts the amount back in the miner's
// balance. Only failure does: nothing was sent, so the miner is still owed it.
func returnsToBalance(s PayoutState) bool { return s == PayoutFailed }

// resolves reports whether a payout in this state is finished with. An unclaimed slatepack
// is not: the money is committed to it until the miner returns it or it is cancelled.
func resolves(s PayoutState) bool { return s == PayoutSent || s == PayoutFailed }

// needsAttention reports whether a state should be surfaced to an operator rather than
// waited on. Reserved means we do not know whether the money left.
func needsAttention(s PayoutState) bool {
	return s == PayoutReserved || s == PayoutAwaitingClaim
}

// transportFromStored resolves a miner's stored preference, defaulting to tor for anything
// unset or unrecognised rather than guessing at a slower manual path.
func transportFromStored(v string) Transport {
	if Transport(strings.TrimSpace(v)) == TransportSlatepack {
		return TransportSlatepack
	}
	return TransportTor
}
