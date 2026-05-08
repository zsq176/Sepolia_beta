package config

import (
	"log"
	"os"
	"strconv"
	"strings"
	"sync"

	"github.com/ethereum/go-ethereum/common"
	"market-maker-service/internal/domain"
)

// Config holds the runtime configuration for the trading bot.
//
// The legacy fields (BTCBetaAddr, V3Pool, …) are kept so that scripts and the
// API server can keep working while we migrate. The new code path consumes the
// aggregated `Instruments` slice instead.
type Config struct {
	// --- Chain & RPC ---
	SepoliaRPC          string
	SepoliaRPCFallbacks []string
	ChainID             int64
	PrivateKey          string

	// --- Token addresses ---
	BTCBetaAddr  string
	USDTBetaAddr string
	ETHBetaAddr  string

	// --- Uniswap (V2/V3/V4) ---
	V2Factory     string
	V2Pair        string
	V2Router      string
	V3Factory     string
	V3Pool        string
	V3Quoter      string
	V3SwapRouter  string
	V4PoolManager string
	V4PoolBTCUSDT string
	V4PoolETHUSDT string
	V4PoolETHBTC  string
	// V4SwapRouter is the periphery swap entry point for V4. We default to the
	// canonical Uniswap PoolSwapTest deployed on Sepolia which exposes a
	// simple swap(PoolKey, SwapParams, TestSettings, hookData) function.
	V4SwapRouter string
	V4Quoter     string

	// --- Binance ---
	BinanceWSURL string
	APIPort      string
	DBPath       string
	RedisAddr    string
	RedisPass    string
	RedisDB      int64
	RedisQueue   string
	RedisLockTTL int64

	// --- Global execution / risk knobs ---
	// MinDeviation is the *global* minimum |dev| that triggers any rebalance.
	// Per-instrument deadbands can override this.
	MinDeviation float64
	// MaxSlippage is the default amountOutMin slippage budget (e.g. 0.01 = 1%).
	MaxSlippage float64
	// GasCapGwei caps the gas price we are willing to pay (network sees Gwei).
	GasCapGwei int64
	// MaxGasUSD caps the per-trade gas cost in USD; ETH price in USD is taken
	// from the BinanceETHUSDT feed at decision time.
	MaxGasUSD float64
	// MinNetEdgeUSD is the minimum net edge (after gas/slippage) to allow a trade.
	MinNetEdgeUSD float64
	// PollIntervalMs is how often the market data aggregator publishes a snapshot.
	PollIntervalMs int64
	// DailyBudgetUSD caps total estimated execution spend per UTC day.
	DailyBudgetUSD float64
	// TriggerBps is an extra global trigger threshold in basis points.
	TriggerBps float64
	// TWAPWindowSec controls the short window used by reference TWAP smoothing.
	TWAPWindowSec int64
	// TWAPSliceCount controls execution slicing count for TWAP-style rebalancing.
	TWAPSliceCount int64
	// Auth & operator controls.
	APIAuthSecret  string
	OperatorWallet string

	// --- Instrument matrix (the new code path) ---
	Instruments []*domain.Instrument

	// Runtime parameter overrides (hot updates).
	mu               sync.RWMutex
	runtimeOverrides map[string]float64
}

