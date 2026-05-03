// Package types defines shared data structures used across all modules
// of the go-dex-arbitrage project.
//
// Design note: keeping types in a separate package prevents import cycles
// and makes the dependency graph easy to understand.
package types

import (
	"math/big"
	"time"
)

// ---- Token ------------------------------------------------------------------

// Token represents an ERC-20 token on an EVM-compatible blockchain.
// Decimals matters because on-chain all values are integers (no floats).
// ETH has 18 decimals, USDC has 6 – always normalise before comparing.
type Token struct {
	Symbol   string // e.g. "ETH", "USDC"
	Address  string // hex address: "0xC02aaa…" (WETH on mainnet)
	Decimals uint8  // scaling factor: real_amount = raw / 10^decimals
	ChainID  int64  // 1 = Ethereum mainnet, 5 = Goerli, 137 = Polygon
}

// Normalize converts a raw on-chain integer amount to a human-readable float.
// Example: raw=1_000_000, decimals=6  →  1.0 (USDC)
func (t Token) Normalize(raw *big.Int) float64 {
	if raw == nil {
		return 0
	}
	divisor := new(big.Float).SetInt(new(big.Int).Exp(
		big.NewInt(10), big.NewInt(int64(t.Decimals)), nil,
	))
	result, _ := new(big.Float).Quo(new(big.Float).SetInt(raw), divisor).Float64()
	return result
}

// ToRaw converts a human-readable amount back to an on-chain integer.
func (t Token) ToRaw(amount float64) *big.Int {
	scale := new(big.Float).SetInt(new(big.Int).Exp(
		big.NewInt(10), big.NewInt(int64(t.Decimals)), nil,
	))
	raw, _ := new(big.Float).Mul(big.NewFloat(amount), scale).Int(nil)
	return raw
}

// ---- Pool -------------------------------------------------------------------

// PoolFee represents the fee tier of a Uniswap V3 pool.
// Uniswap V3 has three tiers: 0.01%, 0.05%, 0.30%, 1.00%.
// The fee is expressed in "hundredths of a bip" (1 bip = 0.01%).
// So fee=3000 means 0.30%.
type PoolFee uint32

const (
	FeeTier100  PoolFee = 100   // 0.01% – stable pairs like USDC/USDT
	FeeTier500  PoolFee = 500   // 0.05% – low-volatility pairs
	FeeTier3000 PoolFee = 3000  // 0.30% – standard pairs (original Uniswap)
	FeeTier10000 PoolFee = 10000 // 1.00% – exotic/volatile pairs
)

// FeeFraction converts the fee tier to a decimal multiplier.
// e.g. FeeTier3000.Fraction() → 0.003
func (f PoolFee) Fraction() float64 {
	return float64(f) / 1_000_000
}

// Pool represents the current state of an AMM liquidity pool.
// In Uniswap V2 this is simply Reserve0/Reserve1.
// In Uniswap V3 each "tick" has its own concentrated liquidity position –
// we simplify by storing the "active" reserves at the current tick.
type Pool struct {
	Address  string  // contract address
	Token0   Token   // the "base" token (lower address lexicographically)
	Token1   Token   // the "quote" token
	Reserve0 *big.Int // raw amount of Token0 in the pool
	Reserve1 *big.Int // raw amount of Token1 in the pool
	Fee      PoolFee  // fee tier for this pool
	DEX      string   // "uniswap_v2", "uniswap_v3", "sushiswap", "dydx"
	// SqrtPriceX96 is the V3-style price encoding. Q64.96 fixed-point.
	// Price = (SqrtPriceX96 / 2^96)^2
	// Included for completeness; our simplified model uses Reserve0/1.
	SqrtPriceX96 *big.Int
	Liquidity    *big.Int // active liquidity at current tick
	UpdatedAt    time.Time
}

// Price0In1 returns the price of Token0 expressed in Token1 units,
// normalising for decimal differences.
// Example: ETH/USDC pool → Price0In1() ≈ 2500 (2500 USDC per ETH)
func (p *Pool) Price0In1() float64 {
	if p.Reserve0 == nil || p.Reserve1 == nil ||
		p.Reserve0.Sign() == 0 {
		return 0
	}
	// Normalise both reserves to "human" units first.
	r0 := p.Token0.Normalize(p.Reserve0)
	r1 := p.Token1.Normalize(p.Reserve1)
	if r0 == 0 {
		return 0
	}
	return r1 / r0
}

// ---- Arbitrage opportunity --------------------------------------------------

// ArbitrageOpportunity describes a detected price discrepancy between two
// pools/DEXs for the same token pair.
type ArbitrageOpportunity struct {
	ID           string    // unique identifier (generated at detection time)
	TokenIn      Token     // token we start with
	TokenOut     Token     // token we end with (same as TokenIn for round-trip)
	Route        []RouteHop // ordered hops: buy on BuyPool, sell on SellPool
	InputAmount  float64   // human-readable amount we put in
	OutputAmount float64   // expected amount we receive back
	GrossProfit  float64   // OutputAmount - InputAmount (in TokenIn units)
	GasCostUSD   float64   // estimated USD cost of the transaction
	NetProfitUSD float64   // GrossProfit (converted to USD) - GasCostUSD
	ProfitPct    float64   // NetProfitUSD / InputAmount * 100
	Slippage     float64   // estimated price impact [0,1]
	DetectedAt   time.Time
	// ExecutionDifficulty scores how competitive this opportunity is.
	// 1 = easy (small, not widely watched), 10 = very competitive.
	ExecutionDifficulty int
}

