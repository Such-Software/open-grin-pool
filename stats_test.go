package main

import "testing"

func TestHashrateReadsInGraphsPerSecond(t *testing.T) {
	// Cuckatoo work is quoted in graphs per second, which is the unit a Grin miner reads on
	// their own rig. Reporting hashes would be a different number by orders of magnitude.
	for _, c := range []struct{ g float64; want string }{
		{0.5, "0.5 g/s"}, {1500, "1.50 kg/s"}, {2.5e6, "2.50 Mg/s"}, {4e9, "4.00 Gg/s"},
	} {
		if got := humanGps(c.g); got != c.want {
			t.Errorf("humanGps(%v) = %q, want %q", c.g, got, c.want)
		}
	}
}

func TestStatsCarryTheTermsThePageAdvertises(t *testing.T) {
	// The page states a fee, a threshold and a maturity. Serving them from the same config
	// the daemon runs on means the page cannot drift from what the pool actually does.
	c := &config{}
	c.Payer.Fee = 0.005
	c.Payer.ThresholdGrin = 10
	s := PoolStats{FeePercent: c.Payer.Fee * 100, ThresholdGrin: c.Payer.ThresholdGrin,
		MaturityBlocks: CoinbaseMaturity}
	if s.FeePercent != 0.5 {
		t.Errorf("fee reported as %v%%, want 0.5", s.FeePercent)
	}
	if s.ThresholdGrin != 10 {
		t.Errorf("threshold reported as %v", s.ThresholdGrin)
	}
	if s.MaturityBlocks != 1440 {
		t.Errorf("maturity reported as %d", s.MaturityBlocks)
	}
}
