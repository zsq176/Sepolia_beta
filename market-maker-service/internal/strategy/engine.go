// Package strategy turns market snapshots into trading decisions.
//
// The engine is intentionally stateless beyond the per-instrument cooldown
// timer: every Decide() call examines the latest snapshot and returns a fully
// specified Decision (or ActionHold). All heavy machinery (slippage, gas
// budget, circuit breakers) is delegated to the risk package and the
// execution layer.
package strategy

import (
	"fmt"
	"log"
	"math"
	"math/big"
	"market-maker-service/internal/config"
	"sync"
	"time"
	"market-maker-service/internal/domain"
)

// Engine produces Decisions from MarketSnapshots.
type Engine struct {
	insts map[string]*domain.Instrument
	cfg   *config.Config

	mu       sync.Mutex
	lastTrade map[string]time.Time
}

// NewEngine prepares a strategy engine for the given instruments.
func NewEngine(insts []*domain.Instrument, cfg *config.Config) *Engine {
	m := make(map[string]*domain.Instrument, len(insts))
	for _, i := range insts {
		m[i.ID] = i
	}
	return &Engine{
		insts:     m,
		cfg:       cfg,
		lastTrade: make(map[string]time.Time),
	}
}

// MarkExecuted should be called by the execution layer whenever a swap was
// actually submitted on-chain, so cooldowns are enforced.
func (e *Engine) MarkExecuted(instrumentID string, when time.Time) {
	e.mu.Lock()
	e.lastTrade[instrumentID] = when
	e.mu.Unlock()
}

// Decide examines a snapshot and returns one Decision per instrument that
// passes the deadband / cooldown / freshness checks. Holds are not returned.
func (e *Engine) Decide(snap *domain.MarketSnapshot) []domain.Decision {
	if snap == nil {
		return nil
	}
	out := make([]domain.Decision, 0, len(snap.Instruments))
	now := snap.GeneratedAt
	for id, s := range snap.Instruments {
		d, ok := e.evaluate(id, s, now)
		if ok {
			out = append(out, d)
		}
	}
	return out
}

// evaluate runs the strategy for a single instrument snapshot. It returns
// (Decision, true) only when the bot should send an order; otherwise the
// reason is logged and (zero, false) is returned.
func (e *Engine) evaluate(id string, s *domain.InstrumentSnapshot, now time.Time) (domain.Decision, bool) {
	inst := s.Instrument
	if inst == nil {
		return domain.Decision{}, false
	}
	p := inst.Strategy

	// --- 1. Data quality gates ---
	if !s.PoolFresh {
		return domain.Decision{}, false
	}
	if !s.TargetFresh {
		return domain.Decision{}, false
	}
	if s.Pool.Mid <= 0 || s.Target.Mid <= 0 {
		return domain.Decision{}, false
	}

	// --- 2. Circuit breaker: an extreme deviation likely signals manipulation. ---
	devAbs := math.Abs(s.Deviation)
	if p.CircuitBreaker > 0 && devAbs >= p.CircuitBreaker {
		log.Printf("[strategy] %s: circuit (dev=%.4f%% >= %.4f%%) skipping",
			id, devAbs*100, p.CircuitBreaker*100)
		return domain.Decision{}, false
	}

	// --- 3. Deadband: do not trade tiny noise. ---
	triggerBps := 0.0
	if e.cfg != nil {
		triggerBps = e.cfg.GetRuntimeFloat("trigger_bps", e.cfg.TriggerBps)
	}
	effectiveDeadband := p.Deadband
	if triggerBps > 0 {
		effectiveDeadband = math.Max(effectiveDeadband, triggerBps/10000.0)
	}
	if devAbs < effectiveDeadband {
		return domain.Decision{}, false
	}

	// --- 4. Cooldown ---
	e.mu.Lock()
	last := e.lastTrade[id]
	e.mu.Unlock()
	cooldownSec := p.CooldownSec
	if e.cfg != nil {
		rc := int64(e.cfg.GetRuntimeFloat("cooldown_sec", float64(cooldownSec)))
		if rc > 0 {
			cooldownSec = rc
		}
	}
	if cooldownSec > 0 && !last.IsZero() && now.Sub(last) < time.Duration(cooldownSec)*time.Second {
		return domain.Decision{}, false
	}

	// --- 5. Sizing: proportional to deviation, then clipped. ---
	notional := math.Abs(p.Gain * s.Deviation * p.MaxNotionalUSD)
	if notional < p.MinNotionalUSD {
		notional = p.MinNotionalUSD
	}
	if notional > p.MaxNotionalUSD {
		notional = p.MaxNotionalUSD
	}

	// --- 6. Direction & amount conversion. ---
	// If pool > target -> sell base for quote (push price down).
	// If pool < target -> buy base with quote (push price up).
	action := domain.ActionSell
	if s.Pool.Mid < s.Target.Mid {
		action = domain.ActionBuy
	}

	amountIn, amountInHuman, err := computeAmountIn(action, inst, s.Pool.Mid, notional)
	if err != nil {
		log.Printf("[strategy] %s: sizing error: %v", id, err)
		return domain.Decision{}, false
	}
	if amountIn.Sign() <= 0 {
		return domain.Decision{}, false
	}

	minOut := computeMinOut(action, inst, s.Pool.Mid, amountIn, p.MaxSlippage)

	dec := domain.Decision{
		InstrumentID:  id,
		Action:        action,
		AmountIn:      amountIn.String(),
		AmountInHuman: amountInHuman,
		MinAmountOut:  minOut.String(),
		TargetPrice:   s.Target.Mid,
		PoolPrice:     s.Pool.Mid,
		Deviation:     s.Deviation,
		NotionalUSD:   notional,
		SlippageBps:   int(p.MaxSlippage * 10000),
		Reason:        fmt.Sprintf("dev=%.4f%% notional=$%.0f", s.Deviation*100, notional),
		GeneratedAt:   now,
	}
	return dec, true
}

