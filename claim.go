package main

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// Claiming a slatepack.
//
// A slatepack payout is only half a transaction: the pool builds a slate, the miner
// completes it with their own wallet and posts it. This is where they fetch theirs.
//
// There is no authentication and none is needed. A slate paying an address can only be
// completed by whoever holds that address's keys, so handing the blob to the wrong person
// gains them nothing. Requiring a login to fetch it would mean inventing accounts, which is
// the thing this pool does not do.

type claimItem struct {
	ID        string `json:"id"`
	Amount    string `json:"amount_grin"`
	State     string `json:"state"`
	At        int64  `json:"at"`
	Slatepack string `json:"slatepack,omitempty"`
}

// claimsHandler lists what a miner has waiting, and serves the blob itself.
//
//	/claims/{miner}          what is waiting
//	/claims/{miner}/{id}     the slatepack, as text
func (as *apiServer) claimsHandler(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.Trim(strings.TrimPrefix(r.URL.Path, "/claims/"), "/"), "/")
	miner := parts[0]
	if miner == "" {
		http.Error(w, `{"error":"no miner"}`, http.StatusBadRequest)
		return
	}

	ids, err := as.db.client.SMembers(payoutPending).Result()
	if err != nil {
		http.Error(w, `{"error":"unavailable"}`, http.StatusServiceUnavailable)
		return
	}

	var mine []PayoutRecord
	for _, id := range ids {
		blob, err := as.db.client.Get(payoutKey + id).Result()
		if err != nil {
			continue
		}
		var rec PayoutRecord
		if json.Unmarshal([]byte(blob), &rec) != nil || rec.Miner != miner {
			continue
		}
		if rec.State == PayoutAwaitingClaim {
			mine = append(mine, rec)
		}
	}

	// A specific blob.
	if len(parts) > 1 && parts[1] != "" {
		for _, rec := range mine {
			if rec.ID != parts[1] {
				continue
			}
			// Read from the recorded path, but refuse anything that escapes the payout
			// directory: the id comes off the wire and a crafted one must not be able to
			// walk the filesystem.
			clean := filepath.Clean(rec.Slatepack)
			base := filepath.Clean(as.conf.Payer.PayoutDir)
			if !strings.HasPrefix(clean, base+string(os.PathSeparator)) {
				http.Error(w, `{"error":"not available"}`, http.StatusNotFound)
				return
			}
			body, err := os.ReadFile(clean)
			if err != nil {
				http.Error(w, `{"error":"not available"}`, http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.Header().Set("Access-Control-Allow-Origin", "*")
			_, _ = w.Write(body)
			return
		}
		http.Error(w, `{"error":"no such claim"}`, http.StatusNotFound)
		return
	}

	out := make([]claimItem, 0, len(mine))
	for _, rec := range mine {
		out = append(out, claimItem{
			ID: rec.ID, Amount: FormatGrin(rec.Amount), State: string(rec.State), At: rec.At,
		})
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{"miner": miner, "claims": out})
}
