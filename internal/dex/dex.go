// Package dex provides integrations with decentralised exchange protocols.
//
// # Architecture
//
// Each DEX integration implements the DEXClient interface:
//
//	type DEXClient interface {
//	    FetchPool(tokenA, tokenB Token) (Pool, error)
//	    Price(pool Pool, tokenIn Token) (float64, error)
//	}
//
// The two integrations here are:
//   - UniswapClient – queries The Graph (GraphQL) for Uniswap V3 pool state
//   - DyDxClient    – queries the dYdX v3 REST API for perpetual market prices
//
// In a real system you would also subscribe to on-chain events via WebSocket
// (see internal/monitor). Polling is used here for simplicity and testability.
package dex

import (
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"time"

	"github.com/user/go-dex-arbitrage/pkg/types"
)

// DEXClient is the common interface every DEX adapter must implement.
type DEXClient interface {
	// FetchPool returns the current state of the pool for the given pair.
	FetchPool(tokenA, tokenB types.Token) (types.Pool, error)
	// Price returns the price of tokenIn expressed in the other token's units.
	Price(pool types.Pool, tokenIn types.Token) (float64, error)
	// Name returns the human-readable DEX identifier.
	Name() string
}

// ---- Uniswap V3 integration -------------------------------------------------

// UniswapClient fetches pool state from the Uniswap V3 subgraph on The Graph.
//
// The Graph is a decentralised indexing protocol. Uniswap publishes a GraphQL
// endpoint that indexes every pool creation, swap, and liquidity event.
// We query it with a simple GraphQL POST request – no SDK needed.
//
// Endpoint (mainnet): https://api.thegraph.com/subgraphs/name/uniswap/uniswap-v3
// Endpoint (goerli):  https://api.thegraph.com/subgraphs/name/uniswap/uniswap-v3-goerli
//
// Rate limits: The Graph free tier allows ~1000 queries/day. For production
// you would use a dedicated node (Alchemy, Infura) or cache aggressively.
type UniswapClient struct {
	GraphURL   string
	httpClient *http.Client
}

// NewUniswapClient constructs a UniswapClient with a sensible HTTP timeout.
func NewUniswapClient(graphURL string) *UniswapClient {
	return &UniswapClient{
		GraphURL: graphURL,
		httpClient: &http.Client{Timeout: 10 * time.Second},
	}
}

func (u *UniswapClient) Name() string { return "uniswap_v3" }

// uniswapPoolResponse mirrors the GraphQL response schema.
// The Graph returns token0 / token1 sorted by address (lowercase).
type uniswapPoolResponse struct {
	Data struct {
		Pools []struct {
			ID              string `json:"id"`
			Token0          struct {
				ID       string `json:"id"`
				Symbol   string `json:"symbol"`
				Decimals string `json:"decimals"`
			} `json:"token0"`
			Token1          struct {
				ID       string `json:"id"`
				Symbol   string `json:"symbol"`
				Decimals string `json:"decimals"`
			} `json:"token1"`
			FeeTier         string `json:"feeTier"`
			Liquidity       string `json:"liquidity"`
			SqrtPrice       string `json:"sqrtPrice"`
			Token0Price     string `json:"token0Price"` // token1 per token0
			Token1Price     string `json:"token1Price"` // token0 per token1
			TotalValueLockedToken0 string `json:"totalValueLockedToken0"`
			TotalValueLockedToken1 string `json:"totalValueLockedToken1"`
		} `json:"pools"`
	} `json:"data"`
}

