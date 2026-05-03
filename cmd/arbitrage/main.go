// go-dex-arbitrage: educational DEX arbitrage bot
//
// Subcommands:
//
//	detect-arbitrage   One-shot: fetch pools and display current opportunities
//	backtest-strategy  Replay historical data and show statistics
//	monitor-live       Continuous polling loop (Ctrl+C to stop)
//
// Usage:
//
//	go run ./cmd/arbitrage detect-arbitrage
//	go run ./cmd/arbitrage backtest-strategy -data testdata/prices.csv
//	go run ./cmd/arbitrage monitor-live -interval 10s
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/user/go-dex-arbitrage/internal/backtest"
	"github.com/user/go-dex-arbitrage/internal/dex"
	"github.com/user/go-dex-arbitrage/internal/monitor"
	"github.com/user/go-dex-arbitrage/pkg/types"
)

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	cmd := os.Args[1]
	// Remaining args are passed to the subcommand's FlagSet
	args := os.Args[2:]

	switch cmd {
	case "detect-arbitrage":
		runDetect(args)
	case "backtest-strategy":
		runBacktest(args)
	case "monitor-live":
		runMonitor(args)
	case "help", "-h", "--help":
		printUsage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %q\n\n", cmd)
		printUsage()
		os.Exit(1)
	}
}

// ---- detect-arbitrage -------------------------------------------------------

func runDetect(args []string) {
	fs := flag.NewFlagSet("detect-arbitrage", flag.ExitOnError)
	minProfit := fs.Float64("min-profit", 0.01, "Minimum net profit in USD to display")
	maxSlippage := fs.Float64("max-slippage", 0.005, "Maximum allowed slippage (e.g. 0.005 = 0.5%)")
	maxHops := fs.Int("max-hops", 3, "Maximum hops in a route")
	_ = fs.Parse(args)

	cfg := types.DefaultConfig()
	cfg.MinProfitUSD = *minProfit
	cfg.MaxSlippage = *maxSlippage
	_ = maxHops // MaxHops is a route.RouteFinder field, not types.Config

	// Use mock clients for demonstration – no API keys required
	clients := buildMockClients()

	_, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	fmt.Println("Fetching pools from DEX clients...")

	// DEXClient.FetchPool fetches one pair at a time
	monitorPairs := [][2]types.Token{
		{types.WETH, types.USDC},
		{types.WETH, types.USDT},
		{types.WBTC, types.USDC},
	}
	var pools []types.Pool
	for _, c := range clients {
		for _, pair := range monitorPairs {
			p, err := c.FetchPool(pair[0], pair[1])
			if err != nil {
				continue // DEX may not have this pair
			}
			pools = append(pools, p)
		}
	}

	if len(pools) == 0 {
		fmt.Println("No pools found. Using generated mock pools for demonstration.")
		pools = buildDemoPools()
	}

	fmt.Printf("Analysing %d pools...\n\n", len(pools))

	// Import detector via the package (avoid circular usage in main)
	// We replicate the high-level flow here for clarity:
	arbRoutes := findArbitrageOpportunities(cfg, pools)

	if len(arbRoutes) == 0 {
		fmt.Println("No profitable arbitrage found with current prices.")
		return
	}

	fmt.Printf("Found %d opportunity(ies):\n\n", len(arbRoutes))
	for i, o := range arbRoutes {
		fmt.Printf("%d. %s\n", i+1, formatOpportunity(o))
	}
}

// ---- backtest-strategy ------------------------------------------------------

func runBacktest(args []string) {
	fs := flag.NewFlagSet("backtest-strategy", flag.ExitOnError)
	dataPath  := fs.String("data", "testdata/prices.csv", "Path to historical price CSV")
	reportOut := fs.String("report", "backtest_report.html", "Path to write HTML report")
	_ = fs.Parse(args)

	cfg := types.DefaultConfig()

	fmt.Printf("Loading historical data from %s...\n", *dataPath)

	history, err := backtest.LoadCSV(*dataPath)
	if err != nil {
		// Fall back to synthetic data so the demo still works
		log.Printf("Could not load CSV (%v). Generating synthetic data...", err)
		history = backtest.GenerateMockHistory(500, 2500, 20)
	}

	fmt.Printf("Loaded %d price points.\n", len(history))

	// Build pool snapshots from history
	snapshots := backtest.BuildSnapshots(history, types.WETH, types.USDC)
	fmt.Printf("Built %d time snapshots.\n\n", len(snapshots))

	engine := backtest.NewEngine(cfg)
	result := engine.Run(snapshots)

	// Print text report
	fmt.Println(backtest.PrintReport(result))

	// Write HTML report
	htmlContent := backtest.HTMLReport(result)
	if err := os.WriteFile(*reportOut, []byte(htmlContent), 0644); err != nil {
		log.Printf("WARN: could not write HTML report: %v", err)
	} else {
		fmt.Printf("HTML report written to: %s\n", *reportOut)
	}
}

