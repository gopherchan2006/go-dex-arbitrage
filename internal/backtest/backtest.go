// Package backtest simulates arbitrage strategies on historical price data.
//
// # Why backtest?
//
// Before trading with real money, you want evidence that your strategy works.
// A backtest replays historical data as if your bot had been running then,
// recording what trades it would have made and calculating P&L.
//
// # Limitations of backtesting (know these before trusting results)
//
//  1. Look-ahead bias: using future data to make past decisions (easy to accidentally add)
//  2. Slippage underestimation: historical data shows mid-prices; real execution costs more
//  3. No competition: your backtest ignores other bots also trying the same trade
//  4. Gas cost changes: Ethereum gas prices are volatile; fixed gas in backtest ≠ reality
//  5. Liquidity illusion: historical reserves may not reflect real available liquidity
//
// Despite these caveats, backtesting is invaluable for sanity-checking logic.
package backtest

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"os"
	"sort"
	"strconv"
	"time"

	"github.com/user/go-dex-arbitrage/internal/amm"
	"github.com/user/go-dex-arbitrage/internal/detector"
	"github.com/user/go-dex-arbitrage/internal/route"
	"github.com/user/go-dex-arbitrage/pkg/types"
)

// ---- Historical data types --------------------------------------------------

// PricePoint represents a single price observation for one token pair on one DEX.
// In practice you'd load this from a database of on-chain events.
// Here we use CSV files in /testdata/.
type PricePoint struct {
	Timestamp time.Time
	DEX       string
	Token0    string
	Token1    string
	Price     float64 // token1 per token0 (human-readable)
	Reserve0  float64 // token0 liquidity (human units)
	Reserve1  float64 // token1 liquidity (human units)
}

// ---- Loader -----------------------------------------------------------------

// LoadCSV loads price history from a CSV file with columns:
// timestamp, dex, token0, token1, price, reserve0, reserve1
func LoadCSV(path string) ([]PricePoint, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("backtest: open %s: %w", path, err)
	}
	defer f.Close()

	r := csv.NewReader(f)
	rows, err := r.ReadAll()
	if err != nil {
		return nil, fmt.Errorf("backtest: read csv: %w", err)
	}

	var points []PricePoint
	for i, row := range rows {
		if i == 0 {
			continue // skip header
		}
		if len(row) < 7 {
			continue
		}

		ts, err := time.Parse(time.RFC3339, row[0])
		if err != nil {
			log.Printf("backtest: skip row %d (bad timestamp): %v", i, err)
			continue
		}

		price, _ := strconv.ParseFloat(row[4], 64)
		r0, _    := strconv.ParseFloat(row[5], 64)
		r1, _    := strconv.ParseFloat(row[6], 64)

		points = append(points, PricePoint{
			Timestamp: ts,
			DEX:       row[1],
			Token0:    row[2],
			Token1:    row[3],
			Price:     price,
			Reserve0:  r0,
			Reserve1:  r1,
		})
	}

	// Sort by timestamp ascending
	sort.Slice(points, func(i, j int) bool {
		return points[i].Timestamp.Before(points[j].Timestamp)
	})
	return points, nil
}

// ---- Engine -----------------------------------------------------------------

// Engine runs the backtesting simulation.
type Engine struct {
	Config   types.Config
	ETHPrice float64 // assumed constant for simplicity
}

// NewEngine creates a backtest engine.
func NewEngine(cfg types.Config) *Engine {
	return &Engine{Config: cfg, ETHPrice: 2500}
}

