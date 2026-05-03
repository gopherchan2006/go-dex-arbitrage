package route

import (
	"testing"

	"github.com/user/go-dex-arbitrage/pkg/types"
)

// buildTestPool is a helper that creates a Pool with given human-readable reserves.
func buildTestPool(addr, dex string, t0, t1 types.Token, r0Human, r1Human float64, fee types.PoolFee) types.Pool {
	return types.Pool{
		Address:  addr,
		Token0:   t0,
		Token1:   t1,
		Reserve0: t0.ToRaw(r0Human),
		Reserve1: t1.ToRaw(r1Human),
		Fee:      fee,
		DEX:      dex,
	}
}

func TestFindBestRoute_DirectSwap(t *testing.T) {
	weth := types.Token{Symbol: "WETH", Address: "0xWETH", Decimals: 18}
	usdc := types.Token{Symbol: "USDC", Address: "0xUSDC", Decimals: 6}

	// One direct pool: 10 ETH / 25000 USDC → price = 2500 USDC/ETH
	pool := buildTestPool("pool1", "uniswap", weth, usdc, 10, 25000, types.FeeTier3000)

	rf := NewRouteFinder([]types.Pool{pool})
	hops, amountOut, err := rf.FindBestRoute(weth, usdc, 1.0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(hops) != 1 {
		t.Errorf("expected 1 hop, got %d", len(hops))
	}
	if amountOut <= 0 || amountOut >= 2500 {
		t.Errorf("expected 0 < amountOut < 2500, got %.4f", amountOut)
	}
	t.Logf("Direct route: 1 WETH → %.4f USDC", amountOut)
}

func TestFindBestRoute_TwoHop(t *testing.T) {
	weth := types.Token{Symbol: "WETH", Address: "0xWETH", Decimals: 18}
	usdc := types.Token{Symbol: "USDC", Address: "0xUSDC", Decimals: 6}
	dai  := types.Token{Symbol: "DAI",  Address: "0xDAI",  Decimals: 18}

	// No direct WETH/DAI pool – must go through USDC
	poolWETHUSDC := buildTestPool("p1", "uniswap", weth, usdc, 100, 250000, types.FeeTier3000)
	poolUSDCDAI2 := types.Pool{
		Address:  "p2",
		Token0:   usdc,
		Token1:   dai,
		Reserve0: usdc.ToRaw(500000),
		Reserve1: dai.ToRaw(500000), // 1:1 stablecoin pool
		Fee:      types.FeeTier500,
		DEX:      "uniswap",
	}

	rf := NewRouteFinder([]types.Pool{poolWETHUSDC, poolUSDCDAI2})
	hops, amountOut, err := rf.FindBestRoute(weth, dai, 1.0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(hops) != 2 {
		t.Errorf("expected 2 hops (WETH→USDC→DAI), got %d", len(hops))
	}
	// Should get ~2500 DAI for 1 WETH (after fees)
	if amountOut < 2000 || amountOut > 2500 {
		t.Errorf("expected ~2400-2499 DAI, got %.4f", amountOut)
	}
	t.Logf("Two-hop route: 1 WETH → %.4f DAI (via USDC)", amountOut)
}

func TestFindBestRoute_NoPossiblePath(t *testing.T) {
	weth := types.Token{Symbol: "WETH", Address: "0xWETH", Decimals: 18}
	dai  := types.Token{Symbol: "DAI",  Address: "0xDAI",  Decimals: 18}
	usdc := types.Token{Symbol: "USDC", Address: "0xUSDC", Decimals: 6}

	// Only WETH/USDC pool – no path to DAI
	pool := buildTestPool("p1", "uniswap", weth, usdc, 10, 25000, types.FeeTier3000)
	rf := NewRouteFinder([]types.Pool{pool})

	_, _, err := rf.FindBestRoute(weth, dai, 1.0)
	if err == nil {
		t.Error("expected error for unreachable token, got nil")
	}
}

func TestFindArbitrageRoutes_DetectsGap(t *testing.T) {
	weth := types.Token{Symbol: "WETH", Address: "0xWETH", Decimals: 18}
	usdc := types.Token{Symbol: "USDC", Address: "0xUSDC", Decimals: 6}

	// Pool A: WETH cheaper (price = 2400 USDC/WETH)
	poolA := buildTestPool("poolA", "uniswap",    weth, usdc, 10, 24000, types.FeeTier3000)
	// Pool B: WETH more expensive (price = 2600 USDC/WETH)
	poolB := buildTestPool("poolB", "sushiswap",  weth, usdc, 10, 26000, types.FeeTier3000)

	routes := FindArbitrageRoutes([]types.Pool{poolA, poolB}, 0.01)
	if len(routes) == 0 {
		t.Fatal("expected at least one arbitrage route, got none")
	}

	r := routes[0]
	// Buy side should be the cheaper pool (poolA, price=2400)
	if r.BuyPool.Address != "poolA" {
		t.Errorf("expected buy on poolA (2400), got %s", r.BuyPool.Address)
	}
	t.Logf("Found arb: buy on %s (%s), sell on %s (%s)",
		r.BuyPool.DEX, r.BuyPool.Address,
		r.SellPool.DEX, r.SellPool.Address)
}

func TestSimulateArb_ProfitableGap(t *testing.T) {
	weth := types.Token{Symbol: "WETH", Address: "0xWETH", Decimals: 18}
	usdc := types.Token{Symbol: "USDC", Address: "0xUSDC", Decimals: 6}

	// Large deep pools: 8.3% gap. Small input = minimal price impact.
	poolA := buildTestPool("poolA", "uniswap",   weth, usdc, 10000, 24000000, types.FeeTier3000)
	poolB := buildTestPool("poolB", "sushiswap", weth, usdc, 10000, 26000000, types.FeeTier3000)

	route := ArbRoute{
		BuyPool:  poolA,
		SellPool: poolB,
		TokenIn:  usdc,
		TokenOut: weth,
	}

	// Small input (100 USDC into 24M reserve) = ~0.0004% price impact
	profit, err := SimulateArb(route, 100)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	t.Logf("Arb profit: %.4f USDC on 100 USDC input (%.3f%%)", profit, profit/100*100)

	// With an 8.3% gap and tiny price impact, fees (2×0.3%) leave ~7.7% profit
	if profit <= 0 {
		t.Errorf("expected positive profit, got %.4f", profit)
	}
}

func TestSimulateArb_NoProfit(t *testing.T) {
	weth := types.Token{Symbol: "WETH", Address: "0xWETH", Decimals: 18}
	usdc := types.Token{Symbol: "USDC", Address: "0xUSDC", Decimals: 6}

	// Identical pools – no gap, fees eat everything
	poolA := buildTestPool("poolA", "uniswap",   weth, usdc, 100, 250000, types.FeeTier3000)
	poolB := buildTestPool("poolB", "sushiswap", weth, usdc, 100, 250000, types.FeeTier3000)

	route := ArbRoute{
		BuyPool:  poolA,
		SellPool: poolB,
		TokenIn:  usdc,
		TokenOut: weth,
	}

	profit, err := SimulateArb(route, 1000)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// No gap → fees make profit negative
	if profit > 0 {
		t.Errorf("expected zero or negative profit for equal pools, got %.4f", profit)
	}
	t.Logf("Identical pools: profit = %.4f USDC (expected ≤ 0)", profit)
}