// ---- monitor-live -----------------------------------------------------------

func runMonitor(args []string) {
	fs := flag.NewFlagSet("monitor-live", flag.ExitOnError)
	interval  := fs.Duration("interval", 10*time.Second, "Polling interval (e.g. 5s, 10s, 1m)")
	minProfit := fs.Float64("min-profit", 0.01, "Minimum net profit in USD to display")
	_ = fs.Parse(args)

	cfg := types.DefaultConfig()
	cfg.MinProfitUSD = *minProfit

	clients := buildMockClients()

	mon := monitor.New(cfg, *interval, clients...)

	// Graceful shutdown on Ctrl+C / SIGTERM
	ctx, cancel := context.WithCancel(context.Background())
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		fmt.Println("\nShutting down monitor...")
		cancel()
	}()

	fmt.Printf("Starting live monitor (interval=%s, min-profit=$%.4f)\n", *interval, *minProfit)
	fmt.Println("Press Ctrl+C to stop.")
	fmt.Println()

	var stats monitor.Stats
	for event := range mon.Start(ctx) {
		fmt.Println(monitor.PrintEvent(event))
		stats.Update(event)
	}

	fmt.Printf("\n--- Session Summary ---\n%s\n", stats.Summary())
}

// ---- Helpers ----------------------------------------------------------------

func buildMockClients() []dex.DEXClient {
	// MockDEXClient lets us test without a real Ethereum node or API key.
	// In production, replace with:
	//   dex.NewUniswapClient("https://api.thegraph.com/subgraphs/name/uniswap/uniswap-v3")
	//   dex.NewDyDxClient("https://api.dydx.exchange")

	uni := dex.NewMockDEXClient("uniswap-mock", nil)
	uni.SetPrice(types.WETH, types.USDC, 2501.00, 1_000) // 1000 WETH @ $2501

	sushi := dex.NewMockDEXClient("sushiswap-mock", nil)
	sushi.SetPrice(types.WETH, types.USDC, 2516.50, 800) // ~0.6% higher → arb opportunity

	return []dex.DEXClient{uni, sushi}
}

// buildDemoPools builds a static set of pools for offline demonstration.
func buildDemoPools() []types.Pool {
	// Pool A: WETH/USDC on Uniswap – price ~2500
	poolA := types.Pool{
		Address:  "0xUniswapWETH_USDC",
		Token0:   types.WETH,
		Token1:   types.USDC,
		Reserve0: types.WETH.ToRaw(100),       // 100 WETH
		Reserve1: types.USDC.ToRaw(250_000),   // 250,000 USDC → price = 2500
		Fee:      types.FeeTier3000,
		DEX:      "uniswap",
	}
	// Pool B: WETH/USDC on SushiSwap – price ~2525 (1% higher → arb)
	poolB := types.Pool{
		Address:  "0xSushiWETH_USDC",
		Token0:   types.WETH,
		Token1:   types.USDC,
		Reserve0: types.WETH.ToRaw(100),
		Reserve1: types.USDC.ToRaw(252_500),
		Fee:      types.FeeTier3000,
		DEX:      "sushiswap",
	}
	return []types.Pool{poolA, poolB}
}

