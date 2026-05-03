// Package amm implements the mathematics of Automated Market Makers (AMMs).
//
// # What is an AMM?
//
// Traditional order-book exchanges match buyers with sellers at specific price
// levels. An AMM replaces the order book with a mathematical formula.
//
// Uniswap V2 uses the "constant product" formula:
//
//	x * y = k
//
// where x = reserve of token A, y = reserve of token B, k = constant.
//
// When a trader swaps Δx of token A in, they receive Δy of token B out such
// that the product remains at least k after the fee is applied:
//
//	(x + Δx_with_fee) * (y - Δy) = k
//
// # Why this matters for arbitrage
//
// Each pool independently maintains its own reserves. If ETH/USDC on Uniswap
// says ETH = $2500 but the same pair on SushiSwap says ETH = $2520, there is
// a $20 price gap. An arbitrageur can buy cheap on Uniswap and sell expensive
// on SushiSwap, pocketing the difference (minus fees and gas).
//
// But beware: the act of trading moves the price toward equilibrium.
// The larger the trade, the more "slippage" you incur. This module quantifies
// exactly how much slippage you get for a given input amount.
package amm

import (
	"errors"
	"fmt"
	"math"
	"math/big"

	"github.com/user/go-dex-arbitrage/pkg/types"
)

// ---- Core formulas ----------------------------------------------------------

// CalcOutputV2 calculates how many units of tokenOut you receive when swapping
// amountIn of tokenIn through a Uniswap V2-style constant-product pool.
//
// The formula (after fee):
//
//	amountInWithFee = amountIn * (1 - fee)
//	amountOut = reserveOut * amountInWithFee / (reserveIn + amountInWithFee)
//
// All values are already normalised to human units (float64, not wei).
// For production, use big.Int arithmetic to avoid rounding – see CalcOutputExact.
func CalcOutputV2(amountIn, reserveIn, reserveOut float64, fee types.PoolFee) (float64, error) {
	if amountIn <= 0 {
		return 0, errors.New("amm: amountIn must be positive")
	}
	if reserveIn <= 0 || reserveOut <= 0 {
		return 0, errors.New("amm: pool reserves must be positive")
	}

	feeFraction := fee.Fraction()
	// amountIn after the fee is deducted
	amountInWithFee := amountIn * (1 - feeFraction)

	// Constant product formula solved for amountOut:
	// (reserveIn + amountInWithFee) * (reserveOut - amountOut) = reserveIn * reserveOut
	// => amountOut = reserveOut * amountInWithFee / (reserveIn + amountInWithFee)
	amountOut := reserveOut * amountInWithFee / (reserveIn + amountInWithFee)
	return amountOut, nil
}

// CalcOutputExact performs the same calculation using big.Int arithmetic,
// which is how Uniswap's Solidity contracts do it to avoid integer truncation.
//
// The Uniswap V2 contract uses integer arithmetic with a 997/1000 fee
// for the 0.3% tier (997 = 1000 - 3):
//
//	amountOut = amountIn * 997 * reserveOut / (reserveIn * 1000 + amountIn * 997)
func CalcOutputExact(amountIn, reserveIn, reserveOut *big.Int, fee types.PoolFee) (*big.Int, error) {
	if amountIn == nil || reserveIn == nil || reserveOut == nil {
		return nil, errors.New("amm: nil argument")
	}
	if amountIn.Sign() <= 0 || reserveIn.Sign() <= 0 || reserveOut.Sign() <= 0 {
		return nil, errors.New("amm: all amounts must be positive")
	}

	// Express fee as numerator / 1_000_000
	// e.g. fee=3000 → feeNum=3000, denom=1_000_000
	// amountInAfterFee = amountIn * (1_000_000 - fee) / 1_000_000
	feeNum := big.NewInt(int64(fee))
	denom   := big.NewInt(1_000_000)
	scale   := new(big.Int).Sub(denom, feeNum) // (1_000_000 - fee)

	// numerator = amountIn * scale * reserveOut
	num := new(big.Int).Mul(amountIn, scale)
	num.Mul(num, reserveOut)

	// denominator = reserveIn * 1_000_000 + amountIn * scale
	den := new(big.Int).Mul(reserveIn, denom)
	den.Add(den, new(big.Int).Mul(amountIn, scale))

	if den.Sign() == 0 {
		return nil, errors.New("amm: division by zero")
	}

	return new(big.Int).Div(num, den), nil
}

// ---- Slippage ---------------------------------------------------------------

// Slippage measures how much the execution price deviates from the "mid price"
// (i.e. the price you would get for an infinitesimally small trade).
//
// Slippage = 1 - (executionPrice / midPrice)
//
// A slippage of 0.005 means you paid 0.5% more (or received 0.5% less) than
// the theoretical no-impact price. Higher slippage = bigger trade relative to
// pool size.
func Slippage(amountIn, reserveIn, reserveOut float64, fee types.PoolFee) (float64, error) {
	midPrice := reserveOut / reserveIn // price with zero impact, no fee
	if midPrice == 0 {
		return 0, errors.New("amm: midPrice is zero")
	}

	amountOut, err := CalcOutputV2(amountIn, reserveIn, reserveOut, fee)
	if err != nil {
		return 0, err
	}

	// Execution price: how many units of tokenOut per unit of tokenIn
	execPrice := amountOut / amountIn

	// Slippage relative to mid price (negative means worse execution)
	slippage := 1 - (execPrice / midPrice)
	return slippage, nil
}