// Load reads configuration from environment variables and assembles the
// instrument matrix that the strategy/execution layers consume.
func Load() *Config {
	cfg := &Config{
		SepoliaRPC:          getEnv("SEPOLIA_RPC", "https://sepolia.infura.io/v3/YOUR_KEY"),
		SepoliaRPCFallbacks: splitCSV(getEnv("SEPOLIA_RPC_FALLBACKS", "")),
		ChainID:             getEnvInt64("CHAIN_ID", 11155111),
		PrivateKey:          getEnv("PRIVATE_KEY", ""),

		BTCBetaAddr:  getEnv("BTC_BETA_ADDR", ""),
		USDTBetaAddr: getEnv("USDT_BETA_ADDR", ""),
		ETHBetaAddr:  getEnv("ETH_BETA_ADDR", ""),

		V2Factory:     getEnv("V2_FACTORY", "0xF62c03E08ada871A0bEb309762E260a7a6a880E6"),
		V2Pair:        getEnv("V2_PAIR", ""),
		V2Router:      getEnv("V2_ROUTER", "0xeE567Fe1712Faf6149d80dA1E6934E354124CfE3"),
		V3Factory:     getEnv("V3_FACTORY", "0x0227628f3F023bb0B980b67D528571c95c6DaC1c"),
		V3Pool:        getEnv("V3_POOL", ""),
		V3Quoter:      getEnv("V3_QUOTER", "0xEd1f6473345F45b75F8179591dd5bA1888cf2FB3"),
		V3SwapRouter:  getEnv("V3_SWAP_ROUTER", "0x3bFA4769FB09eefC5a80d6E87c3B9C650f7Ae48E"),
		V4PoolManager: getEnv("V4_POOL_MANAGER", "0xE03A1074c86CFeDd5C142C4F04F1a1536e203543"),
		V4PoolBTCUSDT: getEnv("V4_POOL_BTC_USDT", ""),
		V4PoolETHUSDT: getEnv("V4_POOL_ETH_USDT", ""),
		V4PoolETHBTC:  getEnv("V4_POOL_ETH_BTC", ""),
		V4SwapRouter:  getEnv("V4_SWAP_ROUTER", "0x9b6b46e2c869aa39918db7f52f5557fe577b6eee"),
		V4Quoter:      getEnv("V4_QUOTER", "0x61b3f2011a92d183c7dbadbda940a7555ccf9227"),

		BinanceWSURL: getEnv("BINANCE_WS_URL", "wss://stream.binance.com:9443"),
		APIPort:      getEnv("API_PORT", "8080"),
		DBPath:       getEnv("DB_PATH", "data/trading.db"),
		RedisAddr:    getEnv("REDIS_ADDR", ""),
		RedisPass:    getEnv("REDIS_PASSWORD", ""),
		RedisDB:      getEnvInt64("REDIS_DB", 0),
		RedisQueue:   getEnv("REDIS_QUEUE", "mm:audit:events"),
		RedisLockTTL: getEnvInt64("REDIS_LOCK_TTL_SEC", 15),

		MinDeviation:   getEnvFloat("MIN_DEVIATION", 0.005),
		MaxSlippage:    getEnvFloat("MAX_SLIPPAGE", 0.01),
		GasCapGwei:     getEnvInt64("GAS_CAP_GWEI", 50),
		MaxGasUSD:      getEnvFloat("MAX_GAS_USD", 5.0),
		MinNetEdgeUSD:  getEnvFloat("MIN_NET_EDGE_USD", 1.0),
		PollIntervalMs: getEnvInt64("POLL_INTERVAL_MS", 5000),
		DailyBudgetUSD: getEnvFloat("DAILY_BUDGET_USD", 250.0),
		TriggerBps:     getEnvFloat("TRIGGER_BPS", 30.0),
		TWAPWindowSec:  getEnvInt64("TWAP_WINDOW_SEC", 45),
		TWAPSliceCount: getEnvInt64("TWAP_SLICE_COUNT", 3),
		APIAuthSecret:  getEnv("API_AUTH_SECRET", "beta-trading-secret"),
		OperatorWallet: strings.ToLower(getEnv("OPERATOR_WALLET", "")),
	}

	cfg.Instruments = buildInstruments(cfg)
	cfg.runtimeOverrides = map[string]float64{}
	return cfg
}

// buildInstruments assembles the four canonical instruments tracked by the bot.
//
// Instruments whose required pool addresses are missing are silently skipped so
// the bot can still come up partially configured.
func buildInstruments(c *Config) []*domain.Instrument {
	if c.BTCBetaAddr == "" || c.USDTBetaAddr == "" {
		return nil
	}

	btc := domain.Token{Symbol: "BTC-Beta", Address: common.HexToAddress(c.BTCBetaAddr), Decimals: 8}
	usdt := domain.Token{Symbol: "USDT-Beta", Address: common.HexToAddress(c.USDTBetaAddr), Decimals: 6}
	eth := domain.Token{Symbol: "ETH-Beta", Address: common.HexToAddress(c.ETHBetaAddr), Decimals: 18}

	defaultStrategy := domain.StrategyParams{
		Deadband:           maxFloat(getEnvFloat("DEADBAND", 0.0030), c.TriggerBps/10000.0),
		Gain:               getEnvFloat("GAIN", 0.30),
		MinNotionalUSD:     getEnvFloat("MIN_NOTIONAL_USD", 50),
		MaxNotionalUSD:     getEnvFloat("MAX_NOTIONAL_USD", 2000),
		CooldownSec:        getEnvInt64("COOLDOWN_SEC", 30),
		CircuitBreaker:     getEnvFloat("CIRCUIT_BREAKER", 0.05),
		CircuitCooldownSec: getEnvInt64("CIRCUIT_COOLDOWN_SEC", 120),
		MaxSlippage:        c.MaxSlippage,
	}

	out := make([]*domain.Instrument, 0, 4)

	// V2 BTC-Beta / USDT-Beta ↔ Binance BTC/USDT
	if c.V2Pair != "" {
		out = append(out, &domain.Instrument{
			ID:    "v2:BTCUSDT",
			Label: "V2 BTC-Beta/USDT-Beta",
			Venue: domain.VenueV2,
			Base:  btc, Quote: usdt,
			Source:   domain.PriceSource{Kind: domain.SourceCEX, BinanceSymbol: "BTCUSDT"},
			Pair:     common.HexToAddress(c.V2Pair),
			Strategy: defaultStrategy,
		})
	}

	// V3 BTC-Beta / USDT-Beta ↔ Binance BTC/USDT
	if c.V3Pool != "" {
		out = append(out, &domain.Instrument{
			ID:    "v3:BTCUSDT",
			Label: "V3 BTC-Beta/USDT-Beta",
			Venue: domain.VenueV3,
			Base:  btc, Quote: usdt,
			Source:   domain.PriceSource{Kind: domain.SourceCEX, BinanceSymbol: "BTCUSDT"},
			Pool:     common.HexToAddress(c.V3Pool),
			Strategy: defaultStrategy,
		})
	}

	// V4 BTC-Beta / USDT-Beta ↔ Binance BTC/USDT
	if c.V4PoolBTCUSDT != "" {
		out = append(out, &domain.Instrument{
			ID:    "v4:BTCUSDT",
			Label: "V4 BTC-Beta/USDT-Beta",
			Venue: domain.VenueV4,
			Base:  btc, Quote: usdt,
			Source:   domain.PriceSource{Kind: domain.SourceCEX, BinanceSymbol: "BTCUSDT"},
			V4Key:    parseV4Key(c.V4PoolBTCUSDT, btc.Address, usdt.Address),
			Strategy: defaultStrategy,
		})
	}

	// V4 ETH-Beta / USDT-Beta ↔ Binance ETH/USDT
	if c.ETHBetaAddr != "" && c.V4PoolETHUSDT != "" {
		out = append(out, &domain.Instrument{
			ID:    "v4:ETHUSDT",
			Label: "V4 ETH-Beta/USDT-Beta",
			Venue: domain.VenueV4,
			Base:  eth, Quote: usdt,
			Source:   domain.PriceSource{Kind: domain.SourceCEX, BinanceSymbol: "ETHUSDT"},
			V4Key:    parseV4Key(c.V4PoolETHUSDT, eth.Address, usdt.Address),
			Strategy: defaultStrategy,
		})
	}

	// V4 ETH-Beta / BTC-Beta ↔ derived ETHUSDT / BTCUSDT
	if c.ETHBetaAddr != "" && c.V4PoolETHBTC != "" {
		out = append(out, &domain.Instrument{
			ID:    "v4:ETHBTC",
			Label: "V4 ETH-Beta/BTC-Beta",
			Venue: domain.VenueV4,
			Base:  eth, Quote: btc,
			Source: domain.PriceSource{
				Kind:        domain.SourceDerived,
				Numerator:   "ETHUSDT",
				Denominator: "BTCUSDT",
			},
			V4Key:    parseV4Key(c.V4PoolETHBTC, eth.Address, btc.Address),
			Strategy: defaultStrategy,
		})
	}

	return out
}

