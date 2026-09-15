package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Amount formatting is the part of a payout most likely to lose money quietly, so it is
// integer arithmetic and these pin it.
func TestGrinAmountsFormatExactly(t *testing.T) {
	for _, c := range []struct {
		nano uint64
		want string
	}{
		{60_000_000_000, "60"},                  // a whole block reward
		{1_000_000_000, "1"},                    // one grin
		{1, "0.000000001"},                      // one nanogrin, the smallest unit
		{1_500_000_000, "1.5"},                  // trailing zeros trimmed
		{999_999_999, "0.999999999"},            // just under one grin
		{60_000_000_001, "60.000000001"},        // a reward plus a nanogrin
		{76_482_000_000, "76.482"},              // a real wownero-sized figure
		{0, "0"},
	} {
		if got := FormatGrin(c.nano); got != c.want {
			t.Errorf("FormatGrin(%d) = %q, want %q", c.nano, got, c.want)
		}
	}
}

// A float64 would misrender some of these. The test above would catch it, but state the
// property directly: every nanogrin value round-trips through the decimal string.
func TestEveryNanogrinSplitSurvivesFormatting(t *testing.T) {
	for _, nano := range []uint64{1, 7, 999_999_999, 1_000_000_007, 60_000_000_003} {
		s := FormatGrin(nano)
		if strings.Contains(s, "e") || strings.Contains(s, "E") {
			t.Errorf("FormatGrin(%d) = %q used exponent notation", nano, s)
		}
		// Reconstruct and compare, so a dropped digit cannot pass.
		parts := strings.SplitN(s, ".", 2)
		var back uint64
		for _, ch := range parts[0] {
			back = back*10 + uint64(ch-'0')
		}
		back *= nanogrinPerGrin
		if len(parts) == 2 {
			frac := parts[1] + strings.Repeat("0", 9-len(parts[1]))
			var f uint64
			for _, ch := range frac {
				f = f*10 + uint64(ch-'0')
			}
			back += f
		}
		if back != nano {
			t.Errorf("FormatGrin(%d) = %q, which reads back as %d", nano, s, back)
		}
	}
}

func TestTorIsTheDefaultAndSlatepackIsExplicit(t *testing.T) {
	s := &Sender{Bin: "grin-wallet", WalletDir: "/w", MinConf: 10}
	tor := strings.Join(s.args(TransportTor, "grin1abc", 1_500_000_000, "/out/x.slatepack"), " ")
	if strings.Contains(tor, "-m") {
		t.Error("the tor transport passed -m, which stops it attempting tor at all")
	}
	if strings.Contains(tor, "-u") {
		t.Error("the tor transport wrote an outfile; it should complete against the listener")
	}
	if !strings.Contains(tor, "-d grin1abc") || !strings.Contains(tor, "1.5") {
		t.Errorf("tor args lost the destination or amount: %s", tor)
	}

	sp := strings.Join(s.args(TransportSlatepack, "grin1abc", 1_500_000_000, "/out/x.slatepack"), " ")
	if !strings.Contains(sp, "-m") {
		t.Error("the slatepack transport omitted -m, so it would try tor and fail first")
	}
	if !strings.Contains(sp, "-u /out/x.slatepack") {
		t.Errorf("the slatepack transport did not name its outfile: %s", sp)
	}
}

func TestTheConfirmationFloorIsPassedThrough(t *testing.T) {
	// Grin will not spend a coinbase for 1440 blocks. Sending below that fails at the
	// wallet; passing it explicitly means the pool refuses earlier and more clearly.
	s := &Sender{Bin: "grin-wallet", WalletDir: "/w", MinConf: CoinbaseMaturity}
	got := strings.Join(s.args(TransportTor, "grin1abc", 1, ""), " ")
	if !strings.Contains(got, "-c 1440") {
		t.Errorf("min confirmations not passed: %s", got)
	}
	bare := &Sender{Bin: "grin-wallet", WalletDir: "/w"}
	if strings.Contains(strings.Join(bare.args(TransportTor, "grin1abc", 1, ""), " "), "-c") {
		t.Error("an unset MinConf still passed -c, overriding the wallet's own default")
	}
}

// The passphrase must never appear in the command line, because argv is world-readable.
func TestThePassphraseIsNeverAnArgument(t *testing.T) {
	dir := t.TempDir()
	pf := filepath.Join(dir, "wallet.pass")
	if err := os.WriteFile(pf, []byte("correct-horse-battery-staple\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := &Sender{Bin: "grin-wallet", WalletDir: "/w", PassFile: pf}
	for _, tr := range []Transport{TransportTor, TransportSlatepack} {
		joined := strings.Join(s.args(tr, "grin1abc", 1, "/out/x"), " ")
		if strings.Contains(joined, "correct-horse") || strings.Contains(joined, pf) {
			t.Errorf("%s args carry the passphrase or its path: %s", tr, joined)
		}
	}
}

func TestAPayoutWithNothingToSendIsRefused(t *testing.T) {
	s := &Sender{Bin: "/nonexistent", WalletDir: "/w", PassFile: "/nonexistent"}
	if _, err := s.Send(context.Background(), TransportTor, "", 1, ""); err != ErrNoDestination {
		t.Errorf("empty destination: want ErrNoDestination, got %v", err)
	}
	if _, err := s.Send(context.Background(), TransportTor, "   ", 1, ""); err != ErrNoDestination {
		t.Errorf("blank destination: want ErrNoDestination, got %v", err)
	}
	if _, err := s.Send(context.Background(), TransportTor, "grin1abc", 0, ""); err != ErrZeroAmount {
		t.Errorf("zero amount: want ErrZeroAmount, got %v", err)
	}
}

func TestAMissingPassphraseFileNamesItself(t *testing.T) {
	s := &Sender{Bin: "/nonexistent", WalletDir: "/w", PassFile: "/nope/wallet.pass"}
	_, err := s.Send(context.Background(), TransportTor, "grin1abc", 1, "")
	if err == nil || !strings.Contains(err.Error(), "/nope/wallet.pass") {
		t.Errorf("want an error naming the missing file, got %v", err)
	}
}