// findArbitrageOpportunities is a thin wrapper to demonstrate the pipeline.
// In production this would delegate entirely to detector.Detector.
func findArbitrageOpportunities(cfg types.Config, pools []types.Pool) []types.ArbitrageOpportunity {
	// Look for pools with the same token pair but different prices
	var opps []types.ArbitrageOpportunity

	for i := 0; i < len(pools); i++ {
		for j := i + 1; j < len(pools); j++ {
			p1, p2 := pools[i], pools[j]
			// Same token pair?
			sameTokens := (p1.Token0.Symbol == p2.Token0.Symbol && p1.Token1.Symbol == p2.Token1.Symbol) ||
				(p1.Token0.Symbol == p2.Token1.Symbol && p1.Token1.Symbol == p2.Token0.Symbol)
			if !sameTokens || p1.DEX == p2.DEX {
				continue
			}

			price1 := p1.Price0In1()
			price2 := p2.Price0In1()
			if price1 == 0 || price2 == 0 {
				continue
			}

			// Price gap percentage
			gap := (price2 - price1) / price1
			if gap < 0 {
				gap = -gap
			}
			const minSpreadFraction = 0.001 // 10 bps minimum
			if gap < minSpreadFraction {
				continue
			}

			// Rough estimate: buy on cheaper, sell on expensive
			buy, sell := p1, p2
			if price2 < price1 {
				buy, sell = p2, p1
			}

			inputAmount := 0.1 // try 0.1 WETH
			buyPrice := buy.Price0In1()
			sellPrice := sell.Price0In1()
			grossProfit := inputAmount * (sellPrice - buyPrice)
			gasUSD := 0.015 // rough $15 gas cost
			netProfit := grossProfit - gasUSD

			if netProfit > cfg.MinProfitUSD {
				opps = append(opps, types.ArbitrageOpportunity{
					TokenIn:     p1.Token0,
					TokenOut:    p1.Token1,
					InputAmount: inputAmount,
					GrossProfit: grossProfit,
					NetProfitUSD: netProfit,
					Route: []types.RouteHop{
						{Pool: buy, TokenIn: p1.Token0, TokenOut: p1.Token1, AmountIn: inputAmount},
						{Pool: sell, TokenIn: p1.Token1, TokenOut: p1.Token0, AmountIn: inputAmount * buyPrice},
					},
					DetectedAt: time.Now(),
				})
			}
		}
	}

	return opps
}

func formatOpportunity(o types.ArbitrageOpportunity) string {
	if len(o.Route) < 1 {
		return fmt.Sprintf("%s→%s profit=$%.4f", o.TokenIn.Symbol, o.TokenOut.Symbol, o.NetProfitUSD)
	}
	return fmt.Sprintf(
		"Buy %s on %s @ %.2f | Sell on %s @ %.2f | input=%.4f %s | gross=$%.4f | gas=$%.4f | net=$%.4f",
		o.TokenOut.Symbol,
		o.Route[0].Pool.DEX,
		o.Route[0].Pool.Price0In1(),
		o.Route[len(o.Route)-1].Pool.DEX,
		o.Route[len(o.Route)-1].Pool.Price0In1(),
		o.InputAmount,
		o.TokenIn.Symbol,
		o.GrossProfit,
		o.GrossProfit-o.NetProfitUSD,
		o.NetProfitUSD,
	)
}

func printUsage() {
	fmt.Print(`go-dex-arbitrage — educational DEX arbitrage bot

USAGE:
  go run ./cmd/arbitrage <command> [flags]

COMMANDS:
  detect-arbitrage     Fetch current DEX pools and display opportunities
  backtest-strategy    Replay historical prices and compute strategy stats
  monitor-live         Continuous polling loop (Ctrl+C to stop)

FLAGS (detect-arbitrage):
  -min-profit  float   Minimum net profit in USD (default 0.01)
  -max-slippage float  Maximum slippage fraction (default 0.005)
  -max-hops    int     Maximum route hops (default 3)

FLAGS (backtest-strategy):
  -data    string   Path to historical CSV (default testdata/prices.csv)
  -report  string   Output HTML report path (default backtest_report.html)

FLAGS (monitor-live):
  -interval  duration   Polling interval (default 10s)
  -min-profit float     Minimum net profit in USD (default 0.01)

EXAMPLES:
  go run ./cmd/arbitrage detect-arbitrage
  go run ./cmd/arbitrage detect-arbitrage -min-profit 5.0
  go run ./cmd/arbitrage backtest-strategy -data testdata/prices.csv
  go run ./cmd/arbitrage monitor-live -interval 5s
`)
}