// Run simulates the arbitrage strategy over a series of price snapshots.
// snapshots maps time → list of pools at that time.
func (e *Engine) Run(snapshots []Snapshot) types.BacktestResult {
	det := detector.NewDetector(e.Config)

	var (
		allOpps   []types.ArbitrageOpportunity
		equity    float64 // cumulative net P&L in USD
		peakEquity float64
		maxDD     float64
		wins, losses int
		totalWinPnL, totalLossPnL float64
	)

	for _, snap := range snapshots {
		opps := det.Detect(snap.Pools)
		if len(opps) == 0 {
			continue
		}

		// Take the best opportunity each block (simplification)
		best := opps[0]
		allOpps = append(allOpps, best)

		equity += best.NetProfitUSD
		if equity > peakEquity {
			peakEquity = equity
		}
		dd := peakEquity - equity
		if dd > maxDD {
			maxDD = dd
		}

		if best.NetProfitUSD > 0 {
			wins++
			totalWinPnL += best.NetProfitUSD
		} else {
			losses++
			totalLossPnL += best.NetProfitUSD
		}
	}

	total := wins + losses
	winRate := 0.0
	if total > 0 {
		winRate = float64(wins) / float64(total)
	}
	avgProfit := 0.0
	if wins > 0 {
		avgProfit = totalWinPnL / float64(wins)
	}
	avgLoss := 0.0
	if losses > 0 {
		avgLoss = totalLossPnL / float64(losses)
	}

	sharpe := calcSharpe(allOpps)

	var start, end time.Time
	if len(snapshots) > 0 {
		start = snapshots[0].Time
		end   = snapshots[len(snapshots)-1].Time
	}

	return types.BacktestResult{
		StartTime:     start,
		EndTime:       end,
		TotalTrades:   total,
		WinningTrades: wins,
		LosingTrades:  losses,
		WinRate:       winRate,
		TotalProfit:   equity,
		MaxDrawdown:   maxDD,
		SharpeRatio:   sharpe,
		AverageProfit: avgProfit,
		AverageLoss:   avgLoss,
		Opportunities: allOpps,
	}
}

// Snapshot is a slice of pool states at a single point in time.
type Snapshot struct {
	Time  time.Time
	Pools []types.Pool
}

// BuildSnapshots converts raw price history into time-indexed pool snapshots.
// Groups price points by timestamp and constructs Pool objects from each.
func BuildSnapshots(history []PricePoint, token0, token1 types.Token) []Snapshot {
	// Group by timestamp
	byTime := make(map[time.Time][]PricePoint)
	for _, p := range history {
		byTime[p.Timestamp] = append(byTime[p.Timestamp], p)
	}

	var times []time.Time
	for t := range byTime {
		times = append(times, t)
	}
	sort.Slice(times, func(i, j int) bool { return times[i].Before(times[j]) })

	var snapshots []Snapshot
	for _, t := range times {
		var pools []types.Pool
		for _, pt := range byTime[t] {
			pools = append(pools, types.Pool{
				Address:  fmt.Sprintf("%s_%s_%s", pt.DEX, pt.Token0, pt.Token1),
				Token0:   token0,
				Token1:   token1,
				Reserve0: token0.ToRaw(pt.Reserve0),
				Reserve1: token1.ToRaw(pt.Reserve1),
				Fee:      types.FeeTier3000,
				DEX:      pt.DEX,
			})
		}
		if len(pools) >= 2 {
			snapshots = append(snapshots, Snapshot{Time: t, Pools: pools})
		}
	}
	return snapshots
}

// ---- Statistics -------------------------------------------------------------

// calcSharpe computes the annualised Sharpe ratio of the opportunity P&L series.
// Sharpe = (mean return - risk-free rate) / std dev of returns
// We use 0 as risk-free rate for simplicity.
func calcSharpe(opps []types.ArbitrageOpportunity) float64 {
	if len(opps) < 2 {
		return 0
	}
	returns := make([]float64, len(opps))
	for i, o := range opps {
		returns[i] = o.NetProfitUSD
	}

	mean := 0.0
	for _, r := range returns {
		mean += r
	}
	mean /= float64(len(returns))

	variance := 0.0
	for _, r := range returns {
		d := r - mean
		variance += d * d
	}
	variance /= float64(len(returns) - 1)
	stddev := math.Sqrt(variance)

	if stddev == 0 {
		return 0
	}
	// Annualise: assuming each block is ~12s → ~2.6M blocks/year
	// If we're sampling at 1-minute intervals: 525600 intervals/year
	annualisationFactor := math.Sqrt(525600)
	return mean / stddev * annualisationFactor
}

