// Package monitor provides live pool monitoring with periodic price refresh.
//
// # Architecture
//
// The monitor uses a simple poll-based architecture:
//
//	Ticker (every N seconds)
//	    │
//	    ▼
//	Fetch pool state from DEX APIs (HTTP / GraphQL)
//	    │
//	    ▼
//	Feed pools into detector.Detect()
//	    │
//	    ▼
//	Publish ArbitrageOpportunity events on output channel
//	    │
//	    ▼
//	cmd/main.go displays or acts on events
//
// # Why polling instead of WebSockets?
//
// Real-time feeds (WebSockets) are ideal for lowest latency but require a
// persistent connection and more complex error handling.  Polling is simpler:
// easier to test, easier to rate-limit, and fine for educational exploration.
// Uniswap's subgraph (The Graph) and dYdX v3 REST both support polling easily.
//
// # Running the monitor
//
//	mon := monitor.New(cfg, clients...)
//	ch  := mon.Start(ctx)
//	for opp := range ch {
//	    fmt.Println(opp)
//	}
package monitor

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/user/go-dex-arbitrage/internal/detector"
	"github.com/user/go-dex-arbitrage/internal/dex"
	"github.com/user/go-dex-arbitrage/pkg/types"
)

// Ensure context is used (monitor loop passes ctx to Start but fetchPools is sync)
var _ = context.Background

// Event is emitted on the monitor's output channel each polling cycle.
type Event struct {
	Time         time.Time
	Opportunities []types.ArbitrageOpportunity
	PoolsChecked  int
	Err          error
}

// Monitor polls DEX clients, detects arbitrage, and emits Events.
type Monitor struct {
	cfg      types.Config
	clients  []dex.DEXClient
	detector *detector.Detector
	interval time.Duration
}

// New creates a Monitor with the given configuration and DEX clients.
// interval controls how often the DEXs are queried.
func New(cfg types.Config, interval time.Duration, clients ...dex.DEXClient) *Monitor {
	return &Monitor{
		cfg:      cfg,
		clients:  clients,
		detector: detector.NewDetector(cfg),
		interval: interval,
	}
}

// Start begins the monitoring loop and returns a channel of Events.
// The loop runs until ctx is cancelled.
// The channel is closed when monitoring stops.
//
// Usage:
//
//	ctx, cancel := context.WithCancel(context.Background())
//	defer cancel()
//	for event := range mon.Start(ctx) {
//	    handleEvent(event)
//	}
func (m *Monitor) Start(ctx context.Context) <-chan Event {
	out := make(chan Event, 16) // buffered to prevent blocking on slow consumers

	go func() {
		defer close(out)

		ticker := time.NewTicker(m.interval)
		defer ticker.Stop()

		log.Printf("monitor: started, polling every %s", m.interval)

		// Run once immediately before the first tick
		m.poll(ctx, out)

		for {
			select {
			case <-ctx.Done():
				log.Printf("monitor: context cancelled, stopping")
				return
			case <-ticker.C:
				m.poll(ctx, out)
			}
		}
	}()

	return out
}

// poll fetches the current pool state from all clients and detects opportunities.
func (m *Monitor) poll(ctx context.Context, out chan<- Event) {
	event := Event{Time: time.Now()}

	pools, err := m.fetchPools()
	if err != nil {
		event.Err = err
		select {
		case out <- event:
		default:
			// Channel full: skip this event rather than blocking
		}
		return
	}

	event.PoolsChecked = len(pools)

	if len(pools) > 0 {
		event.Opportunities = m.detector.Detect(pools)
	}

	select {
	case out <- event:
	default:
		// Slow consumer: drop rather than block the monitor goroutine
		log.Printf("monitor: event channel full, dropping event at %s", event.Time)
	}
}

// fetchPools queries all configured DEX clients for the monitored token pairs.
// DEXClient.FetchPool fetches a single pair; we iterate over all token combinations.
// In production you'd parallelise this with goroutines.
func (m *Monitor) fetchPools() ([]types.Pool, error) {
	// Default token pairs to monitor
	pairs := [][2]types.Token{
		{types.WETH, types.USDC},
		{types.WETH, types.USDT},
		{types.WETH, types.DAI},
		{types.WBTC, types.USDC},
	}

	var all []types.Pool
	for _, client := range m.clients {
		for _, pair := range pairs {
			pool, err := client.FetchPool(pair[0], pair[1])
			if err != nil {
				// Not every DEX supports every pair — skip silently
				continue
			}
			all = append(all, pool)
		}
	}

	if len(all) == 0 {
		return nil, fmt.Errorf("monitor: all DEX clients returned no pools")
	}
	return all, nil
}

// ---- Console display --------------------------------------------------------

// PrintEvent formats an Event for terminal display.
func PrintEvent(e Event) string {
	if e.Err != nil {
		return fmt.Sprintf("[%s] ERROR: %v", e.Time.Format("15:04:05"), e.Err)
	}
	if len(e.Opportunities) == 0 {
		return fmt.Sprintf("[%s] checked %d pools — no opportunities", e.Time.Format("15:04:05"), e.PoolsChecked)
	}

	best := e.Opportunities[0]
	return fmt.Sprintf(
		"[%s] %d pools | %d opps | best: %s %.4f → %s profit $%.4f net $%.4f",
		e.Time.Format("15:04:05"),
		e.PoolsChecked,
		len(e.Opportunities),
		best.TokenIn.Symbol,
		best.InputAmount,
		best.TokenOut.Symbol,
		best.GrossProfit,
		best.NetProfitUSD,
	)
}

// ---- Stats accumulator ------------------------------------------------------

// Stats tracks running statistics across monitor events.
type Stats struct {
	Cycles         int
	TotalOpps      int
	ProfitableOpps int
	TotalNetProfit float64
	PeakProfit     float64 // highest single-trade net profit seen
}

// Update incorporates a new Event into the running statistics.
func (s *Stats) Update(e Event) {
	if e.Err != nil {
		return
	}
	s.Cycles++
	for _, o := range e.Opportunities {
		s.TotalOpps++
		if o.NetProfitUSD > 0 {
			s.ProfitableOpps++
			s.TotalNetProfit += o.NetProfitUSD
			if o.NetProfitUSD > s.PeakProfit {
				s.PeakProfit = o.NetProfitUSD
			}
		}
	}
}

// Summary returns a formatted stats line.
func (s *Stats) Summary() string {
	return fmt.Sprintf(
		"cycles=%d opps=%d profitable=%d totalPnL=$%.4f peak=$%.4f",
		s.Cycles, s.TotalOpps, s.ProfitableOpps, s.TotalNetProfit, s.PeakProfit,
	)
}
