package amm

import (
	"math/big"
	"testing"

	"github.com/user/go-dex-arbitrage/pkg/types"
)

// ---- CalcOutputV2 -----------------------------------------------------------

func TestCalcOutputV2_Basic(t *testing.T) {
	// Classic example: pool has 10 ETH and 25000 USDC.
	// Trader swaps 1 ETH in. Fee = 0.3%.
	//
	// amountInWithFee = 1 * (1 - 0.003) = 0.997
	// amountOut = 25000 * 0.997 / (10 + 0.997) = 24925 / 10.997 ≈ 2266.07
	//
	// Intuition: you get slightly less than 1/10 of the USDC reserve because:
	//  1. You move the price (price impact)
	//  2. The protocol takes a 0.3% fee

	reserveIn  := 10.0
	reserveOut := 25000.0
	amountIn   := 1.0

	out, err := CalcOutputV2(amountIn, reserveIn, reserveOut, types.FeeTier3000)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// At price 2500 USDC/ETH, without fee/impact we'd expect 2500.
	// With fee+impact we expect less.
	if out >= 2500 {
		t.Errorf("expected output < 2500 (fee + impact), got %.4f", out)
	}
	if out <= 0 {
		t.Errorf("expected positive output, got %.4f", out)
	}

	t.Logf("Swap 1 ETH → %.4f USDC (pool: 10 ETH / 25000 USDC, fee=0.3%%)", out)
}

func TestCalcOutputV2_LargeSwapCausesLowOutput(t *testing.T) {
	// Swapping 9 ETH out of a 10 ETH pool is absurd – almost all liquidity gone.
	// The AMM formula still works but gives very poor rate.
	reserveIn  := 10.0
	reserveOut := 25000.0
	amountIn   := 9.0

	out, err := CalcOutputV2(amountIn, reserveIn, reserveOut, types.FeeTier3000)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// If price were 2500 USDC/ETH with no impact, 9 ETH = 22500 USDC.
	// With huge price impact, output should be far less.
	if out >= 22500 {
		t.Errorf("expected output << 22500 due to price impact, got %.2f", out)
	}
	t.Logf("Swap 9 ETH → %.4f USDC (extreme slippage demo)", out)
}

func TestCalcOutputV2_ZeroFee(t *testing.T) {
	// With fee=0 (FeeTier100 is close, but let us test boundary)
	// Use FeeTier100 (0.01%) as lowest available
	out, err := CalcOutputV2(1.0, 100.0, 100.0, types.FeeTier100)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Symmetric pool (100/100), swap 1 → should get slightly less than 1
	if out >= 1.0 {
		t.Errorf("expected output < 1.0 for symmetric pool with fee, got %.6f", out)
	}
}

func TestCalcOutputV2_InvalidInputs(t *testing.T) {
	tests := []struct {
		name       string
		amountIn   float64
		reserveIn  float64
		reserveOut float64
	}{
		{"negative amountIn", -1, 100, 100},
		{"zero amountIn",      0, 100, 100},
		{"zero reserveIn",     1,   0, 100},
		{"zero reserveOut",    1, 100,   0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := CalcOutputV2(tc.amountIn, tc.reserveIn, tc.reserveOut, types.FeeTier3000)
			if err == nil {
				t.Error("expected error, got nil")
			}
		})
	}
}

// ---- CalcOutputExact --------------------------------------------------------

func TestCalcOutputExact_MatchesFloatApproximately(t *testing.T) {
	// Verify that big.Int and float64 versions agree within rounding error.
	//
	// Pool: 10 ETH (18 decimals) / 25000 USDC (6 decimals)
	// We work in raw units:
	//   10 ETH   = 10 * 1e18  raw
	//   25000 USDC = 25000 * 1e6 raw
	//   amountIn = 1 ETH = 1e18 raw

	one := big.NewInt(1)
	reserveIn, _ := new(big.Int).SetString("10000000000000000000", 10) // 10 * 1e18
	reserveOut, _ := new(big.Int).SetString("25000000000", 10)         // 25000 * 1e6
	amountIn     := new(big.Int).Mul(one, big.NewInt(1e18))            // 1 * 1e18

	rawOut, err := CalcOutputExact(amountIn, reserveIn, reserveOut, types.FeeTier3000)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Normalise back to human units:
	// rawOut is in USDC raw (6 decimals): divide by 1e6
	floatOut := new(big.Float).Quo(
		new(big.Float).SetInt(rawOut),
		big.NewFloat(1e6),
	)
	out, _ := floatOut.Float64()

	// Should be ~2266 USDC – must be in reasonable range
	if out < 2200 || out > 2300 {
		t.Errorf("expected ~2266 USDC, got %.2f", out)
	}
	t.Logf("CalcOutputExact: 1 ETH → %.4f USDC", out)
}