// ---- Report -----------------------------------------------------------------

// PrintReport formats a BacktestResult as a human-readable report string.
func PrintReport(r types.BacktestResult) string {
	duration := r.EndTime.Sub(r.StartTime)
	profitabilityPct := 0.0
	if r.TotalTrades > 0 {
		profitabilityPct = float64(r.WinningTrades) / float64(r.TotalTrades) * 100
	}
	return fmt.Sprintf(`
╔═══════════════════════════════════════════════════════╗
║              BACKTEST RESULTS                         ║
╠═══════════════════════════════════════════════════════╣
║ Period:        %-37s ║
║ Duration:      %-37s ║
╠═══════════════════════════════════════════════════════╣
║ Total trades:  %-37d ║
║ Winning:       %-10d  (%.1f%%)                  ║
║ Losing:        %-10d                              ║
╠═══════════════════════════════════════════════════════╣
║ Total P&L:     $%-36.2f ║
║ Avg win:       $%-36.2f ║
║ Avg loss:      $%-36.2f ║
║ Max drawdown:  $%-36.2f ║
║ Sharpe ratio:  %-37.3f ║
╚═══════════════════════════════════════════════════════╝
`,
		fmt.Sprintf("%s → %s", r.StartTime.Format("2006-01-02"), r.EndTime.Format("2006-01-02")),
		duration.String(),
		r.TotalTrades,
		r.WinningTrades, profitabilityPct,
		r.LosingTrades,
		r.TotalProfit,
		r.AverageProfit,
		r.AverageLoss,
		r.MaxDrawdown,
		r.SharpeRatio,
	)
}

// HTMLReport generates a simple HTML chart of cumulative P&L over time.
// Opens nicely in a browser – no external dependencies.
func HTMLReport(r types.BacktestResult) string {
	// Build data points for the cumulative P&L chart
	type point struct {
		Time   string  `json:"t"`
		Equity float64 `json:"y"`
	}

	var points []point
	var cumEquity float64
	for _, o := range r.Opportunities {
		cumEquity += o.NetProfitUSD
		points = append(points, point{
			Time:   o.DetectedAt.Format(time.RFC3339),
			Equity: cumEquity,
		})
	}

	dataJSON, _ := json.Marshal(points)

	return fmt.Sprintf(`<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="UTF-8">
  <title>Backtest Results</title>
  <style>
    body { font-family: monospace; background: #0d1117; color: #c9d1d9; padding: 24px; }
    h1   { color: #58a6ff; }
    .stat { display: inline-block; margin: 8px 16px; }
    .stat-val { font-size: 24px; font-weight: bold; color: #39d353; }
    canvas { background: #161b22; border-radius: 8px; max-width: 100%%; }
  </style>
</head>
<body>
  <h1>🔍 DEX Arbitrage Backtest Report</h1>
  <div>
    <div class="stat"><div class="stat-val">%d</div>Total Trades</div>
    <div class="stat"><div class="stat-val">%.1f%%</div>Win Rate</div>
    <div class="stat"><div class="stat-val">$%.2f</div>Total P&amp;L</div>
    <div class="stat"><div class="stat-val">$%.2f</div>Max Drawdown</div>
    <div class="stat"><div class="stat-val">%.2f</div>Sharpe Ratio</div>
  </div>
  <br>
  <canvas id="chart" width="900" height="400"></canvas>
  <script>
    const data = %s;
    const canvas = document.getElementById('chart');
    const ctx    = canvas.getContext('2d');
    const W = canvas.width, H = canvas.height;
    const pad = 60;

    if (data.length > 0) {
      const ys = data.map(d => d.y);
      const minY = Math.min(...ys), maxY = Math.max(...ys);
      const rangeY = maxY - minY || 1;

      ctx.strokeStyle = '#30363d';
      ctx.lineWidth = 1;
      // Grid lines
      for (let i = 0; i <= 5; i++) {
        const y = pad + (H - 2*pad) * (1 - i/5);
        ctx.beginPath(); ctx.moveTo(pad, y); ctx.lineTo(W-pad, y); ctx.stroke();
        ctx.fillStyle = '#8b949e'; ctx.font = '11px monospace';
        const label = (minY + rangeY * i/5).toFixed(0);
        ctx.fillText('$'+label, 4, y+4);
      }

      // P&L line
      ctx.strokeStyle = '#39d353'; ctx.lineWidth = 2; ctx.beginPath();
      data.forEach((d, i) => {
        const x = pad + (W - 2*pad) * i / (data.length-1);
        const y = pad + (H - 2*pad) * (1 - (d.y - minY)/rangeY);
        i === 0 ? ctx.moveTo(x,y) : ctx.lineTo(x,y);
      });
      ctx.stroke();

      // Zero line
      if (minY < 0 && maxY > 0) {
        const zy = pad + (H - 2*pad) * (1 - (0 - minY)/rangeY);
        ctx.strokeStyle = '#f85149'; ctx.lineWidth = 1; ctx.setLineDash([4,4]);
        ctx.beginPath(); ctx.moveTo(pad, zy); ctx.lineTo(W-pad, zy); ctx.stroke();
      }
    }
  </script>
</body>
</html>
`,
		r.TotalTrades,
		r.WinRate*100,
		r.TotalProfit,
		r.MaxDrawdown,
		r.SharpeRatio,
		string(dataJSON),
	)
}

