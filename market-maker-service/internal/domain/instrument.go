package domain

import "github.com/ethereum/go-ethereum/common"

// Venue identifies which Uniswap version a pool lives on.
type Venue string

const (
	VenueV2 Venue = "v2"
	VenueV3 Venue = "v3"
	VenueV4 Venue = "v4"
)

// Token captures the on-chain ERC20 metadata we need.
type Token struct {
	Symbol   string
	Address  common.Address
	Decimals int
}

// V4PoolKey mirrors the on-chain PoolKey for a Uniswap V4 pool.
type V4PoolKey struct {
	Currency0   common.Address
	Currency1   common.Address
	Fee         uint32
	TickSpacing int32
	Hooks       common.Address
}

// SourceKind tells the strategy how to derive the target price.
type SourceKind string

const (
	// SourceCEX uses a Binance symbol as the reference price.
	SourceCEX SourceKind = "cex"
	// SourceDerived computes the target as a ratio of two CEX feeds.
	SourceDerived SourceKind = "derived"
)

// PriceSource describes how the canonical price for an instrument is sourced.
type PriceSource struct {
	Kind          SourceKind
	BinanceSymbol string // when Kind == SourceCEX
	Numerator     string // when Kind == SourceDerived (e.g. "ETHUSDT")
	Denominator   string // when Kind == SourceDerived (e.g. "BTCUSDT")
}

// StrategyParams controls the per-instrument sizing and throttling behavior.
type StrategyParams struct {
	// Deadband is the absolute deviation below which no trade is placed (e.g. 0.005 = 0.5%).
	Deadband float64
	// Gain is the proportional sizing factor: notional = Gain * deviation * NotionalUSD.
	Gain float64
	// MinNotionalUSD clamps the lower bound of any single rebalance.
	MinNotionalUSD float64
	// MaxNotionalUSD clamps the upper bound to limit market impact.
	MaxNotionalUSD float64
	// CooldownSec is the minimum seconds between two rebalance trades for this instrument.
	CooldownSec int64
	// CircuitBreaker is the deviation threshold above which the instrument is paused
	// (interpreted as a sign of manipulation rather than a normal arbitrage opportunity).
	CircuitBreaker float64
	// CircuitCooldownSec is how long the circuit stays open after tripping.
	CircuitCooldownSec int64
	// MaxSlippage caps the slippage tolerance applied to amountOutMin.
	MaxSlippage float64
}

// Instrument is the unit of work for the market data, strategy and execution stack.
type Instrument struct {
	ID     string
	Label  string
	Venue  Venue
	Base   Token
	Quote  Token
	Source PriceSource

	// Pair is the V2 pair address (only set for VenueV2).
	Pair common.Address
	// Pool is the V3 pool address (only set for VenueV3).
	Pool common.Address
	// V4Key describes the V4 pool (only set for VenueV4).
	V4Key V4PoolKey

	Strategy StrategyParams
}