// computeAmountIn turns a USD notional into an integer-decimal token amount.
//
// For ActionSell we sell `base`, so amountIn is denominated in base units
// (notionalUSD / poolPrice). For ActionBuy we sell `quote`, so amountIn is
// notionalUSD * 1.0 in quote units (assuming quote is USD-pegged) — for
// non-USD quotes we fall back to converting via poolPrice (good enough for the
// tiny test pairs).
func computeAmountIn(action domain.ActionKind, inst *domain.Instrument, poolPrice, notionalUSD float64) (*big.Int, float64, error) {
	if poolPrice <= 0 {
		return nil, 0, fmt.Errorf("pool price <= 0")
	}
	switch action {
	case domain.ActionSell:
		amount := notionalUSD / poolPrice
		return floatToWei(amount, inst.Base.Decimals), amount, nil
	case domain.ActionBuy:
		amount := notionalUSD
		// non-USD quote: convert through poolPrice (e.g. ETH/BTC: notional in
		// USD ≈ poolPrice_in_BTC * btc_price; we conservatively use 1).
		if inst.Quote.Symbol != "USDT-Beta" {
			amount = notionalUSD / poolPrice
		}
		return floatToWei(amount, inst.Quote.Decimals), amount, nil
	}
	return nil, 0, fmt.Errorf("unknown action %q", action)
}

// computeMinOut applies slippage tolerance to the expected output of an
// exact-input swap. We use poolPrice as the "fair" price; the executor will
// further quote against the pool itself when a quoter is wired up.
func computeMinOut(action domain.ActionKind, inst *domain.Instrument, poolPrice float64, amountIn *big.Int, slippage float64) *big.Int {
	if slippage < 0 {
		slippage = 0
	}
	if slippage > 0.5 {
		slippage = 0.5
	}
	amtF := new(big.Float).SetInt(amountIn)

	var expected *big.Float
	switch action {
	case domain.ActionSell:
		// in base, out quote
		f := new(big.Float).Quo(amtF, new(big.Float).SetFloat64(pow10(inst.Base.Decimals)))
		f = new(big.Float).Mul(f, new(big.Float).SetFloat64(poolPrice))
		expected = new(big.Float).Mul(f, new(big.Float).SetFloat64(pow10(inst.Quote.Decimals)))
	case domain.ActionBuy:
		// in quote, out base
		f := new(big.Float).Quo(amtF, new(big.Float).SetFloat64(pow10(inst.Quote.Decimals)))
		f = new(big.Float).Quo(f, new(big.Float).SetFloat64(poolPrice))
		expected = new(big.Float).Mul(f, new(big.Float).SetFloat64(pow10(inst.Base.Decimals)))
	default:
		return big.NewInt(0)
	}

	expected = new(big.Float).Mul(expected, new(big.Float).SetFloat64(1-slippage))
	out, _ := expected.Int(nil)
	if out == nil {
		return big.NewInt(0)
	}
	return out
}

func floatToWei(amount float64, decimals int) *big.Int {
	if amount <= 0 || math.IsNaN(amount) || math.IsInf(amount, 0) {
		return big.NewInt(0)
	}
	f := new(big.Float).Mul(new(big.Float).SetFloat64(amount), new(big.Float).SetFloat64(pow10(decimals)))
	out, _ := f.Int(nil)
	if out == nil {
		return big.NewInt(0)
	}
	return out
}

func pow10(n int) float64 {
	r := 1.0
	if n > 0 {
		for i := 0; i < n; i++ {
			r *= 10
		}
	} else {
		for i := 0; i < -n; i++ {
			r /= 10
		}
	}
	return r
}
