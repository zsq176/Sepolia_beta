package domain

import "time"

// QuoteSource identifies who produced a price observation.
type QuoteSource string

const (
	SourceBinance  QuoteSource = "binance"
	SourceUniswap  QuoteSource = "uniswap"
	SourceComputed QuoteSource = "computed"
)

// Quote is a single price observation expressed as base/quote.
type Quote struct {
	Source QuoteSource
	Symbol string    // human readable, e.g. "BTC/USDT"
	Mid    float64   // mid price in quote terms (1 base = Mid quote)
	Time   time.Time // when the observation was produced
	Stale  bool      // true if the observation is older than the freshness window
}

// InstrumentSnapshot bundles together the on-chain reading and the canonical
// (off-chain or derived) reference price for a single instrument.
type InstrumentSnapshot struct {
	Instrument *Instrument
	Pool       Quote   // observed Uniswap pool price
	Target     Quote   // canonical reference (Binance / derived)
	Deviation  float64 // (Pool.Mid - Target.Mid) / Target.Mid

	// Quality flags
	PoolFresh   bool
	TargetFresh bool
}

// MarketSnapshot is the system-wide view emitted by the aggregator on every tick.
type MarketSnapshot struct {
	GeneratedAt time.Time
	Binance     map[string]Quote                // keyed by symbol e.g. "BTCUSDT"
	Instruments map[string]*InstrumentSnapshot // keyed by Instrument.ID
}
