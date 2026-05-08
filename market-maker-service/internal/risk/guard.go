// Package risk implements the second-stage gate between Strategy and Execution.
//
// While the strategy engine handles per-instrument deadband / sizing, the risk
// guard is responsible for system-wide invariants: gas price ceilings, USD
// gas budgets, minimum net edge after costs, manipulation-driven circuit
// breakers, and exponential back-off on consecutive failures.
package risk

import (
	"context"
	"fmt"
	"log"
	"math/big"
	"sync"
	"time"
	"market-maker-service/internal/config"
	"market-maker-service/internal/domain"

)

// EthPriceProvider returns the current ETH/USD price. Used to express gas
// budgets in USD instead of wei.
type EthPriceProvider func() (float64, bool)

// State tracks per-instrument health (circuit breakers, fail counts).
type instrumentState struct {
	circuitOpen      bool
	circuitOpenedAt  time.Time
	consecutiveFails int
	backoffUntil     time.Time
	lastReason       string
}

// Guard mediates the fail-safes for the trading bot.
type Guard struct {
	cfg     *config.Config
	client  interface {
		SuggestGasPrice(ctx context.Context) (*big.Int, error)
	}
	ethPx   EthPriceProvider

	mu     sync.Mutex
	states map[string]*instrumentState
	dayKey string
	dayGas float64
}

// NewGuard constructs a Guard. ethPx is allowed to be nil; in that case the
// USD gas check falls back to the GasCapGwei limit only.
func NewGuard(cfg *config.Config, client interface {
	SuggestGasPrice(ctx context.Context) (*big.Int, error)
}, ethPx EthPriceProvider) *Guard {
	return &Guard{
		cfg:    cfg,
		client: client,
		ethPx:  ethPx,
		states: make(map[string]*instrumentState),
	}
}

// Verdict is the result of a risk check.
type Verdict struct {
	Allow  bool
	Reason string
}

// Allow runs all risk checks for a candidate Decision and returns a Verdict.
// The caller must respect Allow == false (skip / requeue based on Reason).
func (g *Guard) Allow(ctx context.Context, d *domain.Decision) Verdict {
	st := g.stateOf(d.InstrumentID)
	if d.PoolPrice <= 0 || d.TargetPrice <= 0 {
		return Verdict{false, "quality invalid"}
	}

	// 1. Per-instrument back-off after recent failures.
	if !st.backoffUntil.IsZero() && time.Now().Before(st.backoffUntil) {
		return Verdict{false, fmt.Sprintf("back-off until %s (%s)", st.backoffUntil.Format(time.RFC3339), st.lastReason)}
	}

	// 2. Per-instrument circuit breaker (opened previously).
	if st.circuitOpen {
		return Verdict{false, "circuit breaker open"}
	}

	// 3. Gas price ceiling (network gwei).
	if g.client != nil {
		gp, err := g.client.SuggestGasPrice(ctx)
		if err == nil && gp != nil {
			gasCapGwei := int64(g.cfg.GetRuntimeFloat("max_gas_gwei", float64(g.cfg.GasCapGwei)))
			cap := new(big.Int).Mul(big.NewInt(gasCapGwei), big.NewInt(1e9))
			if gp.Cmp(cap) > 0 {
				gpGwei := new(big.Float).Quo(new(big.Float).SetInt(gp), new(big.Float).SetFloat64(1e9))
				return Verdict{false, fmt.Sprintf("gas %s gwei > cap %d gwei", gpGwei.Text('f', 1), gasCapGwei)}
			}

			// 4. USD gas budget gate (uses an estimated swap gas cost of 250k).
			maxGasUSD := g.cfg.GetRuntimeFloat("max_gas_usd_per_trade", g.cfg.MaxGasUSD)
			if maxGasUSD > 0 && g.ethPx != nil {
				if eth, ok := g.ethPx(); ok && eth > 0 {
					gasUSD := estimatedGasUSD(gp, 250_000, eth)
					if gasUSD > maxGasUSD {
						return Verdict{false, fmt.Sprintf("gas $%.2f > cap $%.2f", gasUSD, maxGasUSD)}
					}
					dailyBudget := g.cfg.GetRuntimeFloat("daily_budget_usd", g.cfg.DailyBudgetUSD)
					if dailyBudget > 0 && !g.consumeDailyBudget(gasUSD, dailyBudget) {
						return Verdict{false, fmt.Sprintf("daily budget exceeded: +$%.2f > $%.2f", gasUSD, dailyBudget)}
					}
					// 5. Net edge gate: the deviation in USD must beat gas + slippage.
					expectedEdge := d.NotionalUSD * absFloat(d.Deviation)
					netEdge := expectedEdge - gasUSD
					minNet := g.cfg.GetRuntimeFloat("min_net_edge_usd", g.cfg.MinNetEdgeUSD)
					if netEdge < minNet {
						return Verdict{false, fmt.Sprintf("net edge $%.2f < $%.2f (edge=$%.2f gas=$%.2f)",
							netEdge, minNet, expectedEdge, gasUSD)}
					}
				}
			}
		}
	}

	return Verdict{Allow: true}
}

