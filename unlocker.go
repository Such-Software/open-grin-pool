package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"
)

// The maturity watcher.
//
// What upstream had here did not work and could not have. checkMature() never compared
// anything against the chain tip; it fetched the block and returned its own height, so every
// block was "mature" the instant it was found. It read that height with
// headerMap["height"].(int64) when encoding/json decodes every number to float64, so the
// unchecked type assertion panicked on the first block the pool ever found. It called
// /v1/blocks/<hash>, which returns 404 on grin 5.x. And lastMinedInFoundPos was never
// advanced, so it re-read the whole list forever.
//
// This one asks the node two questions instead: how far has the chain got, and does the
// block at our round's height still have our hash.

const foreignAPIPath = "/v2/foreign"

type nodeAPI struct {
	conf *config
	http *http.Client
}

func newNodeAPI(conf *config) *nodeAPI {
	return &nodeAPI{conf: conf, http: &http.Client{Timeout: 20 * time.Second}}
}

func (n *nodeAPI) call(method string, params interface{}, out interface{}) error {
	body, err := json.Marshal(map[string]interface{}{
		"jsonrpc": "2.0", "id": 1, "method": method, "params": params,
	})
	if err != nil {
		return err
	}
	url := fmt.Sprintf("http://%s:%d%s", n.conf.Node.Address, n.conf.Node.APIPort, foreignAPIPath)
	req, err := http.NewRequest("POST", url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.SetBasicAuth(n.conf.Node.AuthUser, n.conf.Node.AuthPass)

	res, err := n.http.Do(req)
	if err != nil {
		return fmt.Errorf("%s: %w", method, err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: node returned %s", method, res.Status)
	}
	raw, err := io.ReadAll(res.Body)
	if err != nil {
		return err
	}

	// grin wraps its v2 results in {"result":{"Ok":...}} or {"result":{"Err":...}}, and a
	// transport-level 200 with an Err inside is still a failure.
	var envelope struct {
		Result struct {
			Ok  json.RawMessage `json:"Ok"`
			Err json.RawMessage `json:"Err"`
		} `json:"result"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return fmt.Errorf("%s: undecodable response: %w", method, err)
	}
	if envelope.Error != nil {
		return fmt.Errorf("%s: %s", method, envelope.Error.Message)
	}
	if len(envelope.Result.Err) > 0 {
		return fmt.Errorf("%s: node error %s", method, string(envelope.Result.Err))
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(envelope.Result.Ok, out)
}

type chainTip struct {
	Height uint64 `json:"height"`
	Hash   string `json:"last_block_pushed_hash"`
}

func (n *nodeAPI) tipHeight() (uint64, error) {
	var tip struct {
		Height uint64 `json:"height"`
	}
	if err := n.call("get_tip", []interface{}{}, &tip); err != nil {
		return 0, err
	}
	return tip.Height, nil
}

// hashAtHeight returns the hash the chain currently holds at a height. Numbers are decoded
// into a typed struct rather than interface{}, so a shape change is an unmarshal error here
// instead of a panic in the watcher.
func (n *nodeAPI) hashAtHeight(height uint64) (string, error) {
	var header struct {
		Height uint64 `json:"height"`
		Hash   string `json:"hash"`
	}
	if err := n.call("get_header", []interface{}{height, nil, nil}, &header); err != nil {
		return "", err
	}
	return header.Hash, nil
}

// heightOfHash resolves the height of a block we just found. The pool relays the node's
// stratum rather than building jobs, so a submission tells us a hash and nothing else; the
// chain is the only place the height lives.
func (n *nodeAPI) heightOfHash(hash string) (uint64, error) {
	var header struct {
		Height uint64 `json:"height"`
		Hash   string `json:"hash"`
	}
	if err := n.call("get_header", []interface{}{nil, hash, nil}, &header); err != nil {
		return 0, err
	}
	return header.Height, nil
}

// rewardAt returns the coinbase a block actually paid: the flat block reward plus the fees
// of the transactions it carried. Asking the chain rather than assuming the flat reward
// means a round is credited for what it earned, fees included.
func (n *nodeAPI) rewardAt(height uint64) (uint64, error) {
	var block struct {
		Kernels []struct {
			Fee uint64 `json:"fee"`
		} `json:"kernels"`
	}
	if err := n.call("get_block", []interface{}{height, nil, nil}, &block); err != nil {
		return 0, err
	}
	total := uint64(BlockReward)
	for _, k := range block.Kernels {
		total += k.Fee
	}
	return total, nil
}

// BlockUnlocker credits rounds the chain has both matured and kept.
type BlockUnlocker struct {
	db   *database
	conf *config
	node *nodeAPI
}

func NewBlockUnlocker(db *database, conf *config) *BlockUnlocker {
	return &BlockUnlocker{db: db, conf: conf, node: newNodeAPI(conf)}
}

// sweep settles every open round the chain has moved past. Errors are logged and left for
// the next pass rather than abandoning the round: a round stays open until it is positively
// credited or positively orphaned, so a node that is briefly unreachable costs nothing.
func (u *BlockUnlocker) sweep() {
	tip, err := u.node.tipHeight()
	if err != nil {
		log.Warning("unlocker: cannot read chain tip, will retry: ", err)
		return
	}

	heights, err := u.db.openRoundHeights()
	if err != nil {
		log.Error("unlocker: cannot list open rounds: ", err)
		return
	}

	for _, h := range heights {
		if !IsMature(h, tip) {
			continue
		}
		rec, err := u.db.getRound(h)
		if err != nil {
			log.Error("unlocker: cannot read round ", h, ": ", err)
			continue
		}

		onChain, err := u.node.hashAtHeight(h)
		if err != nil {
			log.Warning("unlocker: cannot check round ", h, ", will retry: ", err)
			continue
		}
		if onChain != rec.Hash {
			// The chain matured past this height holding someone else's block. There is no
			// coinbase to pay from, so crediting it would promise a balance the pool cannot
			// settle.
			log.Warning("unlocker: round ", h, " was orphaned; chain holds ", onChain,
				" where we found ", rec.Hash)
			if err := u.db.orphanRound(rec); err != nil {
				log.Error("unlocker: cannot mark round ", h, " orphaned: ", err)
			}
			continue
		}

		reward := rec.Reward
		if reward == 0 {
			// Resolved here rather than at block-find time, because by now the block is
			// 1440 deep and its fees are settled fact rather than a template's estimate.
			reward, err = u.node.rewardAt(h)
			if err != nil {
				log.Warning("unlocker: cannot read reward for round ", h, ", will retry: ", err)
				continue
			}
			rec.Reward = reward
		}

		credits, fee, err := SplitRound(&Round{
			Height: rec.Height, Hash: rec.Hash, Finder: rec.Finder,
			Shares: rec.Shares, Reward: reward,
		}, u.conf.Payer.Fee)
		if err != nil {
			// ErrNoShares is the one case that is not a bug: a block found against an empty
			// share table has nobody to pay. The reward stays in the pool wallet rather than
			// going to whoever happens to be mining now.
			log.Error("unlocker: cannot split round ", h, ": ", err)
			if err == ErrNoShares {
				if err := u.db.orphanRound(rec); err != nil {
					log.Error("unlocker: cannot close empty round ", h, ": ", err)
				}
			}
			continue
		}

		if err := u.db.creditRound(rec, credits, fee); err != nil {
			log.Error("unlocker: cannot credit round ", h, ": ", err)
			continue
		}
		log.Warning("unlocker: round ", h, " credited to ", len(credits), " miners, fee ",
			strconv.FormatUint(fee, 10), " nanogrin")
	}
}

func (u *BlockUnlocker) watch() {
	// A grin block is a minute and maturity is 1440 of them, so there is nothing to gain
	// from checking often. Five minutes costs at most five minutes of settlement lag on a
	// reward that has already waited a day.
	tick := time.Tick(5 * time.Minute)
	u.sweep()
	for range tick {
		u.sweep()
	}
}

func initUnlocker(db *database, conf *config) {
	NewBlockUnlocker(db, conf).watch()
}