// FetchPool queries The Graph for the highest-liquidity pool for the pair.
// The query returns up to 3 pools (one per fee tier) and picks the deepest.
func (u *UniswapClient) FetchPool(tokenA, tokenB types.Token) (types.Pool, error) {
	// Normalise to lowercase for The Graph address comparison
	addrA := strings.ToLower(tokenA.Address)
	addrB := strings.ToLower(tokenB.Address)

	// GraphQL query: find pools where one token is A and the other is B,
	// ordered by TVL descending so we get the most liquid pool first.
	query := fmt.Sprintf(`{
		"query": "{ pools(where: { token0_in: [\"%s\",\"%s\"], token1_in: [\"%s\",\"%s\"] }, orderBy: totalValueLockedUSD, orderDirection: desc, first: 3) { id feeTier liquidity sqrtPrice token0Price token1Price token0 { id symbol decimals } token1 { id symbol decimals } totalValueLockedToken0 totalValueLockedToken1 } }"
	}`, addrA, addrB, addrA, addrB)

	resp, err := u.httpClient.Post(u.GraphURL, "application/json", strings.NewReader(query))
	if err != nil {
		return types.Pool{}, fmt.Errorf("uniswap: graph query failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return types.Pool{}, fmt.Errorf("uniswap: read response: %w", err)
	}

	var result uniswapPoolResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return types.Pool{}, fmt.Errorf("uniswap: parse response: %w", err)
	}
	if len(result.Data.Pools) == 0 {
		return types.Pool{}, fmt.Errorf("uniswap: no pool found for %s/%s", tokenA.Symbol, tokenB.Symbol)
	}

	// Take the first (most liquid) pool
	p := result.Data.Pools[0]

	// Parse reserves from TVL fields (these are human-readable floats in the Graph API)
	// We convert them back to big.Int for consistency with the types.Pool interface.
	r0 := parseBigIntFromFloat(p.TotalValueLockedToken0, tokenA.Decimals)
	r1 := parseBigIntFromFloat(p.TotalValueLockedToken1, tokenB.Decimals)

	feeTier := types.PoolFee(parseUint32(p.FeeTier))
	sqrtPrice, _ := new(big.Int).SetString(p.SqrtPrice, 10)
	liq, _       := new(big.Int).SetString(p.Liquidity,  10)

	return types.Pool{
		Address:      p.ID,
		Token0:       tokenA,
		Token1:       tokenB,
		Reserve0:     r0,
		Reserve1:     r1,
		Fee:          feeTier,
		SqrtPriceX96: sqrtPrice,
		Liquidity:    liq,
		DEX:          u.Name(),
		UpdatedAt:    time.Now(),
	}, nil
}

// Price returns the price of tokenIn expressed in the pool's other token.
func (u *UniswapClient) Price(pool types.Pool, tokenIn types.Token) (float64, error) {
	if pool.Token0.Address == tokenIn.Address {
		return pool.Price0In1(), nil
	}
	if pool.Reserve1 == nil || pool.Reserve1.Sign() == 0 {
		return 0, fmt.Errorf("uniswap: zero reserve1")
	}
	// Invert: price of token1 in token0 terms
	p := pool.Price0In1()
	if p == 0 {
		return 0, fmt.Errorf("uniswap: zero price")
	}
	return 1 / p, nil
}

// ---- dYdX integration -------------------------------------------------------

// DyDxClient fetches perpetual market prices from the dYdX v3 REST API.
//
// dYdX is a decentralised perpetual futures exchange. Unlike Uniswap (spot AMM),
// dYdX uses an order-book model off-chain with on-chain settlement.
//
// For arbitrage purposes, we compare the dYdX mark price (derived from index)
// with Uniswap spot. When they diverge significantly, there may be an arb
// between the spot market and the perpetual.
//
// API docs: https://docs.dydx.exchange/
// Free, no auth required for public market data.
type DyDxClient struct {
	BaseURL    string
	httpClient *http.Client
}

// NewDyDxClient constructs a DyDxClient.
func NewDyDxClient(baseURL string) *DyDxClient {
	return &DyDxClient{
		BaseURL: baseURL,
		httpClient: &http.Client{Timeout: 10 * time.Second},
	}
}

func (d *DyDxClient) Name() string { return "dydx_v3" }

// dydxMarketResponse mirrors the /v3/markets REST endpoint.
type dydxMarketResponse struct {
	Markets map[string]struct {
		Market        string `json:"market"`
		Status        string `json:"status"`
		BaseAsset     string `json:"baseAsset"`
		QuoteAsset    string `json:"quoteAsset"`
		IndexPrice    string `json:"indexPrice"`
		OraclePrice   string `json:"oraclePrice"`
		PriceChange24H string `json:"priceChange24H"`
		Volume24H     string `json:"volume24H"`
		MinOrderSize  string `json:"minOrderSize"`
	} `json:"markets"`
}