// RecordSuccess clears the failure counter and any back-off for the instrument.
func (g *Guard) RecordSuccess(instrumentID string) {
	st := g.stateOf(instrumentID)
	g.mu.Lock()
	defer g.mu.Unlock()
	st.consecutiveFails = 0
	st.backoffUntil = time.Time{}
	st.lastReason = ""
}

// RecordFailure increments the failure counter and triggers exponential
// back-off (5s, 15s, 45s, 2m15s, …) up to a cap.
func (g *Guard) RecordFailure(instrumentID string, reason string) {
	st := g.stateOf(instrumentID)
	g.mu.Lock()
	defer g.mu.Unlock()
	st.consecutiveFails++
	backoff := time.Duration(5) * time.Second
	for i := 1; i < st.consecutiveFails && backoff < 5*time.Minute; i++ {
		backoff *= 3
	}
	if backoff > 5*time.Minute {
		backoff = 5 * time.Minute
	}
	st.backoffUntil = time.Now().Add(backoff)
	st.lastReason = reason
	log.Printf("[risk] %s: failure %d, back-off %s (%s)",
		instrumentID, st.consecutiveFails, backoff, reason)
}

// TripCircuit marks an instrument's circuit breaker as open. While open the
// guard rejects all decisions for that instrument.
func (g *Guard) TripCircuit(instrumentID, reason string, openFor time.Duration) {
	st := g.stateOf(instrumentID)
	g.mu.Lock()
	defer g.mu.Unlock()
	st.circuitOpen = true
	st.circuitOpenedAt = time.Now()
	st.lastReason = reason
	log.Printf("[risk] %s: circuit OPEN (%s) for %s", instrumentID, reason, openFor)
	go func() {
		time.Sleep(openFor)
		g.mu.Lock()
		st.circuitOpen = false
		g.mu.Unlock()
		log.Printf("[risk] %s: circuit CLEARED", instrumentID)
	}()
}

// CircuitOpen reports whether a given instrument is currently halted.
func (g *Guard) CircuitOpen(instrumentID string) bool {
	st := g.stateOf(instrumentID)
	g.mu.Lock()
	defer g.mu.Unlock()
	return st.circuitOpen
}

// Snapshot returns a read-only summary of the per-instrument risk state. Used
// by the API server for the /api/health and /api/risk endpoints.
func (g *Guard) Snapshot() map[string]map[string]interface{} {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make(map[string]map[string]interface{}, len(g.states))
	for id, st := range g.states {
		out[id] = map[string]interface{}{
			"circuit_open":      st.circuitOpen,
			"consecutive_fails": st.consecutiveFails,
			"backoff_until":     st.backoffUntil.Unix(),
			"last_reason":       st.lastReason,
		}
	}
	out["_global"] = map[string]interface{}{
		"day_key":          g.dayKey,
		"daily_gas_spent":  g.dayGas,
		"daily_budget_usd": g.cfg.GetRuntimeFloat("daily_budget_usd", g.cfg.DailyBudgetUSD),
	}
	return out
}

func (g *Guard) stateOf(id string) *instrumentState {
	g.mu.Lock()
	defer g.mu.Unlock()
	st, ok := g.states[id]
	if !ok {
		st = &instrumentState{}
		g.states[id] = st
	}
	return st
}

// estimatedGasUSD approximates: gasPrice (wei) * gasUnits / 1e18 * ethPriceUSD.
func estimatedGasUSD(gasPriceWei *big.Int, gasUnits uint64, ethPriceUSD float64) float64 {
	wei := new(big.Float).SetInt(gasPriceWei)
	wei = new(big.Float).Mul(wei, new(big.Float).SetUint64(gasUnits))
	eth := new(big.Float).Quo(wei, new(big.Float).SetFloat64(1e18))
	usd := new(big.Float).Mul(eth, new(big.Float).SetFloat64(ethPriceUSD))
	f, _ := usd.Float64()
	return f
}

func absFloat(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}

func (g *Guard) consumeDailyBudget(amount float64, budget float64) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	day := time.Now().UTC().Format("2006-01-02")
	if g.dayKey != day {
		g.dayKey = day
		g.dayGas = 0
	}
	if g.dayGas+amount > budget {
		return false
	}
	g.dayGas += amount
	return true
}