// MaxInputForSlippage calculates the maximum amountIn that keeps slippage
// below the given threshold.
//
// Derivation: slippage <= s
//   execPrice / midPrice >= 1 - s
//   (amountOut/amountIn) / (reserveOut/reserveIn) >= 1 - s
//
// Solved numerically with a binary search (analytic solution is messier).
func MaxInputForSlippage(reserveIn, reserveOut, maxSlippage float64, fee types.PoolFee) (float64, error) {
	if maxSlippage <= 0 || maxSlippage >= 1 {
		return 0, fmt.Errorf("amm: maxSlippage must be in (0,1), got %f", maxSlippage)
	}

	lo, hi := 0.0, reserveIn*0.5 // never try to drain more than half the pool
	for i := 0; i < 64; i++ {
		mid := (lo + hi) / 2
		s, err := Slippage(mid, reserveIn, reserveOut, fee)
		if err != nil || math.IsNaN(s) {
			hi = mid
			continue
		}
		if s < maxSlippage {
			lo = mid // can go larger
		} else {
			hi = mid // too much slippage
		}
	}
	return lo, nil
}

// ---- Price impact -----------------------------------------------------------

// PriceImpact is the percentage change in the marginal price of a pool
// caused by a swap. Unlike slippage (which measures output quality),
// price impact measures how much the pool price has shifted.
//
// After the swap:  new_price = (reserveIn + amountIn) / (reserveOut - amountOut)
// Before:          old_price =  reserveIn / reserveOut
//
// Impact = (new_price - old_price) / old_price
func PriceImpact(amountIn, reserveIn, reserveOut float64, fee types.PoolFee) (float64, error) {
	oldPrice := reserveIn / reserveOut
	if oldPrice == 0 {
		return 0, errors.New("amm: reserveOut is zero")
	}

	amountOut, err := CalcOutputV2(amountIn, reserveIn, reserveOut, fee)
	if err != nil {
		return 0, err
	}

	newReserveIn  := reserveIn + amountIn
	newReserveOut := reserveOut - amountOut
	if newReserveOut <= 0 {
		return 0, errors.New("amm: cannot drain pool completely")
	}
	newPrice := newReserveIn / newReserveOut

	return (newPrice - oldPrice) / oldPrice, nil
}

// ---- Gas accounting ---------------------------------------------------------

// GasCostUSD estimates the USD cost of a transaction given gas price and
// current ETH price.
//
// Cost in ETH = gasLimit * gasPriceGwei * 1e-9
// Cost in USD = Cost in ETH * ethPriceUSD
//
// This is critical for arbitrage: a $5 profit evaporates if gas costs $8.
func GasCostUSD(gasLimit uint64, gasPriceGwei, ethPriceUSD float64) float64 {
	costETH := float64(gasLimit) * gasPriceGwei * 1e-9
	return costETH * ethPriceUSD
}

// IsArbitrageProfitable returns whether an arbitrage is worth executing after
// accounting for gas costs.
//
// grossProfit: raw profit in USD before gas
// gasCostUSD:  estimated transaction cost in USD
// minProfit:   minimum acceptable net profit to proceed
func IsArbitrageProfitable(grossProfitUSD, gasCostUSD, minProfitUSD float64) bool {
	net := grossProfitUSD - gasCostUSD
	return net >= minProfitUSD
}

// ---- Optimal input calculation ----------------------------------------------

// OptimalInputV2 calculates the theoretically optimal input amount that
// maximises profit when arbitraging between two V2-style pools.
//
// Derivation uses the classic formula for two constant-product AMMs:
//
//	r0a = reserve of tokenIn  on pool A (buy side)
//	r1a = reserve of tokenOut on pool A
//	r0b = reserve of tokenOut on pool B (sell side – denominated in tokenIn)
//	r1b = reserve of tokenIn  on pool B
//
// Optimal input = sqrt(r0a * r0b * r1a * r1b) - r0a * r0b / (r1a + r0b)
//
// (Ignoring fees for simplicity; fees lower the effective reserves.)
//
// Returns 0 if no profitable input exists (pools are in equilibrium).
func OptimalInputV2(
	buyPool  types.Pool, // pool where tokenIn is cheap
	sellPool types.Pool, // pool where tokenIn is expensive
) (float64, error) {
	if buyPool.Reserve0 == nil || buyPool.Reserve1 == nil ||
		sellPool.Reserve0 == nil || sellPool.Reserve1 == nil {
		return 0, errors.New("amm: nil pool reserves")
	}

	ra := buyPool.Token0.Normalize(buyPool.Reserve0)  // token0 on buy pool
	rb := buyPool.Token0.Normalize(buyPool.Reserve1)  // token1 on buy pool
	rc := sellPool.Token0.Normalize(sellPool.Reserve0) // token0 on sell pool
	rd := sellPool.Token0.Normalize(sellPool.Reserve1) // token1 on sell pool

	if ra <= 0 || rb <= 0 || rc <= 0 || rd <= 0 {
		return 0, errors.New("amm: zero reserve in pool")
	}

	// Apply fee adjustments (simplification: use effective reserves)
	feeA := 1 - buyPool.Fee.Fraction()
	feeB := 1 - sellPool.Fee.Fraction()

	ra *= feeA
	rc *= feeB

	num := math.Sqrt(ra * rb * rc * rd)
	den := rb + rc
	if den == 0 {
		return 0, errors.New("amm: denominator zero in optimal input")
	}

	optimal := (num - ra*rc/1) / den // simplified; full derivation in docs/amm-mechanics.md
	if optimal < 0 {
		return 0, nil // no profitable input (pools in equilibrium)
	}
	return optimal, nil
}
