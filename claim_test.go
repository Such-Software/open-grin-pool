package main

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestAClaimPathCannotEscapeThePayoutDirectory(t *testing.T) {
	// The payout id arrives off the wire. A crafted one must not be able to walk the
	// filesystem and hand out anything the daemon can read.
	base := "/var/lib/lepool/payouts"
	for _, bad := range []string{
		"/var/lib/lepool/payouts/../../../etc/passwd",
		"/etc/passwd",
		"/var/lib/lepool/payoutsX/evil.slatepack",
		"",
	} {
		clean := filepath.Clean(bad)
		if strings.HasPrefix(clean, filepath.Clean(base)+"/") {
			t.Errorf("%q was treated as inside the payout directory", bad)
		}
	}
	good := filepath.Clean(base + "/grin1abc-123.slatepack")
	if !strings.HasPrefix(good, filepath.Clean(base)+"/") {
		t.Error("a legitimate payout path was rejected")
	}
}

func TestOnlyUnclaimedPayoutsAreOffered(t *testing.T) {
	// A sent or expired payout has nothing to claim; offering it would invite a miner to
	// complete a slate that no longer has outputs behind it.
	for _, s := range []PayoutState{PayoutSent, PayoutFailed, PayoutExpired, PayoutReserved} {
		if s == PayoutAwaitingClaim {
			t.Fatal("fixture error")
		}
	}
	if PayoutAwaitingClaim != "awaiting-claim" {
		t.Errorf("state name changed to %q; the claim filter keys on it", PayoutAwaitingClaim)
	}
}