// ---- Slippage ---------------------------------------------------------------

func TestSlippage_SmallTrade(t *testing.T) {
	// A 0.001% trade in a large pool should have negligible slippage
	reserveIn  := 10000.0
	reserveOut := 25_000_000.0
	amountIn   := 0.01 // tiny relative to pool

	slip, err := Slippage(amountIn, reserveIn, reserveOut, types.FeeTier3000)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Slippage for a tiny trade should be dominated by the fee (0.3%)
	// and negligible price impact – total ~0.3%
	if slip < 0 || slip > 0.01 { // allow up to 1% for this tiny trade
		t.Errorf("expected slippage ~0.003, got %.6f", slip)
	}
	t.Logf("Small trade slippage: %.4f%%", slip*100)
}

func TestSlippage_LargeTrade(t *testing.T) {
	// 10% of pool → significant slippage
	reserveIn  := 1000.0
	reserveOut := 2_500_000.0
	amountIn   := 100.0 // 10% of reserveIn

	slip, err := Slippage(amountIn, reserveIn, reserveOut, types.FeeTier3000)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// 10% of pool should cause several percent slippage
	if slip < 0.05 {
		t.Errorf("expected slippage > 5%% for 10%% of pool, got %.4f%%", slip*100)
	}
	t.Logf("Large trade (10%% pool) slippage: %.4f%%", slip*100)
}

// ---- Gas accounting ---------------------------------------------------------

func TestGasCostUSD_Reasonable(t *testing.T) {
	// 250k gas * 30 gwei * $2500/ETH
	// = 250000 * 30 * 1e-9 * 2500
	// = 0.0075 ETH * 2500 = $18.75
	cost := GasCostUSD(250_000, 30, 2500)
	expected := 18.75
	if diff := cost - expected; diff > 0.01 || diff < -0.01 {
		t.Errorf("expected $%.2f, got $%.2f", expected, cost)
	}
	t.Logf("Gas cost: $%.2f", cost)
}

// ---- IsArbitrageProfitable --------------------------------------------------

func TestIsArbitrageProfitable(t *testing.T) {
	tests := []struct {
		gross, gas, min float64
		want            bool
	}{
		{50, 18.75, 10, true},  // $50 gross - $18.75 gas = $31.25 net > $10 min
		{20, 18.75, 10, false}, // $20 - $18.75 = $1.25 net < $10 min
		{18, 18.75, 0, false},  // negative net profit
		{10, 0, 5, true},       // no gas, net = $10 > $5
	}
	for _, tc := range tests {
		got := IsArbitrageProfitable(tc.gross, tc.gas, tc.min)
		if got != tc.want {
			t.Errorf("gross=%.2f gas=%.2f min=%.2f: want %v got %v",
				tc.gross, tc.gas, tc.min, tc.want, got)
		}
	}
}

// ---- MaxInputForSlippage ----------------------------------------------------

func TestMaxInputForSlippage(t *testing.T) {
	reserveIn  := 1000.0
	reserveOut := 2_500_000.0
	maxSlip    := 0.005 // 0.5%

	maxIn, err := MaxInputForSlippage(reserveIn, reserveOut, maxSlip, types.FeeTier3000)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if maxIn <= 0 {
		t.Fatal("expected positive max input")
	}

	// Verify the result actually respects the threshold
	actualSlip, err := Slippage(maxIn, reserveIn, reserveOut, types.FeeTier3000)
	if err != nil {
		t.Fatalf("unexpected error verifying: %v", err)
	}
	if actualSlip > maxSlip+0.001 { // 0.1% tolerance for binary search convergence
		t.Errorf("slippage %.4f%% exceeds max %.4f%%", actualSlip*100, maxSlip*100)
	}
	t.Logf("Max input for 0.5%% slippage: %.4f (actual slippage: %.4f%%)", maxIn, actualSlip*100)
}
