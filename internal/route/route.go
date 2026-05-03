// Package route implements path finding for multi-hop token swaps.
//
// # Why multi-hop?
//
// Not every token pair has a direct pool. To swap TOKEN_A for TOKEN_C you
// might need to go: A → B → C (two hops). The route finder explores all
// paths up to a configurable depth and picks the one that maximises output.
//
// # Algorithm
//
// We use a modified Breadth-First Search (BFS) over the "pool graph":
//   - Nodes  = tokens
//   - Edges  = pools (connect Token0 and Token1)
//
// BFS guarantees we find the shortest path (fewest hops) first. For each
// path of equal hop count we compare expected output and keep the best.
//
// # Example
//
//	Find path: WETH → DAI
//	Available pools: WETH/USDC, USDC/DAI, WETH/DAI
//
//	BFS explores:
//	  Depth 1: WETH→DAI (direct, via WETH/DAI pool)
//	  Depth 2: WETH→USDC→DAI (two-hop via WETH/USDC + USDC/DAI pool)
//
//	We calculate output for each and pick the best.
package route

import (
	"errors"
	"fmt"
	"math"

	"github.com/user/go-dex-arbitrage/internal/amm"
	"github.com/user/go-dex-arbitrage/pkg/types"
)

// RouteFinder finds optimal swap paths across a set of known pools.
type RouteFinder struct {
	// Pools is the universe of available liquidity pools.
	// In a real system this would be continuously updated by the monitor.
	Pools []types.Pool

	// MaxHops limits the path length. 1 = direct swap only, 3 = up to 3 hops.
	// Ethereum arbitrage rarely uses more than 3 hops because each hop adds
	// gas cost and slippage.
	MaxHops int

	// MaxSlippage is the maximum acceptable slippage per hop.
	MaxSlippage float64
}

// NewRouteFinder constructs a RouteFinder with sensible defaults.
func NewRouteFinder(pools []types.Pool) *RouteFinder {
	return &RouteFinder{
		Pools:       pools,
		MaxHops:     3,
		MaxSlippage: 0.50, // 50% – permissive default; tighten for production
	}
}

// FindBestRoute returns the route that produces the most tokenOut for the
// given tokenIn amount.
//
// It runs BFS up to MaxHops depth, simulates the swap output at each step,
// and returns the route with the highest final output.
func (rf *RouteFinder) FindBestRoute(
	tokenIn  types.Token,
	tokenOut types.Token,
	amountIn float64,
) ([]types.RouteHop, float64, error) {
	if amountIn <= 0 {
		return nil, 0, errors.New("route: amountIn must be positive")
	}

	type state struct {
		currentToken  types.Token
		currentAmount float64
		hops          []types.RouteHop
	}

	queue := []state{{currentToken: tokenIn, currentAmount: amountIn}}
	bestAmount := 0.0
	var bestHops []types.RouteHop

	for len(queue) > 0 {
		curr := queue[0]
		queue = queue[1:]

		if len(curr.hops) >= rf.MaxHops {
			continue
		}

		// Try every pool that contains the current token
		for _, pool := range rf.Pools {
			var nextToken types.Token
			var reserveIn, reserveOut float64

			// Determine direction: are we swapping token0→token1 or vice-versa?
			if pool.Token0.Address == curr.currentToken.Address {
				nextToken   = pool.Token1
				reserveIn   = pool.Token0.Normalize(pool.Reserve0)
				reserveOut  = pool.Token1.Normalize(pool.Reserve1)
			} else if pool.Token1.Address == curr.currentToken.Address {
				nextToken   = pool.Token0
				reserveIn   = pool.Token1.Normalize(pool.Reserve1)
				reserveOut  = pool.Token0.Normalize(pool.Reserve0)
			} else {
				continue // pool doesn't involve current token
			}

			// Don't revisit tokens (prevents cycles)
			if tokenAlreadyVisited(curr.hops, nextToken) {
				continue
			}

			amountOut, err := amm.CalcOutputV2(curr.currentAmount, reserveIn, reserveOut, pool.Fee)
			if err != nil || amountOut <= 0 {
				continue
			}

			// Prune paths that exceed max slippage per hop
			slip, _ := amm.Slippage(curr.currentAmount, reserveIn, reserveOut, pool.Fee)
			if slip > rf.MaxSlippage {
				continue
			}

			hop := types.RouteHop{
				Pool:      pool,
				TokenIn:   curr.currentToken,
				TokenOut:  nextToken,
				AmountIn:  curr.currentAmount,
				AmountOut: amountOut,
			}
			newHops := append(append([]types.RouteHop{}, curr.hops...), hop)

			if nextToken.Address == tokenOut.Address {
				// Reached the destination – check if this is the best route
				if amountOut > bestAmount {
					bestAmount = amountOut
					bestHops   = newHops
				}
			} else {
				// Continue BFS from the next token
				queue = append(queue, state{
					currentToken:  nextToken,
					currentAmount: amountOut,
					hops:          newHops,
				})
			}
		}
	}

	if len(bestHops) == 0 {
		return nil, 0, fmt.Errorf("route: no path found from %s to %s", tokenIn.Symbol, tokenOut.Symbol)
	}
	return bestHops, bestAmount, nil
}