// parseV4Key reads a V4 PoolKey description from env. Format:
//
//	"<currency0>,<currency1>,<fee>,<tickSpacing>,<hooks>"
//
// All addresses must be checksummed/lowercase 0x..., fee/tickSpacing decimal.
// If the env var is just an address (legacy), we synthesize a default key with
// fee=3000, tickSpacing=60, hooks=0x0 and currency0/1 sorted by token address.
func parseV4Key(raw string, base common.Address, quote common.Address) domain.V4PoolKey {
	parts := strings.Split(raw, ",")
	if len(parts) >= 5 {
		fee, _ := strconv.ParseUint(strings.TrimSpace(parts[2]), 10, 32)
		spacing, _ := strconv.ParseInt(strings.TrimSpace(parts[3]), 10, 32)
		return domain.V4PoolKey{
			Currency0:   common.HexToAddress(strings.TrimSpace(parts[0])),
			Currency1:   common.HexToAddress(strings.TrimSpace(parts[1])),
			Fee:         uint32(fee),
			TickSpacing: int32(spacing),
			Hooks:       common.HexToAddress(strings.TrimSpace(parts[4])),
		}
	}

	// Legacy single-address form: synthesize a default key.
	c0, c1 := base, quote
	if strings.ToLower(c0.Hex()) > strings.ToLower(c1.Hex()) {
		c0, c1 = c1, c0
	}
	log.Printf("[config] V4 pool key %q is legacy form, synthesizing default {fee=3000, spacing=60, hooks=0x0}", raw)
	return domain.V4PoolKey{
		Currency0:   c0,
		Currency1:   c1,
		Fee:         3000,
		TickSpacing: 60,
		Hooks:       common.Address{},
	}
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func getEnvFloat(key string, fallback float64) float64 {
	if v := os.Getenv(key); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return fallback
}

func getEnvInt64(key string, fallback int64) int64 {
	if v := os.Getenv(key); v != "" {
		if i, err := strconv.ParseInt(v, 10, 64); err == nil {
			return i
		}
	}
	return fallback
}

func splitCSV(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	seen := map[string]struct{}{}
	for _, p := range parts {
		v := strings.TrimSpace(p)
		if v == "" {
			continue
		}
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	return out
}

func maxFloat(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}

func (c *Config) GetRuntimeFloat(key string, fallback float64) float64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.runtimeOverrides == nil {
		return fallback
	}
	if v, ok := c.runtimeOverrides[key]; ok {
		return v
	}
	return fallback
}

func (c *Config) SetRuntimeFloat(key string, value float64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.runtimeOverrides == nil {
		c.runtimeOverrides = map[string]float64{}
	}
	c.runtimeOverrides[key] = value
}

func (c *Config) SnapshotRuntime() map[string]float64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make(map[string]float64, len(c.runtimeOverrides))
	for k, v := range c.runtimeOverrides {
		out[k] = v
	}
	return out
}
