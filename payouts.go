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
	// PayoutExpired means an unclaimed slatepack was cancelled at the wallet and the amount
	// returned to the miner's balance. They keep the money; they just have to be paid again.
	PayoutExpired PayoutState = "expired"
)

// PayoutRecord is one attempt to pay one miner.
type PayoutRecord struct {
	ID        string      `json:"id"`
	Miner     string      `json:"miner"`
	Amount    uint64      `json:"amount"`
	Transport Transport   `json:"transport"`
	State     PayoutState `json:"state"`
	Slatepack string      `json:"slatepack,omitempty"`
	TxID      string      `json:"txid,omitempty"`
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

// ThresholdFrom converts the configured payout floor to nanogrin. Rounding is deliberate:
// a floor is a minimum, so a fractional nanogrin rounds up rather than letting a payout
// slip under the published number.
func ThresholdFrom(conf *config) uint64 {
	return uint64(conf.Payer.ThresholdGrin*float64(nanogrinPerGrin) + 0.5)
}

// transportFor reads a miner's chosen transport. Tor is the default because it needs
// nothing from the miner at payout time beyond being online, and it is what the pool page
// tells people to expect.
//
// A miner sets it through the stratum password field, which is otherwise unused: connecting
// with pass "slatepack" records the preference. That keeps the promise of no signup and no
// account, and it means the choice travels with the miner's own config rather than living
// somewhere they cannot reach. Without it the stored field was unreachable and the slatepack
// transport the page advertises could never actually be selected.
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
	if returnsToBalance(state) {
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
			// Without the wallet's transaction id the payout can never be cancelled, so the
			// outputs behind it would stay locked forever. Record it or refuse the payout.
			rec.TxID = SlateIDFrom(out)
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
func returnsToBalance(s PayoutState) bool { return s == PayoutFailed || s == PayoutExpired }

// resolves reports whether a payout in this state is finished with. An unclaimed slatepack
// is not: the money is committed to it until the miner returns it or it is cancelled.
func resolves(s PayoutState) bool {
	return s == PayoutSent || s == PayoutFailed || s == PayoutExpired
}

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


// SlatepackTTL is how long an unclaimed slatepack is held before it is cancelled and the
// amount returned to the miner's balance.
//
// Thirty days is a compromise between two real costs. Too short and a miner who was away
// loses a payout they could have claimed. Too long and the outputs behind every unclaimed
// blob stay locked, so the pool cannot spend them to pay anybody else, and a pool whose
// funds are all committed to abandoned slatepacks cannot pay at all.
const SlatepackTTL = 30 * 24 * time.Hour

// Expired reports whether an awaiting-claim payout has outlived its TTL.
func Expired(rec *PayoutRecord, now time.Time) bool {
	if rec.State != PayoutAwaitingClaim {
		return false
	}
	return now.Sub(time.Unix(rec.At, 0)) >= SlatepackTTL
}

// ExpireStale cancels unclaimed slatepacks past their TTL and returns the amounts.
//
// The order is load-bearing and is the reverse of what reads naturally. Cancel at the wallet
// FIRST: that invalidates the slate the miner is holding, so once it succeeds they cannot
// complete it. Only then credit them back. Crediting first would leave a window in which a
// miner could finalise the slatepack they already have AND hold the returned balance, and
// the pool pays twice.
//
// A cancel that fails leaves the record untouched and awaiting claim, so the next pass tries
// again. That is the safe direction: the miner can still claim, and nothing was double-paid.
func (p *Payer) ExpireStale(ctx context.Context) {
	ids, err := p.db.client.SMembers(payoutPending).Result()
	if err != nil {
		log.Error("payer: cannot list open payouts: ", err)
		return
	}
	now := p.now()
	for _, id := range ids {
		blob, err := p.db.client.Get(payoutKey + id).Result()
		if err != nil {
			continue
		}
		var rec PayoutRecord
		if json.Unmarshal([]byte(blob), &rec) != nil || !Expired(&rec, now) {
			continue
		}
		if rec.TxID == "" {
			log.Error("payer: payout ", id, " is stale but carries no transaction id, so it "+
				"cannot be cancelled; the outputs behind it stay locked until someone "+
				"cancels it by hand")
			continue
		}
		if out, err := p.send.Cancel(ctx, rec.TxID); err != nil {
			log.Warning("payer: could not cancel stale payout ", id, ", will retry: ", err,
				" ", trimForLog(out))
			continue
		}
		if err := p.settle(&rec, PayoutExpired, "unclaimed past TTL, cancelled"); err != nil {
			// Cancelled at the wallet but not credited back. The miner is owed and the
			// ledger does not say so, which needs a person.
			log.Error("payer: CANCELLED BUT NOT CREDITED for ", rec.Miner, " amount ",
				strconv.FormatUint(rec.Amount, 10), " nanogrin: ", err)
			continue
		}
		log.Warning("payer: expired unclaimed slatepack for ", rec.Miner, ", ",
			FormatGrin(rec.Amount), " GRIN returned to balance")
	}
}
