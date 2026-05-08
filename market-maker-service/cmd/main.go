package main

import (
	"context"
	"log"
	"math/big"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
	"market-maker-service/internal/api"
	"market-maker-service/internal/binance"
	"market-maker-service/internal/config"
	"market-maker-service/internal/db"
	"market-maker-service/internal/execution"
	"market-maker-service/internal/market"
	"market-maker-service/internal/risk"
	"market-maker-service/internal/strategy"
	syncer "market-maker-service/internal/sync"
	"market-maker-service/internal/uniswap"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/joho/godotenv"
)

// main wires the layered trading stack:
//
//	binance.Client + uniswap.Registry  --feeds-->  market.Aggregator
//	market.Aggregator                  --snap--->  sync.Engine (state) + execution.Service
//	execution.Service                  --gates-->  risk.Guard      --routes--> V3/V4 Executor
//	api.Server                         --reads-->  sync.Engine + risk.Guard + db.Store
//
// Each layer talks only through interfaces / value types from internal/domain.
func main() {
	log.Println("[main] starting trading bot…")
	if err := godotenv.Load(); err == nil {
		log.Println("[main] loaded .env")
	}

	cfg := config.Load()

	// --- chain client ---
	client, clientErr := ethclient.Dial(cfg.SepoliaRPC)
	if clientErr != nil {
		log.Printf("[main] sepolia rpc unavailable: %v (running in degraded mode)", clientErr)
	} else {
		defer client.Close()
		log.Println("[main] connected to sepolia")
	}

	// --- store ---
	store, err := db.NewStore(cfg.DBPath)
	if err != nil {
		log.Fatalf("[main] db init: %v", err)
	}
	defer store.Close()

	// --- binance ---
	bn := binance.NewClient(cfg.BinanceWSURL)
	bnSymbols := []string{"btcusdt", "ethusdt"}
	if err := bn.Connect(bnSymbols); err != nil {
		log.Printf("[main] binance ws warn: %v", err)
	}
	defer bn.Close()

	// --- chain id ---
	chainID := big.NewInt(cfg.ChainID)
	if client != nil {
		if id, err := client.ChainID(context.Background()); err == nil {
			chainID = id
		}
	}

	// --- risk guard (needs an ETH price provider for USD gas budgeting) ---
	guard := risk.NewGuard(cfg, client, func() (float64, bool) {
		return bn.GetPrice("ETHUSDT")
	})

	// --- engine (state mirror used by API) ---
	engine := syncer.NewEngine(store, guard)
	defer engine.Stop()

	// --- market data layer ---
	var aggregator *market.Aggregator
	var execSvc *execution.Service
	if client != nil && len(cfg.Instruments) > 0 {
		registry := uniswap.NewRegistry(client, common.HexToAddress(cfg.V4PoolManager))
		aggregator = market.NewAggregator(
			bn,
			registry,
			cfg.Instruments,
			cfg,
			time.Duration(cfg.PollIntervalMs)*time.Millisecond,
		)
		aggregator.Subscribe(engine.OnSnapshot)

		// --- strategy + execution ---
		if cfg.PrivateKey != "" {
			pk, err := execution.PrivateKeyFromHex(cleanHex(cfg.PrivateKey))
			if err != nil {
				log.Printf("[main] invalid private key: %v (running read-only)", err)
			} else {
				strat := strategy.NewEngine(cfg.Instruments, cfg)
				svc := execution.NewService(
					client, pk, chainID,
					common.HexToAddress(cfg.V3SwapRouter),
					common.HexToAddress(cfg.V4SwapRouter),
					common.HexToAddress(cfg.V4PoolManager),
					strat, guard, store, cfg,
				)
				execSvc = svc
				aggregator.Subscribe(svc.OnSnapshot)
				log.Printf("[main] execution service ready (operator=%s)", svc.Address().Hex())
			}
		} else {
			log.Println("[main] PRIVATE_KEY not set — running in read-only mode")
		}

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		aggregator.Start(ctx)
		log.Printf("[main] market aggregator started (%d instruments, interval=%dms)",
			len(cfg.Instruments), cfg.PollIntervalMs)
	} else {
		log.Println("[main] aggregator disabled: no chain client or no instruments configured")
	}

	// --- api server ---
	apiCfg := &api.ConfigAPI{
		BTCBetaAddr:   cfg.BTCBetaAddr,
		USDTBetaAddr:  cfg.USDTBetaAddr,
		ETHBetaAddr:   cfg.ETHBetaAddr,
		V2Pair:        cfg.V2Pair,
		V3Pool:        cfg.V3Pool,
		V4PoolManager: cfg.V4PoolManager,
		V4SwapRouter:  cfg.V4SwapRouter,
		ChainID:       chainID.Int64(),
	}
	server := api.NewServer(store, engine, guard, bn, cfg.Instruments, client, apiCfg, cfg, cfg.APIPort)
	go server.Start()

	log.Printf("[main] api: http://localhost:%s", cfg.APIPort)
	log.Printf("[main] ws : ws://localhost:%s/ws", cfg.APIPort)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	log.Println("[main] shutting down…")
	if aggregator != nil {
		aggregator.Stop()
	}
	if execSvc != nil {
		execSvc.Close()
	}
}

// cleanHex strips a leading "0x" if the operator copied it that way.
func cleanHex(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "0x") || strings.HasPrefix(s, "0X") {
		return s[2:]
	}
	return s
}
