package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"
)

// Pool stats for the public page.
//
// The inherited /pool handler cannot serve this: it proxies the node's /v1/status, which
// grin 5.x returns 404 for. Everything here comes from sources that work, and a field the
// pool genuinely cannot compute is omitted rather than filled with a zero. A dash on the
// page is honest; a zero is a claim.

// PoolStats is the shape the pool page reads.
type PoolStats struct {
	Height        uint64  `json:"height,omitempty"`
	Miners        int     `json:"miners"`
	BlocksFound   int64   `json:"blocks_found"`
	HashrateGps   float64 `json:"hashrate_gps,omitempty"`
	HashrateHuman string  `json:"hashrate_human,omitempty"`
	FeePercent    float64 `json:"fee_percent"`
	ThresholdGrin float64 `json:"threshold_grin"`
	MaturityBlocks int    `json:"maturity_blocks"`
}

// hashrateWindow is how far back shares are counted when estimating pool hashrate. Long
// enough that one lucky minute does not swing it, short enough to track a rig going offline.
const hashrateWindow = 10 * time.Minute

func (as *apiServer) statsHandler(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("Access-Control-Allow-Origin", "*")
	h.Set("Cache-Control", "no-store")

	s := PoolStats{
		FeePercent:     as.conf.Payer.Fee * 100,
		ThresholdGrin:  as.conf.Payer.ThresholdGrin,
		MaturityBlocks: CoinbaseMaturity,
	}

	// Height from the foreign API, which is the one the node actually serves.
	if tip, err := newNodeAPI(as.conf).tipHeight(); err == nil {
		s.Height = tip
	} else {
		log.Warning("stats: no chain tip: ", err)
	}

	if shares, err := as.db.client.HGetAll(sharesKey).Result(); err == nil {
		s.Miners = len(shares)
		var total float64
		for _, v := range shares {
			if n, err := strconv.ParseFloat(v, 64); err == nil {
				total += n
			}
		}
		// Share difficulty summed over the window, as graphs per second. Cuckatoo work is
		// quoted in graphs, so this is the unit a Grin miner recognises.
		if total > 0 {
			s.HashrateGps = total / hashrateWindow.Seconds()
			s.HashrateHuman = humanGps(s.HashrateGps)
		}
	}

	if blocks, err := as.db.client.HGet("pool", "rewardCredited").Result(); err == nil && blocks != "" {
		// Rounds credited is the honest "blocks found": a block the chain kept and that we
		// actually paid people for, rather than every solution the stratum ever saw.
		if v, err := strconv.ParseUint(blocks, 10, 64); err == nil && v > 0 {
			s.BlocksFound = int64(v / BlockReward)
		}
	}

	raw, err := json.Marshal(s)
	if err != nil {
		http.Error(w, "{}", http.StatusInternalServerError)
		return
	}
	_, _ = w.Write(raw)
}

func humanGps(g float64) string {
	switch {
	case g >= 1e9:
		return fmt.Sprintf("%.2f Gg/s", g/1e9)
	case g >= 1e6:
		return fmt.Sprintf("%.2f Mg/s", g/1e6)
	case g >= 1e3:
		return fmt.Sprintf("%.2f kg/s", g/1e3)
	default:
		return fmt.Sprintf("%.1f g/s", g)
	}
}