// tokenAlreadyVisited returns true if token has been used in any prior hop.
// This prevents circular routes like A→B→A→C.
func tokenAlreadyVisited(hops []types.RouteHop, token types.Token) bool {
	for _, h := range hops {
		if h.TokenIn.Address == token.Address {
			return true
		}
	}
	return false
}

// ---- Arbitrage route detection ---------------------------------------------

// ArbRoute describes a complete round-trip arbitrage:
//   tokenIn → (buy on DEX A) → intermediate → (sell on DEX B) → tokenIn
//
// The simplest form is a 2-hop round trip via 2 pools:
//   Pool A: buy tokenOut cheaply
//   Pool B: sell tokenOut for more tokenIn than we started with
type ArbRoute struct {
	BuyPool  types.Pool  // pool where tokenOut is cheap (we buy here)
	SellPool types.Pool  // pool where tokenOut is expensive (we sell here)
	TokenIn  types.Token // capital token (e.g. USDC)
	TokenOut types.Token // intermediate token (e.g. WETH)
}

// FindArbitrageRoutes scans all pairs of pools that share the same token pair
// and returns routes where a price gap exists.
//
// The "price gap" check: if pool A prices tokenOut at priceA and pool B at
// priceB, and priceA < priceB, then buying on A and selling on B is profitable
// before fees and gas.
func FindArbitrageRoutes(pools []types.Pool, minGapPct float64) []ArbRoute {
	var routes []ArbRoute

	// Compare every pair of pools that share the same token0 and token1
	for i := 0; i < len(pools); i++ {
		for j := i + 1; j < len(pools); j++ {
			a, b := pools[i], pools[j]

			// Pools must trade the same token pair (any order)
			if !samePair(a, b) {
				continue
			}

			priceA := a.Price0In1()
			priceB := b.Price0In1()

			if priceA <= 0 || priceB <= 0 {
				continue
			}

			// Calculate gap percentage
			gap := math.Abs(priceA-priceB) / math.Min(priceA, priceB)
			if gap < minGapPct {
				continue
			}

			// Buy on the cheaper pool, sell on the expensive pool
			var buyPool, sellPool types.Pool
			if priceA < priceB {
				buyPool, sellPool = a, b // token0 is cheaper on A
			} else {
				buyPool, sellPool = b, a
			}

			routes = append(routes, ArbRoute{
				BuyPool:  buyPool,
				SellPool: sellPool,
				TokenIn:  buyPool.Token1, // we start with token1 (e.g. USDC)
				TokenOut: buyPool.Token0, // we buy token0 (e.g. WETH)
			})
		}
	}
	return routes
}

// samePair returns true if two pools trade the same tokens (in any order).
func samePair(a, b types.Pool) bool {
	a0, a1 := a.Token0.Symbol, a.Token1.Symbol
	b0, b1 := b.Token0.Symbol, b.Token1.Symbol
	return (a0 == b0 && a1 == b1) || (a0 == b1 && a1 == b0)
}

// SimulateArb simulates a 2-hop round-trip arbitrage and returns the net
// profit in tokenIn units.
//
//  Step 1: swap amountIn of tokenIn → tokenOut on buyPool
//  Step 2: swap tokenOut → tokenIn on sellPool
//  Net:    amountBackIn - amountIn = gross profit
func SimulateArb(route ArbRoute, amountIn float64) (grossProfit float64, err error) {
	// Step 1: buy on the cheap pool
	r0buy := route.BuyPool.Token1.Normalize(route.BuyPool.Reserve1) // reserveIn  for tokenIn
	r1buy := route.BuyPool.Token0.Normalize(route.BuyPool.Reserve0) // reserveOut for tokenOut
	if r0buy <= 0 || r1buy <= 0 {
		return 0, errors.New("route: buy pool has zero reserves")
	}

	tokenOutAmount, err := amm.CalcOutputV2(amountIn, r0buy, r1buy, route.BuyPool.Fee)
	if err != nil {
		return 0, fmt.Errorf("route: step1 swap: %w", err)
	}

	// Step 2: sell on the expensive pool
	r0sell := route.SellPool.Token0.Normalize(route.SellPool.Reserve0) // reserveIn for tokenOut
	r1sell := route.SellPool.Token1.Normalize(route.SellPool.Reserve1) // reserveOut for tokenIn
	if r0sell <= 0 || r1sell <= 0 {
		return 0, errors.New("route: sell pool has zero reserves")
	}

	tokenInBack, err := amm.CalcOutputV2(tokenOutAmount, r0sell, r1sell, route.SellPool.Fee)
	if err != nil {
		return 0, fmt.Errorf("route: step2 swap: %w", err)
	}

	return tokenInBack - amountIn, nil
}
