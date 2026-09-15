package main

type payer struct {
	db    *database
	conf  *config
	owner *OwnerAPI
}

type jsonRPCResponse struct {
	ID      string                 `json:"id"`
	JsonRpc string                 `json:"jsonrpc"`
	Method  string                 `json:"method"`
	Result  interface{}            `json:"result"`
	Error   map[string]interface{} `json:"error"`
}

// The balance-delta payer is gone.
//
// It read the wallet's spendable balance on a daily timer and split whatever it had grown
// by across whoever held shares at that moment. Rounds replace that entirely: the unlocker
// credits each miner against the specific block their shares earned, once the chain has
// matured it and still holds our hash. Running both would credit every reward twice, once
// per model.
//
// What remains for this file is the half rounds do not do: taking a credited balance and
// actually sending it, over Tor to a listening wallet or as a slatepack to be claimed. That
// is unwritten, so the payer starts nothing rather than pretending to pay.

func initPayer(db *database, conf *config) *payer {
	log.Warning("payer: automatic payouts are not implemented; balances accrue and must be " +
		"paid by hand. Rounds are credited by the unlocker as blocks mature.")
	return &payer{db: db, conf: conf, owner: NewOwnerAPI(db, conf)}
}