// ---- Mock data generator ----------------------------------------------------

// GenerateMockHistory creates synthetic price history for two DEXs with
// occasional price divergences, useful for testing the backtest engine
// without external data.
func GenerateMockHistory(
	n int, // number of time steps
	basePrice float64, // starting price (e.g. 2500 USDC/ETH)
	divergeEvery int, // inject a price gap every N steps
) []PricePoint {
	now  := time.Now().UTC().Truncate(time.Minute)
	pts  := make([]PricePoint, 0, n*2)
	price := basePrice
	rng  := newSimpleLCG(42) // deterministic pseudo-random

	for i := 0; i < n; i++ {
		t := now.Add(time.Duration(i) * time.Minute)

		// Random walk
		price += (rng.float()*2 - 1) * 5 // ±$5 per step

		priceUniswap   := price
		priceSushiswap := price

		// Inject a divergence every N steps
		if divergeEvery > 0 && i%divergeEvery == 0 {
			priceSushiswap = price * 1.015 // 1.5% higher on Sushiswap → arb opportunity
		}

		reserve := 1_000.0 // 1000 ETH in pool

		pts = append(pts, PricePoint{
			Timestamp: t,
			DEX:      "uniswap",
			Token0:   "WETH",
			Token1:   "USDC",
			Price:    priceUniswap,
			Reserve0: reserve,
			Reserve1: reserve * priceUniswap,
		}, PricePoint{
			Timestamp: t,
			DEX:      "sushiswap",
			Token0:   "WETH",
			Token1:   "USDC",
			Price:    priceSushiswap,
			Reserve0: reserve,
			Reserve1: reserve * priceSushiswap,
		})
	}
	return pts
}

// simpleLCG is a minimal deterministic pseudo-random number generator.
// Not cryptographically secure – only for test data generation.
type simpleLCG struct{ state uint64 }

func newSimpleLCG(seed uint64) *simpleLCG { return &simpleLCG{seed} }
func (l *simpleLCG) next() uint64 {
	l.state = l.state*6364136223846793005 + 1442695040888963407
	return l.state
}
func (l *simpleLCG) float() float64 { return float64(l.next()>>11) / (1 << 53) }

// Ensure the amm package is used (its imports satisfy Go's "no unused imports" rule).
var _ = amm.CalcOutputV2
var _ = route.FindArbitrageRoutes
