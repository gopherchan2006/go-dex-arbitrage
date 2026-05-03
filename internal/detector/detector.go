// Package detector implements the arbitrage opportunity detection engine.
//
// The detector's job is simple: given the current state of multiple DEX pools,
// find all pairs of pools trading the same tokens at different prices, and
// rank them by expected profitability after gas and fees.
//
// # Detection pipeline
//
//  1. Collect pool states from all configured DEX clients
//  2. Group pools by (token0, token1) pair
//  3. For each group with ≥2 pools: compare prices
//  4. For gaps above threshold: simulate optimal arbitrage input
//  5. Subtract gas cost to get net profit
//  6. Rank and filter by net profit ≥ minimum
package detector

import (
	"fmt"
	"log"
	"math"
	"sort"
	"time"

	"github.com/user/go-dex-arbitrage/internal/amm"
	"github.com/user/go-dex-arbitrage/internal/route"
	"github.com/user/go-dex-arbitrage/pkg/types"
)

// Detector scans DEX pools for arbitrage opportunities.
type Detector struct {
	Config types.Config
	// ETHPriceUSD is used to convert gas costs from ETH to USD.
	// In production, fetch this from a price oracle.
	ETHPriceUSD float64
}

// NewDetector creates a Detector with default ETH price.
func NewDetector(cfg types.Config) *Detector {
	return &Detector{Config: cfg, ETHPriceUSD: 2500}
}

// Detect runs the full detection pipeline over the provided pool list.
// Returns opportunities sorted by NetProfitUSD descending.
func (d *Detector) Detect(pools []types.Pool) []types.ArbitrageOpportunity {
	arbRoutes := route.FindArbitrageRoutes(pools, 0.001) // 0.1% minimum gap to consider

	var opportunities []types.ArbitrageOpportunity
	for _, r := range arbRoutes {
		opp, err := d.evaluateRoute(r)
		if err != nil {
			log.Printf("detector: skip route %s/%s: %v",
				r.BuyPool.Address, r.SellPool.Address, err)
			continue
		}
		if opp.NetProfitUSD >= d.Config.MinProfitUSD {
			opportunities = append(opportunities, opp)
		}
	}

	// Sort by net profit descending
	sort.Slice(opportunities, func(i, j int) bool {
		return opportunities[i].NetProfitUSD > opportunities[j].NetProfitUSD
	})
	return opportunities
}

// evaluateRoute calculates the expected profit for a single arbitrage route.
func (d *Detector) evaluateRoute(r route.ArbRoute) (types.ArbitrageOpportunity, error) {
	// Find optimal input amount using the analytic formula
	optimalInput, err := amm.OptimalInputV2(r.BuyPool, r.SellPool)
	if err != nil || optimalInput <= 0 {
		return types.ArbitrageOpportunity{}, fmt.Errorf("no optimal input: %w", err)
	}

	// Cap at configured maximum
	maxInput := d.Config.MaxInputUSD / d.buyPoolPrice(r)
	inputAmount := math.Min(optimalInput, maxInput)
	if inputAmount <= 0 {
		return types.ArbitrageOpportunity{}, fmt.Errorf("input amount is zero")
	}

	// Simulate the round-trip
	grossProfit, err := route.SimulateArb(r, inputAmount)
	if err != nil {
		return types.ArbitrageOpportunity{}, fmt.Errorf("simulate: %w", err)
	}

	// Convert gross profit to USD (profit is in TokenIn units = USDC in typical case)
	grossProfitUSD := grossProfit * d.tokenPriceUSD(r.TokenIn)

	// Gas cost
	gasCost := amm.GasCostUSD(
		d.Config.GasLimitSwap,
		d.Config.GasPriceGwei,
		d.ETHPriceUSD,
	)

	netProfitUSD := grossProfitUSD - gasCost
	profitPct := 0.0
	if inputAmount > 0 {
		profitPct = grossProfit / inputAmount * 100
	}

	// Slippage of the buy leg
	var slip float64
	if r.BuyPool.Reserve1 != nil && r.BuyPool.Reserve0 != nil {
		r1 := r.BuyPool.Token1.Normalize(r.BuyPool.Reserve1)
		r0 := r.BuyPool.Token0.Normalize(r.BuyPool.Reserve0)
		slip, _ = amm.Slippage(inputAmount, r1, r0, r.BuyPool.Fee)
	}

	// Execution difficulty: scale with profit (more profit = more competition)
	difficulty := 1
	if netProfitUSD > 1000 {
		difficulty = 8
	} else if netProfitUSD > 100 {
		difficulty = 5
	} else if netProfitUSD > 10 {
		difficulty = 3
	}

	id := fmt.Sprintf("arb_%s_%s_%d", r.BuyPool.DEX, r.SellPool.DEX, time.Now().UnixNano())

	return types.ArbitrageOpportunity{
		ID:      id,
		TokenIn: r.TokenIn,
		TokenOut: r.TokenOut,
		Route: []types.RouteHop{
			{Pool: r.BuyPool, TokenIn: r.TokenIn, TokenOut: r.TokenOut, AmountIn: inputAmount},
			{Pool: r.SellPool, TokenIn: r.TokenOut, TokenOut: r.TokenIn},
		},
		InputAmount:         inputAmount,
		OutputAmount:        inputAmount + grossProfit,
		GrossProfit:         grossProfit,
		GasCostUSD:          gasCost,
		NetProfitUSD:        netProfitUSD,
		ProfitPct:           profitPct,
		Slippage:            slip,
		ExecutionDifficulty: difficulty,
		DetectedAt:          time.Now(),
	}, nil
}

// buyPoolPrice returns the price of TokenOut in TokenIn terms for the buy side.
func (d *Detector) buyPoolPrice(r route.ArbRoute) float64 {
	p := r.BuyPool.Price0In1()
	if p <= 0 {
		return 1
	}
	return p
}

// tokenPriceUSD returns an estimated USD price for the given token.
// In a real system, query a price oracle (Chainlink, Uniswap TWAP, etc.).
func (d *Detector) tokenPriceUSD(t types.Token) float64 {
	switch t.Symbol {
	case "WETH", "ETH":
		return d.ETHPriceUSD
	case "USDC", "USDT", "DAI":
		return 1.0
	case "WBTC":
		return 45000 // rough estimate
	default:
		return 1.0
	}
}

// ---- Report -----------------------------------------------------------------

// Report prints a human-readable summary of detected opportunities.
func Report(opps []types.ArbitrageOpportunity) string {
	if len(opps) == 0 {
		return "No profitable arbitrage opportunities found.\n"
	}

	out := fmt.Sprintf("=== Arbitrage Opportunities (%d found) ===\n\n", len(opps))
	for i, o := range opps {
		buyDex  := o.Route[0].Pool.DEX
		sellDex := o.Route[1].Pool.DEX
		out += fmt.Sprintf(
			"#%d  %s/%s\n"+
				"    Buy on:      %-15s  Sell on: %s\n"+
				"    Input:       %.4f %s\n"+
				"    Gross:       $%.2f  Gas: $%.2f  Net: $%.2f\n"+
				"    Profit%%:    %.3f%%  Slippage: %.3f%%  Difficulty: %d/10\n\n",
			i+1,
			o.TokenIn.Symbol, o.TokenOut.Symbol,
			buyDex, sellDex,
			o.InputAmount, o.TokenIn.Symbol,
			o.GrossProfit*1, o.GasCostUSD, o.NetProfitUSD,
			o.ProfitPct, o.Slippage*100, o.ExecutionDifficulty,
		)
	}
	return out
}