// FetchPool constructs a synthetic Pool from dYdX perpetual market data.
// dYdX doesn't have AMM reserves; we synthesise reserves from the oracle price
// and a notional TVL so that CalcOutputV2 can still be called.
// The pool's Fee is set to 0 because dYdX charges maker/taker fees separately.
func (d *DyDxClient) FetchPool(tokenA, tokenB types.Token) (types.Pool, error) {
	marketSymbol := fmt.Sprintf("%s-%s", tokenA.Symbol, tokenB.Symbol)
	url := fmt.Sprintf("%s/v3/markets?market=%s", d.BaseURL, marketSymbol)

	resp, err := d.httpClient.Get(url)
	if err != nil {
		return types.Pool{}, fmt.Errorf("dydx: request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return types.Pool{}, fmt.Errorf("dydx: read response: %w", err)
	}

	var result dydxMarketResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return types.Pool{}, fmt.Errorf("dydx: parse response: %w", err)
	}

	market, ok := result.Markets[marketSymbol]
	if !ok {
		return types.Pool{}, fmt.Errorf("dydx: market %s not found", marketSymbol)
	}

	// Synthesise reserves: assume $10M notional depth on each side.
	// This is a simplification – dYdX is an order book, not an AMM.
	// We use these synthetic reserves so the rest of our AMM maths can apply.
	price := parseFloat64(market.OraclePrice)
	if price == 0 {
		price = parseFloat64(market.IndexPrice)
	}

	notionalDepth := 10_000_000.0 // $10M
	r0 := tokenA.ToRaw(notionalDepth / price) // token0 (base) in raw units
	r1 := tokenB.ToRaw(notionalDepth)          // token1 (quote, USD) in raw units

	return types.Pool{
		Address:   marketSymbol,
		Token0:    tokenA,
		Token1:    tokenB,
		Reserve0:  r0,
		Reserve1:  r1,
		Fee:       types.FeeTier100, // dYdX taker fee ~0.05%; use lowest tier
		DEX:       d.Name(),
		UpdatedAt: time.Now(),
	}, nil
}

// Price returns the oracle/index price of tokenIn expressed in the other token.
func (d *DyDxClient) Price(pool types.Pool, tokenIn types.Token) (float64, error) {
	return pool.Price0In1(), nil
}

// ---- Mock DEX (for testing / backtesting) -----------------------------------

// MockDEXClient is a DEX client that serves fixed pool states.
// Used in unit tests and the backtesting engine to avoid network calls.
type MockDEXClient struct {
	dexName string
	Pools   map[string]types.Pool // key: "TOKEN0/TOKEN1"
}

// NewMockDEXClient creates a mock DEX with pre-loaded pool states.
func NewMockDEXClient(name string, pools []types.Pool) *MockDEXClient {
	m := &MockDEXClient{dexName: name, Pools: make(map[string]types.Pool)}
	for _, p := range pools {
		key := p.Token0.Symbol + "/" + p.Token1.Symbol
		m.Pools[key] = p
	}
	return m
}

func (m *MockDEXClient) Name() string { return m.dexName }

func (m *MockDEXClient) FetchPool(tokenA, tokenB types.Token) (types.Pool, error) {
	key := tokenA.Symbol + "/" + tokenB.Symbol
	if p, ok := m.Pools[key]; ok {
		return p, nil
	}
	// Try reversed pair
	key = tokenB.Symbol + "/" + tokenA.Symbol
	if p, ok := m.Pools[key]; ok {
		return p, nil
	}
	return types.Pool{}, fmt.Errorf("mock: no pool for %s/%s", tokenA.Symbol, tokenB.Symbol)
}

func (m *MockDEXClient) Price(pool types.Pool, tokenIn types.Token) (float64, error) {
	return pool.Price0In1(), nil
}

// SetPrice updates a mock pool's reserves to reflect a given price.
// Useful for testing arbitrage detection when you set different prices on two mocks.
func (m *MockDEXClient) SetPrice(token0, token1 types.Token, price float64, liquidity float64) {
	// Given: price = reserve1_human / reserve0_human
	// Choose reserve0 = liquidity, then reserve1 = liquidity * price
	r0 := token0.ToRaw(liquidity)
	r1 := token1.ToRaw(liquidity * price)
	key := token0.Symbol + "/" + token1.Symbol
	m.Pools[key] = types.Pool{
		Address:   fmt.Sprintf("mock_%s_%s", token0.Symbol, token1.Symbol),
		Token0:    token0,
		Token1:    token1,
		Reserve0:  r0,
		Reserve1:  r1,
		Fee:       types.FeeTier3000,
		DEX:       m.dexName,
		UpdatedAt: time.Now(),
	}
}

// ---- Helpers ----------------------------------------------------------------

func parseBigIntFromFloat(s string, decimals uint8) *big.Int {
	var f float64
	fmt.Sscanf(s, "%f", &f)
	if f == 0 {
		return big.NewInt(0)
	}
	scale := new(big.Float).SetInt(new(big.Int).Exp(
		big.NewInt(10), big.NewInt(int64(decimals)), nil,
	))
	raw, _ := new(big.Float).Mul(big.NewFloat(f), scale).Int(nil)
	return raw
}

func parseFloat64(s string) float64 {
	var f float64
	fmt.Sscanf(s, "%f", &f)
	return f
}

func parseUint32(s string) uint32 {
	var v uint32
	fmt.Sscanf(s, "%d", &v)
	return v
}
