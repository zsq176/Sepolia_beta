// Package sync glues the data layer (market.Aggregator) to the persistence
// layer (db.Store) and exposes a small surface for the API server.
//
// Historically this package owned price polling and arbitrage execution. After
// the production refactor, both responsibilities moved out: market.Aggregator
// owns the data plane, and execution.Service owns the order plane. What
// remains here is:
//
//   - bookkeeping of the latest prices (so /api/prices stays cheap)
//   - 1-minute kline aggregation written to SQLite
//   - per-instrument circuit breaker mirror used by the API for surface area
package sync

import (
	"log"
	"math"
	"sync"
	"time"
	"market-maker-service/internal/db"
	"market-maker-service/internal/domain"
	"market-maker-service/internal/risk"
)

// Engine is a read-only view over the latest market snapshot, plus a kline
// aggregator that batches 1-minute OHLC samples to the store.
type Engine struct {
	store *db.Store
	guard *risk.Guard

	mu      sync.RWMutex
	prices  map[string]float64               // pair -> mid (legacy keys for the API)
	devs    map[string]float64               // instrumentID -> deviation
	last    *domain.MarketSnapshot
	klineMu sync.Mutex
	klines  map[string]*klineBucket

	stopCh   chan struct{}
	stopOnce sync.Once
}

// NewEngine constructs the read-only engine.
func NewEngine(store *db.Store, guard *risk.Guard) *Engine {
	e := &Engine{
		store:  store,
		guard:  guard,
		prices: make(map[string]float64),
		devs:   make(map[string]float64),
		klines: make(map[string]*klineBucket),
		stopCh: make(chan struct{}),
	}
	go e.klineLoop()
	return e
}

// OnSnapshot is registered as a market.Subscriber by main.go.
func (e *Engine) OnSnapshot(snap *domain.MarketSnapshot) {
	if snap == nil {
		return
	}
	e.mu.Lock()
	e.last = snap
	for sym, q := range snap.Binance {
		e.prices["binance:"+formatBinanceSymbol(sym)] = q.Mid
	}
	for id, is := range snap.Instruments {
		e.prices[is.Instrument.Label] = is.Pool.Mid
		e.prices[id] = is.Pool.Mid
		e.devs[id] = is.Deviation
	}
	e.mu.Unlock()

	// Update kline buckets.
	now := time.Now().Unix()
	e.klineMu.Lock()
	for id, is := range snap.Instruments {
		if is.Pool.Mid <= 0 {
			continue
		}
		b := e.klines[id]
		if b == nil || now-b.startedAt >= 60 {
			if b != nil {
				e.flushKline(id, b)
			}
			e.klines[id] = &klineBucket{
				startedAt: now,
				open:      is.Pool.Mid, high: is.Pool.Mid, low: is.Pool.Mid, close: is.Pool.Mid,
			}
			continue
		}
		b.close = is.Pool.Mid
		if is.Pool.Mid > b.high {
			b.high = is.Pool.Mid
		}
		if is.Pool.Mid < b.low {
			b.low = is.Pool.Mid
		}
	}
	e.klineMu.Unlock()

	e.checkBreakers(snap)
}

// GetPrices returns a copy of the price map for the API.
func (e *Engine) GetPrices() map[string]float64 {
	e.mu.RLock()
	defer e.mu.RUnlock()
	out := make(map[string]float64, len(e.prices))
	for k, v := range e.prices {
		out[k] = v
	}
	return out
}

// LatestSnapshot returns the most recent snapshot or nil if none has been
// received yet.
func (e *Engine) LatestSnapshot() *domain.MarketSnapshot {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.last
}

// IsCircuitBroken reports the global flag (any instrument with circuit open).
func (e *Engine) IsCircuitBroken() bool {
	if e.guard == nil {
		return false
	}
	for _, st := range e.guard.Snapshot() {
		if open, ok := st["circuit_open"].(bool); ok && open {
			return true
		}
	}
	return false
}

// Stop cancels the kline loop.
func (e *Engine) Stop() {
	e.stopOnce.Do(func() { close(e.stopCh) })
}

// checkBreakers trips the per-instrument circuit when |dev| crosses the threshold.
func (e *Engine) checkBreakers(snap *domain.MarketSnapshot) {
	if e.guard == nil {
		return
	}
	for id, is := range snap.Instruments {
		if !is.PoolFresh || !is.TargetFresh {
			continue
		}
		p := is.Instrument.Strategy
		if p.CircuitBreaker > 0 && math.Abs(is.Deviation) >= p.CircuitBreaker {
			if !e.guard.CircuitOpen(id) {
				cd := time.Duration(p.CircuitCooldownSec) * time.Second
				if cd <= 0 {
					cd = 2 * time.Minute
				}
				e.guard.TripCircuit(id, "deviation > circuit threshold", cd)
			}
		}
	}
}

// klineLoop periodically flushes whatever is in the in-progress buckets to
// SQLite, even if a bucket has not closed yet — that way the chart never goes
// blank for more than 60 seconds.
func (e *Engine) klineLoop() {
	t := time.NewTicker(60 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-e.stopCh:
			return
		case <-t.C:
			e.klineMu.Lock()
			for id, b := range e.klines {
				e.flushKline(id, b)
			}
			e.klineMu.Unlock()
		}
	}
}

func (e *Engine) flushKline(id string, b *klineBucket) {
	if e.store == nil || b == nil {
		return
	}
	e.store.InsertPrice(&db.PricePoint{
		Time:  b.startedAt,
		Pair:  id,
		Open:  b.open,
		High:  b.high,
		Low:   b.low,
		Close: b.close,
	})
	e.store.InsertKline(&db.PricePoint{
		Time:     b.startedAt,
		Pair:     id,
		Interval: "1m",
		Open:     b.open,
		High:     b.high,
		Low:      b.low,
		Close:    b.close,
	})
}

type klineBucket struct {
	startedAt              int64
	open, high, low, close float64
}

// formatBinanceSymbol turns "BTCUSDT" into "BTC/USDT" for legacy API keys.
func formatBinanceSymbol(sym string) string {
	if len(sym) <= 4 {
		return sym
	}
	for _, q := range []string{"USDT", "BUSD", "USD", "ETH", "BTC"} {
		if len(sym) > len(q) && sym[len(sym)-len(q):] == q {
			return sym[:len(sym)-len(q)] + "/" + q
		}
	}
	return sym
}

var _ = log.Println // keep log dep referenced for future debug
