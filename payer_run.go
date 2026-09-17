package main

import (
	"context"
	"os"
	"time"
)

// Running payouts.
//
// Two passes on a timer: pay everyone past the threshold, then cancel slatepacks nobody
// claimed. Both are safe to run repeatedly, because a payout reserves before it sends and an
// expiry cancels before it credits back, so a pass that dies half way leaves the ledger
// consistent and the next pass finishes the job.
//
// Payouts need a wallet that can spend. If the config does not name one, nothing is started
// and the reason is logged once: silently accruing balances that can never leave is worse
// than refusing to start the payer.
func initPayouts(db *database, conf *config) {
	if conf.Payer.WalletBin == "" || conf.Payer.WalletDir == "" || conf.Payer.WalletPassFile == "" {
		log.Warning("payer: not started. payer.wallet_bin, wallet_dir and wallet_pass_file " +
			"must all be set for the pool to send anything; balances will accrue unpaid")
		return
	}
	if _, err := os.Stat(conf.Payer.WalletPassFile); err != nil {
		log.Error("payer: not started. cannot read ", conf.Payer.WalletPassFile, ": ", err)
		return
	}
	if conf.Payer.PayoutDir == "" {
		log.Warning("payer: not started. payer.payout_dir must be set, or a slatepack payout " +
			"has nowhere to be written and the miner can never claim it")
		return
	}
	if err := os.MkdirAll(conf.Payer.PayoutDir, 0o755); err != nil {
		log.Error("payer: not started. cannot create ", conf.Payer.PayoutDir, ": ", err)
		return
	}

	sender := &Sender{
		Bin:       conf.Payer.WalletBin,
		WalletDir: conf.Payer.WalletDir,
		PassFile:  conf.Payer.WalletPassFile,
		MinConf:   CoinbaseMaturity,
		Timeout:   5 * time.Minute,
	}
	payer := NewPayer(db, sender, ThresholdFrom(conf), conf.Payer.PayoutDir)

	log.Warning("payer: started. paying above ", FormatGrin(ThresholdFrom(conf)),
		" GRIN, blobs in ", conf.Payer.PayoutDir)

	// Hourly rather than daily. Coinbase maturity already delays a payout by about a day;
	// adding up to another 24 hours of scheduling lag on top serves nobody, and an hourly
	// pass over a handful of miners costs nothing.
	tick := time.NewTicker(time.Hour)
	defer tick.Stop()
	for {
		ctx := context.Background()
		payer.Run(ctx)
		payer.ExpireStale(ctx)
		<-tick.C
	}
}
