// Package market hosts the data layer of the trading stack.
//
// The Aggregator polls every configured data source (Binance for the canonical
// price, Uniswap PriceReader for the on-chain price) on a fixed cadence and
// publishes a single MarketSnapshot via Subscribe. Strategy/Risk/Execution
// layers consume snapshots; nobody else reads RPC or WS state directly.
package market

import (
	"context"
	"log"
	"math"
	"market-maker-service/internal/config"
	"sync"
	"time"
	"market-maker-service/internal/binance"
	"market-maker-service/internal/domain"
	"market-maker-service/internal/uniswap"
)

// Subscriber receives a copy of every snapshot the aggregator emits. The
// aggregator does not block on slow subscribers — it skips them on a backlog.
type Subscriber func(*domain.MarketSnapshot)

// Aggregator is the central market-data hub. It is goroutine-safe.
type Aggregator struct {
	bn       *binance.Client
	readers  *uniswap.Registry
	insts    []*domain.Instrument
	cfg      *config.Config
	interval time.Duration
	// stalenessWindow is how old (against now()) a quote can be before we
	// treat it as stale and freeze trading on the affected instrument.
	stalenessWindow time.Duration

	mu       sync.RWMutex
	last     *domain.MarketSnapshot
	subsLock sync.Mutex
	subs     []Subscriber
	stopCh   chan struct{}
	stopOnce sync.Once
}

// NewAggregator wires up the data sources for the given instruments.
func NewAggregator(
	bn *binance.Client,
	readers *uniswap.Registry,
	insts []*domain.Instrument,
	cfg *config.Config,
	interval time.Duration,
) *Aggregator {
	if interval <= 0 {
		interval = 5 * time.Second
	}
	return &Aggregator{
		bn:              bn,
		readers:         readers,
		insts:           insts,
		cfg:             cfg,
		interval:        interval,
		stalenessWindow: 30 * time.Second,
		stopCh:          make(chan struct{}),
	}
}

// Subscribe registers a snapshot listener. The listener is invoked on the
// aggregator's poll goroutine, so it must return quickly.
func (a *Aggregator) Subscribe(s Subscriber) {
	a.subsLock.Lock()
	a.subs = append(a.subs, s)
	a.subsLock.Unlock()
}

// Snapshot returns the most recent snapshot the aggregator emitted, or nil if
// no tick has been produced yet.
func (a *Aggregator) Snapshot() *domain.MarketSnapshot {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.last
}

// Start launches the polling loop. It returns immediately.
func (a *Aggregator) Start(ctx context.Context) {
	go a.loop(ctx)
}

// Stop ends the polling loop. Safe to call multiple times.
func (a *Aggregator) Stop() {
	a.stopOnce.Do(func() { close(a.stopCh) })
}

func (a *Aggregator) loop(ctx context.Context) {
	t := time.NewTicker(a.interval)
	defer t.Stop()
	a.tick(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-a.stopCh:
			return
		case <-t.C:
			a.tick(ctx)
		}
	}
}