// IsProfit returns true when net profit is positive after gas.
func (a *ArbitrageOpportunity) IsProfit() bool {
	return a.NetProfitUSD > 0
}

// RouteHop represents a single swap step within an arbitrage route.
type RouteHop struct {
	Pool      Pool
	TokenIn   Token
	TokenOut  Token
	AmountIn  float64
	AmountOut float64
}

// ---- Backtest result --------------------------------------------------------

// BacktestResult contains aggregate statistics from a backtesting run.
type BacktestResult struct {
	StartTime      time.Time
	EndTime        time.Time
	TotalTrades    int
	WinningTrades  int
	LosingTrades   int
	WinRate        float64 // WinningTrades / TotalTrades
	TotalProfit    float64 // sum of net profits (USD)
	MaxDrawdown    float64 // worst peak-to-trough equity decline (USD)
	SharpeRatio    float64 // risk-adjusted return
	AverageProfit  float64 // per winning trade
	AverageLoss    float64 // per losing trade
	Opportunities  []ArbitrageOpportunity
}

// ---- Config -----------------------------------------------------------------

// Config holds all runtime configuration.
// Values can be loaded from a YAML file or environment variables.
type Config struct {
	// Network
	RPCURL      string `yaml:"rpc_url"`      // e.g. "https://eth-mainnet.g.alchemy.com/v2/KEY"
	WSS         string `yaml:"wss"`          // WebSocket endpoint
	ChainID     int64  `yaml:"chain_id"`     // 1 mainnet, 5 goerli, 137 polygon
	NetworkName string `yaml:"network_name"` // "mainnet", "goerli"

	// Arbitrage parameters
	MinProfitUSD    float64 `yaml:"min_profit_usd"`    // skip opportunities below this
	MaxSlippage     float64 `yaml:"max_slippage"`      // e.g. 0.005 = 0.5%
	MaxInputUSD     float64 `yaml:"max_input_usd"`     // capital limit per trade
	GasPriceGwei    float64 `yaml:"gas_price_gwei"`    // manual override; 0 = fetch from RPC
	GasLimitSwap    uint64  `yaml:"gas_limit_swap"`    // estimated gas units for a 2-hop swap

	// Monitoring
	PollIntervalSec int    `yaml:"poll_interval_sec"` // how often to query DEX state
	UniswapGraphURL string `yaml:"uniswap_graph_url"` // The Graph endpoint
	DyDxAPIURL     string `yaml:"dydx_api_url"`

	// Backtesting
	HistoricalDataPath string `yaml:"historical_data_path"`
	BacktestStartTime  string `yaml:"backtest_start_time"` // RFC3339
	BacktestEndTime    string `yaml:"backtest_end_time"`

	// Wallet (testnet only – never commit real keys)
	PrivateKeyHex   string `yaml:"private_key_hex"`   // hex, no 0x prefix
	WalletAddress   string `yaml:"wallet_address"`    // 0x...
}

// DefaultConfig returns a safe testnet-friendly configuration.
func DefaultConfig() Config {
	return Config{
		RPCURL:          "https://goerli.infura.io/v3/YOUR_KEY",
		WSS:             "wss://goerli.infura.io/ws/v3/YOUR_KEY",
		ChainID:         5,
		NetworkName:     "goerli",
		MinProfitUSD:    10.0,
		MaxSlippage:     0.005,
		MaxInputUSD:     1000.0,
		GasLimitSwap:    250_000,
		GasPriceGwei:    30,
		PollIntervalSec: 10,
		UniswapGraphURL: "https://api.thegraph.com/subgraphs/name/uniswap/uniswap-v3",
		DyDxAPIURL:     "https://api.dydx.exchange",
	}
}

// ---- Well-known tokens (Ethereum mainnet) -----------------------------------

var (
	WETH = Token{Symbol: "WETH", Address: "0xC02aaA39b223FE8D0A0e5C4F27eAD9083C756Cc2", Decimals: 18, ChainID: 1}
	USDC = Token{Symbol: "USDC", Address: "0xA0b86991c6218b36c1d19D4a2e9Eb0cE3606eB48", Decimals: 6, ChainID: 1}
	USDT = Token{Symbol: "USDT", Address: "0xdAC17F958D2ee523a2206206994597C13D831ec7", Decimals: 6, ChainID: 1}
	DAI  = Token{Symbol: "DAI",  Address: "0x6B175474E89094C44Da98b954EedeAC495271d0F", Decimals: 18, ChainID: 1}
	WBTC = Token{Symbol: "WBTC", Address: "0x2260FAC5E5542a773Aa44fBCfeDf7C193bc2C599", Decimals: 8, ChainID: 1}
)

// GoerliTokens are the test-network equivalents used for testnet demos.
var (
	GoerliWETH = Token{Symbol: "WETH", Address: "0xB4FBF271143F4FBf7B91A5ded31805e42b2208d6", Decimals: 18, ChainID: 5}
	GoerliUSDC = Token{Symbol: "USDC", Address: "0x07865c6E87B9F70255377e024ace6630C1Eaa37F", Decimals: 6, ChainID: 5}
)
