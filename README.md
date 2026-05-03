# go-dex-arbitrage

An educational Go project demonstrating DEX arbitrage concepts: monitoring liquidity pools, calculating profitable routes, executing atomic swaps via smart contracts, and backtesting strategies on historical data.

> **This is a learning project, not production software.**
> No real funds are involved. All DEX interactions use mock clients by default.

## Architecture

```
go-dex-arbitrage/
├── cmd/arbitrage/main.go          # CLI: detect / backtest / monitor
│
├── pkg/types/types.go             # Shared types: Token, Pool, Opportunity
│
├── internal/
│   ├── amm/amm.go                 # AMM math: x*y=k formula, slippage, optimal sizing
│   ├── dex/dex.go                 # DEX clients: Uniswap V3, dYdX, Mock
│   ├── route/route.go             # BFS route finder, arbitrage detection
│   ├── detector/detector.go       # Full pipeline: pools → routes → evaluation
│   ├── contract/contract.go       # Go bindings for ArbitrageExecutor contract
│   ├── backtest/backtest.go       # Historical simulation, Sharpe ratio, HTML reports
│   └── monitor/monitor.go         # Live polling loop, event channel
│
├── contracts/
│   └── ArbitrageExecutor.sol      # Solidity: atomic swaps + flash loan arb
│
├── testdata/prices.csv            # Mock historical OHLCV data for backtesting
│
└── docs/
    ├── amm-mechanics.md           # x*y=k derivation, price impact math
    ├── dex-landscape.md           # Uniswap / dYdX / SushiSwap comparison
    ├── arbitrage-strategies.md    # Detection pipeline, profitability, competition
    └── smart-contracts.md         # Solidity patterns, flash loans, ABI/Go bindings
```

### Data Flow

```
DEX APIs (GraphQL / REST)
        │
        ▼
   dex.DEXClient.FetchPools()
        │  []types.Pool
        ▼
   route.FindArbitrageRoutes()    ← BFS over pool graph
        │  []RouteHop
        ▼
   amm.OptimalInputV2()           ← binary search for max profit size
   amm.IsArbitrageProfitable()    ← net profit > gas cost?
        │  []ArbitrageOpportunity
        ▼
   contract.FromOpportunity()     ← encode calldata
        │  ArbitrageParams
        ▼
   TxBuilder.BuildArbitrageTx()   ← sign + broadcast
```

## Learning Goals

After working through this project you will understand:

1. **AMM mathematics** — how constant-product pools price tokens, what slippage is, and how to size trades optimally
2. **DEX architecture** — differences between AMM (Uniswap) and order-book (dYdX) exchanges
3. **Arbitrage mechanics** — why price gaps exist, how to detect and measure them, why most are unprofitable after fees
4. **Smart contracts** — atomicity, reentrancy, flash loans, ABI encoding, Go bindings
5. **MEV** — mempool visibility, sandwich attacks, Flashbots private mempool
6. **Backtesting** — replay historical data, compute Sharpe ratio, identify strategy weaknesses
7. **Go patterns** — interfaces for pluggable clients, channels for event streams, context cancellation

## Quick Start

```bash
# Clone and enter
cd c:/dd/projects/go-dex-arbitrage

# Build
go build ./...

# Run tests
go test ./...

# Detect arbitrage (uses mock data, no API keys needed)
go run ./cmd/arbitrage detect-arbitrage

# Backtest on synthetic data
go run ./cmd/arbitrage backtest-strategy

# Live monitor (mock clients, updates every 10s)
go run ./cmd/arbitrage monitor-live -interval 3s
```

## Using Real DEX APIs

The `internal/dex/dex.go` package includes real clients. Replace mock clients in `cmd/arbitrage/main.go`:

```go
// Uniswap V3 (The Graph subgraph)
uniswap := dex.NewUniswapClient(
    "https://api.thegraph.com/subgraphs/name/uniswap/uniswap-v3",
)

// dYdX v3 (public REST API, no key required)
dydx := dex.NewDyDxClient("https://api.dydx.exchange")

mon := monitor.New(cfg, 10*time.Second, uniswap, dydx)
```

No API keys required for public subgraphs or dYdX.

## Configuration

`pkg/types/types.go` contains `DefaultConfig()` with sensible defaults:

| Parameter       | Default | Meaning                                   |
|-----------------|---------|-------------------------------------------|
| `MinProfitUSD`  | 0.01    | Skip opportunities below this net profit  |
| `MaxSlippage`   | 0.5%    | Reject routes with higher slippage        |
| `MaxHops`       | 3       | Maximum swaps in a route                  |
| `GasLimitGwei`  | 20      | Assumed gas price for profitability check |
| `MinSpreadBps`  | 10      | Min price gap in basis points             |

## Testing

```bash
# All tests
go test ./...

# With verbose output
go test -v ./internal/amm/...
go test -v ./internal/route/...

# With coverage
go test -cover ./...
```

Key test files:
- `internal/amm/amm_test.go` — tests CalcOutputV2, Slippage, OptimalInput
- `internal/route/route_test.go` — tests BFS routing and arb detection

## Gotchas (Learn These Early)

1. **Integer overflow**: ERC-20 amounts use `uint256` (18 decimal places). Always use `math/big` for production calculations. `float64` loses precision beyond ~15 significant digits.

2. **Decimals**: `1 WETH = 1e18 wei`. Never display raw amounts to users without dividing by `10^decimals`.

3. **Price vs reserves**: Pool price is NOT stored directly; it's derived from reserves: `price = reserve1 / reserve0`.

4. **Gas underestimation**: On-chain gas varies. Simulate via `eth_estimateGas` before broadcasting, and add 20% buffer.

5. **MEV**: In mainnet, if your tx is in the public mempool, bots will see it and likely frontrun the opportunity. Use Flashbots for private submission.

6. **Testnet vs mainnet**: Testnet prices are fake. Backtests on testnet data mean nothing for mainnet strategy development.

7. **Block timing**: Ethereum blocks are ~12 seconds. A 1.5% price gap seen at T=0 may be 0.3% by T=12s when your tx actually executes.

## Key Dependencies

No external dependencies are required to build and run this project.
All DEX interactions use mock clients by default.

To use real Ethereum interactions (not included in this educational version):

```
github.com/ethereum/go-ethereum   — Ethereum client, ABI encoding, key management
github.com/shopspring/decimal     — Arbitrary-precision decimal arithmetic
```

## Documentation

- [AMM Mechanics](docs/amm-mechanics.md) — Mathematical derivation of x*y=k, slippage, price impact
- [DEX Landscape](docs/dex-landscape.md) — Comparison of Uniswap, dYdX, SushiSwap, Curve
- [Arbitrage Strategies](docs/arbitrage-strategies.md) — Pipeline, profitability, competition
- [Smart Contracts](docs/smart-contracts.md) — Solidity patterns, flash loans, Go ABI bindings