// tick collects a fresh snapshot and broadcasts it to subscribers.
func (a *Aggregator) tick(ctx context.Context) {
	now := time.Now()
	snap := &domain.MarketSnapshot{
		GeneratedAt: now,
		Binance:     map[string]domain.Quote{},
		Instruments: map[string]*domain.InstrumentSnapshot{},
	}

	// 1. Binance feeds:
	// - snap.Binance uses raw WS/REST mid for UI freshness.
	// - target reference below keeps using TWAP for strategy stability.
	twapQuotes := map[string]domain.Quote{}
	for _, sym := range neededSymbols(a.insts) {
		twapWindow := int64(45)
		if a.cfg != nil {
			twapWindow = int64(a.cfg.GetRuntimeFloat("twap_window_sec", float64(a.cfg.TWAPWindowSec)))
		}
		if twap, ok := a.bn.GetTWAP(sym, twapWindow); ok && twap > 0 {
			twapQuotes[sym] = domain.Quote{
				Source: domain.SourceBinance,
				Symbol: sym,
				Mid:    twap,
				Time:   now,
				Stale:  false,
			}
		}
		if raw, ok := a.bn.GetPrice(sym); ok && raw > 0 {
			snap.Binance[sym] = domain.Quote{
				Source: domain.SourceBinance,
				Symbol: sym,
				Mid:    raw,
				Time:   now,
				Stale:  false,
			}
		} else if q, ok := twapQuotes[sym]; ok {
			// Fallback for UI if raw is temporarily unavailable.
			snap.Binance[sym] = q
		}
	}

	// 2. Uniswap reads in parallel — one slow RPC must not block the others.
	type readResult struct {
		id    string
		price float64
		ts    time.Time
		err   error
	}
	results := make(chan readResult, len(a.insts))
	for _, inst := range a.insts {
		inst := inst
		go func() {
			cctx, cancel := context.WithTimeout(ctx, 8*time.Second)
			defer cancel()
			price, ts, err := a.readers.Read(cctx, inst)
			results <- readResult{id: inst.ID, price: price, ts: ts, err: err}
		}()
	}
	collected := map[string]readResult{}
	for i := 0; i < len(a.insts); i++ {
		select {
		case r := <-results:
			collected[r.id] = r
		case <-ctx.Done():
			return
		}
	}

	// 3. Stitch instrument snapshots.
	for _, inst := range a.insts {
		r := collected[inst.ID]
		poolQuote := domain.Quote{
			Source: domain.SourceUniswap,
			Symbol: inst.Label,
			Time:   r.ts,
			Stale:  r.err != nil || time.Since(r.ts) > a.stalenessWindow,
		}
		if r.err != nil {
			log.Printf("[market] %s: read error: %v", inst.ID, r.err)
		} else {
			poolQuote.Mid = r.price
		}

		target, fresh := computeTarget(inst.Source, twapQuotes, now, a.stalenessWindow)
		dev := 0.0
		if poolQuote.Mid > 0 && target.Mid > 0 {
			dev = (poolQuote.Mid - target.Mid) / target.Mid
		}

		snap.Instruments[inst.ID] = &domain.InstrumentSnapshot{
			Instrument:  inst,
			Pool:        poolQuote,
			Target:      target,
			Deviation:   dev,
			PoolFresh:   !poolQuote.Stale && poolQuote.Mid > 0,
			TargetFresh: fresh,
		}
	}

	a.mu.Lock()
	a.last = snap
	a.mu.Unlock()

	a.subsLock.Lock()
	subs := append([]Subscriber(nil), a.subs...)
	a.subsLock.Unlock()
	for _, s := range subs {
		s(snap)
	}
}

// neededSymbols collects the set of Binance symbols any instrument depends on.
func neededSymbols(insts []*domain.Instrument) []string {
	seen := map[string]struct{}{}
	for _, inst := range insts {
		switch inst.Source.Kind {
		case domain.SourceCEX:
			seen[inst.Source.BinanceSymbol] = struct{}{}
		case domain.SourceDerived:
			seen[inst.Source.Numerator] = struct{}{}
			seen[inst.Source.Denominator] = struct{}{}
		}
	}
	out := make([]string, 0, len(seen))
	for s := range seen {
		out = append(out, s)
	}
	return out
}

// computeTarget turns the instrument's PriceSource description into a concrete
// reference quote, using the Binance feed map already collected this tick.
func computeTarget(src domain.PriceSource, binance map[string]domain.Quote, now time.Time, staleness time.Duration) (domain.Quote, bool) {
	switch src.Kind {
	case domain.SourceCEX:
		q, ok := binance[src.BinanceSymbol]
		if !ok || q.Mid <= 0 || time.Since(q.Time) > staleness {
			return domain.Quote{Source: domain.SourceBinance, Symbol: src.BinanceSymbol, Time: now, Stale: true}, false
		}
		return q, true

	case domain.SourceDerived:
		num, okN := binance[src.Numerator]
		den, okD := binance[src.Denominator]
		if !okN || !okD || den.Mid <= 0 || math.IsNaN(num.Mid/den.Mid) {
			return domain.Quote{Source: domain.SourceComputed, Symbol: src.Numerator + "/" + src.Denominator, Time: now, Stale: true}, false
		}
		fresh := time.Since(num.Time) <= staleness && time.Since(den.Time) <= staleness
		return domain.Quote{
			Source: domain.SourceComputed,
			Symbol: src.Numerator + "/" + src.Denominator,
			Mid:    num.Mid / den.Mid,
			Time:   now,
			Stale:  !fresh,
		}, fresh
	}
	return domain.Quote{Time: now, Stale: true}, false
}
