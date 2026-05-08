package api

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"math/big"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
	"market-maker-service/internal/binance"
	"market-maker-service/internal/config"
	"market-maker-service/internal/db"
	"market-maker-service/internal/domain"
	"market-maker-service/internal/risk"
	syncer "market-maker-service/internal/sync"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/gorilla/websocket"
)

// Server is the HTTP+WebSocket front-end for the bot.
type Server struct {
	store  *db.Store
	engine *syncer.Engine
	guard  *risk.Guard
	bn     *binance.Client
	insts  []*domain.Instrument
	wsHub  *WSHub
	port   string
	client interface {
		BalanceAt(ctx context.Context, account common.Address, blockNumber *big.Int) (*big.Int, error)
		CallContract(ctx context.Context, call ethereum.CallMsg, blockNumber *big.Int) ([]byte, error)
	}
	cfg    *ConfigAPI
	auth   *authState
	rtCfg  *config.Config
	cacheMu     sync.RWMutex
	klineCache  map[string]cachedKlines
}

type cachedKlines struct {
	points   []db.PricePoint
	cachedAt time.Time
}

// ConfigAPI is the on-the-wire shape of the bot's bootstrap configuration.
type ConfigAPI struct {
	BTCBetaAddr   string `json:"btc_beta_addr"`
	USDTBetaAddr  string `json:"usdt_beta_addr"`
	ETHBetaAddr   string `json:"eth_beta_addr"`
	V2Pair        string `json:"v2_pair"`
	V3Pool        string `json:"v3_pool"`
	V4PoolManager string `json:"v4_pool_manager"`
	V4SwapRouter  string `json:"v4_swap_router"`
	ChainID       int64  `json:"chain_id"`
}

type authState struct {
	secret []byte
	mu     sync.Mutex
	nonce  map[string]string
}

// WSHub fans out compact JSON snapshots to every connected dashboard.
type WSHub struct {
	clients   map[*websocket.Conn]*wsClientState
	mu        sync.RWMutex
	broadcast chan map[string]float64
	upgrader  websocket.Upgrader
}

type wsClientState struct {
	pairs map[string]bool
}

// NewServer wires the API server.
func NewServer(
	store *db.Store,
	engine *syncer.Engine,
	guard *risk.Guard,
	bn *binance.Client,
	insts []*domain.Instrument,
	client interface {
		BalanceAt(ctx context.Context, account common.Address, blockNumber *big.Int) (*big.Int, error)
		CallContract(ctx context.Context, call ethereum.CallMsg, blockNumber *big.Int) ([]byte, error)
	},
	cfg *ConfigAPI,
	rtCfg *config.Config,
	port string,
) *Server {
	hub := &WSHub{
		clients:   make(map[*websocket.Conn]*wsClientState),
		broadcast: make(chan map[string]float64, 256),
		upgrader: websocket.Upgrader{
			CheckOrigin: func(r *http.Request) bool { return true },
		},
	}
	go hub.run()
	return &Server{
		store: store, engine: engine, guard: guard, insts: insts,
		bn: bn,
		wsHub: hub, port: port, client: client, cfg: cfg,
		auth: &authState{
			secret: []byte(defaultString(os.Getenv("API_AUTH_SECRET"), "beta-trading-secret")),
			nonce:  make(map[string]string),
		},
		rtCfg: rtCfg,
		klineCache: map[string]cachedKlines{},
	}
}

// Start blocks while serving HTTP. Run in its own goroutine.
func (s *Server) Start() {
	mux := http.NewServeMux()

	mux.HandleFunc("/api/prices", s.handlePrices)
	mux.HandleFunc("/api/market/klines", s.handleKlines)
	mux.HandleFunc("/api/market/prices", s.handlePrices)
	mux.HandleFunc("/api/klines", s.handleKlines)
	mux.HandleFunc("/api/snapshot", s.handleSnapshot)
	mux.HandleFunc("/api/instruments", s.handleInstruments)
	mux.HandleFunc("/api/risk", s.handleRisk)
	mux.HandleFunc("/api/trades", s.handleTrades)
	mux.HandleFunc("/api/positions", s.handlePositions)
	mux.HandleFunc("/api/account/trades", s.handleAccountTrades)
	mux.HandleFunc("/api/account/positions", s.handleAccountPositions)
	mux.HandleFunc("/api/account/op-logs", s.handleAccountOpLogs)
	mux.HandleFunc("/api/replay/decisions", s.handleReplayDecisions)
	mux.HandleFunc("/api/sop/incidents", s.handleIncidents)
	mux.HandleFunc("/api/admin/params", s.handleRuntimeParams)
	mux.HandleFunc("/api/auth/challenge", s.handleAuthChallenge)
	mux.HandleFunc("/api/auth/verify", s.handleAuthVerify)
	mux.HandleFunc("/api/balances", s.handleBalances)
	mux.HandleFunc("/api/config", s.handleConfig)
	mux.HandleFunc("/api/health", s.handleHealth)
	mux.HandleFunc("/ws", s.handleWS)

	go s.priceBroadcaster()

	log.Printf("[api] listening on :%s", s.port)
	log.Fatal(http.ListenAndServe(":"+s.port, corsMiddleware(mux)))
}

// --- handlers ---

func (s *Server) handlePrices(w http.ResponseWriter, r *http.Request) {
	if s.engine == nil {
		prices := s.realtimeBinancePrices(map[string]float64{})
		writeJSON(w, prices)
		return
	}
	prices := s.engine.GetPrices()
	prices = s.realtimeBinancePrices(prices)
	prices["circuit_broken"] = boolToFloat(s.engine.IsCircuitBroken())
	writeJSON(w, prices)
}

// handleSnapshot returns the rich market view the strategy/risk layers see.
// This is the canonical "what is the bot looking at right now" endpoint.
func (s *Server) handleSnapshot(w http.ResponseWriter, r *http.Request) {
	if s.engine == nil {
		writeJSON(w, map[string]any{})
		return
	}
	snap := s.engine.LatestSnapshot()
	if snap == nil {
		writeJSON(w, map[string]any{"ready": false})
		return
	}
	out := map[string]any{
		"ready":        true,
		"generated_at": snap.GeneratedAt.Unix(),
		"binance":      flattenBinance(snap),
		"instruments":  flattenInstruments(snap),
	}
	writeJSON(w, out)
}

// handleInstruments lists the configured instrument matrix (no live state).
func (s *Server) handleInstruments(w http.ResponseWriter, r *http.Request) {
	out := make([]map[string]any, 0, len(s.insts))
	for _, i := range s.insts {
		out = append(out, map[string]any{
			"id":     i.ID,
			"label":  i.Label,
			"venue":  string(i.Venue),
			"base":   i.Base.Symbol,
			"quote":  i.Quote.Symbol,
			"source": map[string]any{"kind": string(i.Source.Kind), "binance": i.Source.BinanceSymbol, "numerator": i.Source.Numerator, "denominator": i.Source.Denominator},
			"strategy": map[string]any{
				"deadband":         i.Strategy.Deadband,
				"gain":             i.Strategy.Gain,
				"min_notional_usd": i.Strategy.MinNotionalUSD,
				"max_notional_usd": i.Strategy.MaxNotionalUSD,
				"cooldown_sec":     i.Strategy.CooldownSec,
				"circuit_breaker":  i.Strategy.CircuitBreaker,
				"max_slippage":     i.Strategy.MaxSlippage,
			},
		})
	}
	writeJSON(w, out)
}

// handleRisk exposes per-instrument risk state (circuit breakers, back-offs).
func (s *Server) handleRisk(w http.ResponseWriter, r *http.Request) {
	if s.guard == nil {
		writeJSON(w, map[string]any{})
		return
	}
	writeJSON(w, s.guard.Snapshot())
}

func (s *Server) handleTrades(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		s.handleTradesPost(w, r)
		return
	}
	limit := 100
	if l, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && l > 0 {
		limit = l
	}
	trades, err := s.store.GetTrades(limit)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	writeJSON(w, trades)
}

func (s *Server) handleAccountTrades(w http.ResponseWriter, r *http.Request) {
	claims, ok := s.requireAuth(w, r)
	if !ok {
		return
	}
	limit := 100
	if l, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && l > 0 {
		limit = l
	}
	trades, err := s.store.GetTradesByWallet(claims.Address, limit)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	writeJSON(w, trades)
}

func (s *Server) handleTradesPost(w http.ResponseWriter, r *http.Request) {
	claims, ok := s.requireAuth(w, r)
	if !ok {
		return
	}
	var req struct {
		Pair   string  `json:"pair"`
		Action string  `json:"action"`
		Amount string  `json:"amount"`
		Price  float64 `json:"price"`
		TxHash string  `json:"tx_hash"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid body", 400)
		return
	}
	t := &db.Trade{
		Timestamp: time.Now().Unix(),
		Wallet:    claims.Address,
		Pair:      req.Pair,
		Action:    req.Action,
		Amount:    req.Amount,
		Price:     req.Price,
		TxHash:    req.TxHash,
		Status:    "submitted",
		Source:    "user",
	}
	if err := s.store.InsertTrade(t); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	_ = s.store.InsertUserOpLog(&db.UserOpLog{
		Timestamp: time.Now().Unix(),
		Wallet:    claims.Address,
		Operation: "manual_trade_submit",
		Target:    req.Pair,
		Detail:    fmt.Sprintf("action=%s amount=%s", req.Action, req.Amount),
		Status:    "submitted",
		TxHash:    req.TxHash,
	})
	writeJSON(w, map[string]string{"status": "ok"})
}

func (s *Server) handlePositions(w http.ResponseWriter, r *http.Request) {
	positions, err := s.store.GetPositions()
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	writeJSON(w, positions)
}

func (s *Server) handleAccountPositions(w http.ResponseWriter, r *http.Request) {
	claims, ok := s.requireAuth(w, r)
	if !ok {
		return
	}
	positions, err := s.store.GetPositionsByWallet(claims.Address)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	writeJSON(w, positions)
}

func (s *Server) handleKlines(w http.ResponseWriter, r *http.Request) {
	pair := r.URL.Query().Get("pair")
	interval := r.URL.Query().Get("interval")
	if interval == "" {
		interval = "1m"
	}
	now := time.Now()
	from := now.Add(-24 * time.Hour)
	to := now
	if t, err := strconv.ParseInt(r.URL.Query().Get("from"), 10, 64); err == nil {
		from = time.Unix(t, 0)
	}
	if t, err := strconv.ParseInt(r.URL.Query().Get("to"), 10, 64); err == nil {
		to = time.Unix(t, 0)
	}
	// Guard window to protect DB from full-history scans.
	if to.Sub(from) > 7*24*time.Hour {
		http.Error(w, "time window too large (max 7d)", http.StatusBadRequest)
		return
	}
	limit := 2000
	if l, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && l > 0 {
		limit = l
	}
	cacheKey := fmt.Sprintf("%s|%s|%d|%d|%d", pair, interval, from.Unix(), to.Unix(), limit)
	s.cacheMu.RLock()
	if c, ok := s.klineCache[cacheKey]; ok && time.Since(c.cachedAt) <= 2*time.Second {
		s.cacheMu.RUnlock()
		writeJSON(w, c.points)
		return
	}
	s.cacheMu.RUnlock()
	points, err := s.store.GetPrices(pair, interval, from, to, limit)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	s.cacheMu.Lock()
	s.klineCache[cacheKey] = cachedKlines{points: points, cachedAt: time.Now()}
	s.cacheMu.Unlock()
	writeJSON(w, points)
}

func (s *Server) handleBalances(w http.ResponseWriter, r *http.Request) {
	address := r.URL.Query().Get("address")
	if address == "" || s.client == nil {
		writeJSON(w, map[string]string{})
		return
	}
	addr := common.HexToAddress(address)
	balances := make(map[string]string)

	bal, err := s.client.BalanceAt(context.Background(), addr, nil)
	if err == nil {
		balances["ETH"] = new(big.Float).Quo(
			new(big.Float).SetInt(bal),
			new(big.Float).SetFloat64(math.Pow10(18)),
		).String()
	}

	for _, tokenAddr := range []string{s.cfg.BTCBetaAddr, s.cfg.USDTBetaAddr, s.cfg.ETHBetaAddr} {
		if tokenAddr == "" {
			continue
		}
		token := common.HexToAddress(tokenAddr)
		data := append(common.FromHex("0x70a08231"), common.LeftPadBytes(addr.Bytes(), 32)...)
		result, err := s.client.CallContract(context.Background(), ethereum.CallMsg{
			From: addr, To: &token, Data: data,
		}, nil)
		if err == nil && len(result) >= 32 {
			symbol := "UNKNOWN"
			switch strings.ToLower(tokenAddr) {
			case strings.ToLower(s.cfg.BTCBetaAddr):
				symbol = "BTC-Beta"
			case strings.ToLower(s.cfg.USDTBetaAddr):
				symbol = "USDT-Beta"
			case strings.ToLower(s.cfg.ETHBetaAddr):
				symbol = "ETH-Beta"
			}
			b := new(big.Int).SetBytes(result)
			balances[symbol] = b.String()
		}
	}
	writeJSON(w, balances)
}

func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	if s.cfg != nil {
		writeJSON(w, s.cfg)
		return
	}
	writeJSON(w, map[string]string{})
}

func (s *Server) handleAuthChallenge(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Address string `json:"address"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}
	addr := strings.ToLower(strings.TrimSpace(req.Address))
	if !common.IsHexAddress(addr) {
		http.Error(w, "invalid address", http.StatusBadRequest)
		return
	}
	nonce := fmt.Sprintf("login:%d", time.Now().UnixNano())
	s.auth.mu.Lock()
	s.auth.nonce[addr] = nonce
	s.auth.mu.Unlock()
	writeJSON(w, map[string]string{
		"address": addr,
		"nonce":   nonce,
		"message": fmt.Sprintf("Beta Trading login\naddress=%s\nnonce=%s", addr, nonce),
	})
}

func (s *Server) handleAuthVerify(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Address   string `json:"address"`
		Nonce     string `json:"nonce"`
		Signature string `json:"signature"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}
	addr := strings.ToLower(strings.TrimSpace(req.Address))
	s.auth.mu.Lock()
	expectedNonce := s.auth.nonce[addr]
	s.auth.mu.Unlock()
	if expectedNonce == "" || expectedNonce != req.Nonce {
		http.Error(w, "invalid nonce", http.StatusUnauthorized)
		return
	}
	msg := fmt.Sprintf("Beta Trading login\naddress=%s\nnonce=%s", addr, req.Nonce)
	if !verifySignature(addr, msg, req.Signature) {
		http.Error(w, "signature verification failed", http.StatusUnauthorized)
		return
	}
	s.auth.mu.Lock()
	delete(s.auth.nonce, addr)
	s.auth.mu.Unlock()
	role := "viewer"
	if strings.EqualFold(defaultString(os.Getenv("OPERATOR_WALLET"), ""), addr) {
		role = "operator"
	}
	token, err := s.signToken(addr, role, time.Now().Add(8*time.Hour))
	if err != nil {
		http.Error(w, "token error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"token": token, "address": addr, "role": role, "expires_in_sec": 28800})
}

func (s *Server) handleReplayDecisions(w http.ResponseWriter, r *http.Request) {
	_, ok := s.requireAuth(w, r)
	if !ok {
		return
	}
	instrumentID := r.URL.Query().Get("instrument_id")
	now := time.Now()
	from := now.Add(-1 * time.Hour)
	to := now
	if t, err := strconv.ParseInt(r.URL.Query().Get("from"), 10, 64); err == nil && t > 0 {
		from = time.Unix(t, 0)
	}
	if t, err := strconv.ParseInt(r.URL.Query().Get("to"), 10, 64); err == nil && t > 0 {
		to = time.Unix(t, 0)
	}
	limit := 500
	if l, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && l > 0 {
		limit = l
	}
	logs, err := s.store.GetDecisionLogs(instrumentID, from, to, limit)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	writeJSON(w, logs)
}

func (s *Server) handleIncidents(w http.ResponseWriter, r *http.Request) {
	claims, ok := s.requireAuth(w, r)
	if !ok {
		return
	}
	if claims.Role != "operator" && claims.Role != "admin" {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Kind    string         `json:"kind"`
		Status  string         `json:"status"`
		Details map[string]any `json:"details"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}
	if err := s.store.InsertIncident(req.Kind, req.Status, req.Details); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	writeJSON(w, map[string]string{"status": "ok"})
}

func (s *Server) handleRuntimeParams(w http.ResponseWriter, r *http.Request) {
	if s.rtCfg == nil {
		http.Error(w, "runtime config unavailable", http.StatusServiceUnavailable)
		return
	}
	claims, ok := s.requireAuth(w, r)
	if !ok {
		return
	}
	if r.Method == http.MethodGet {
		out := map[string]float64{
			"trigger_bps":           s.rtCfg.GetRuntimeFloat("trigger_bps", s.rtCfg.TriggerBps),
			"cooldown_sec":          s.rtCfg.GetRuntimeFloat("cooldown_sec", 30),
			"max_gas_usd_per_trade": s.rtCfg.GetRuntimeFloat("max_gas_usd_per_trade", s.rtCfg.MaxGasUSD),
			"daily_budget_usd":      s.rtCfg.GetRuntimeFloat("daily_budget_usd", s.rtCfg.DailyBudgetUSD),
			"twap_slice_count":      s.rtCfg.GetRuntimeFloat("twap_slice_count", float64(s.rtCfg.TWAPSliceCount)),
			"twap_window_sec":       s.rtCfg.GetRuntimeFloat("twap_window_sec", float64(s.rtCfg.TWAPWindowSec)),
			"min_net_edge_usd":      s.rtCfg.GetRuntimeFloat("min_net_edge_usd", s.rtCfg.MinNetEdgeUSD),
		}
		writeJSON(w, out)
		return
	}
	if claims.Role != "operator" && claims.Role != "admin" {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Key    string  `json:"key"`
		Value  float64 `json:"value"`
		Reason string  `json:"reason"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}
	if req.Reason == "" {
		http.Error(w, "reason required", http.StatusBadRequest)
		return
	}
	if !isWhitelistedParam(req.Key) {
		http.Error(w, "param key not allowed", http.StatusBadRequest)
		return
	}
	if !isParamInSafeRange(req.Key, req.Value) {
		http.Error(w, "value out of safe range", http.StatusBadRequest)
		return
	}
	old := s.rtCfg.GetRuntimeFloat(req.Key, s.paramFallback(req.Key))
	s.rtCfg.SetRuntimeFloat(req.Key, req.Value)
	_ = s.store.InsertParamChange(claims.Address, req.Key, fmt.Sprintf("%f", old), fmt.Sprintf("%f", req.Value), req.Reason)
	writeJSON(w, map[string]any{"status": "ok", "key": req.Key, "old": old, "new": req.Value})
}

func (s *Server) handleAccountOpLogs(w http.ResponseWriter, r *http.Request) {
	claims, ok := s.requireAuth(w, r)
	if !ok {
		return
	}
	switch r.Method {
	case http.MethodGet:
		limit := 200
		if l, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && l > 0 {
			limit = l
		}
		logs, err := s.store.GetUserOpLogsByWallet(claims.Address, limit)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		writeJSON(w, logs)
	case http.MethodPost:
		var req struct {
			Operation string `json:"operation"`
			Target    string `json:"target"`
			Detail    string `json:"detail"`
			Status    string `json:"status"`
			TxHash    string `json:"tx_hash"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid body", http.StatusBadRequest)
			return
		}
		if strings.TrimSpace(req.Operation) == "" {
			http.Error(w, "operation required", http.StatusBadRequest)
			return
		}
		if err := s.store.InsertUserOpLog(&db.UserOpLog{
			Timestamp: time.Now().Unix(),
			Wallet:    claims.Address,
			Operation: req.Operation,
			Target:    defaultString(req.Target, "-"),
			Detail:    defaultString(req.Detail, "-"),
			Status:    defaultString(req.Status, "ok"),
			TxHash:    req.TxHash,
		}); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		writeJSON(w, map[string]string{"status": "ok"})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	conn, err := s.wsHub.upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("ws upgrade error: %v", err)
		return
	}
	s.wsHub.mu.Lock()
	s.wsHub.clients[conn] = &wsClientState{pairs: map[string]bool{}}
	s.wsHub.mu.Unlock()

	go func() {
		defer func() {
			s.wsHub.mu.Lock()
			delete(s.wsHub.clients, conn)
			s.wsHub.mu.Unlock()
			conn.Close()
		}()
		for {
			_, body, err := conn.ReadMessage()
			if err != nil {
				return
			}
			var req struct {
				Type  string   `json:"type"`
				Pairs []string `json:"pairs"`
			}
			if err := json.Unmarshal(body, &req); err != nil {
				continue
			}
			if req.Type != "subscribe" {
				continue
			}
			pairs := make(map[string]bool, len(req.Pairs))
			for _, p := range req.Pairs {
				pp := strings.TrimSpace(p)
				if pp != "" {
					pairs[pp] = true
				}
			}
			s.wsHub.mu.Lock()
			if st, ok := s.wsHub.clients[conn]; ok {
				st.pairs = pairs
			}
			s.wsHub.mu.Unlock()
		}
	}()
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	status := map[string]any{
		"status": "ok",
		"engine": "ok",
		"rpc":    "connected",
		"db":     "connected",
	}
	if s.engine == nil {
		status["engine"] = "nil"
	}
	if s.client == nil {
		status["rpc"] = "disconnected"
	}
	if s.store == nil {
		status["db"] = "nil"
	}
	if s.engine != nil {
		status["circuit_broken"] = s.engine.IsCircuitBroken()
		status["last_snapshot"] = func() int64 {
			if snap := s.engine.LatestSnapshot(); snap != nil {
				return snap.GeneratedAt.Unix()
			}
			return 0
		}()
	}
	writeJSON(w, status)
}

func (s *Server) priceBroadcaster() {
	t := time.NewTicker(500 * time.Millisecond)
	defer t.Stop()
	for range t.C {
		if s.engine == nil {
			continue
		}
		prices := s.engine.GetPrices()
		prices = s.realtimeBinancePrices(prices)
		prices["circuit_broken"] = boolToFloat(s.engine.IsCircuitBroken())
		select {
		case s.wsHub.broadcast <- prices:
		default:
		}
	}
}

func (s *Server) realtimeBinancePrices(prices map[string]float64) map[string]float64 {
	if prices == nil {
		prices = map[string]float64{}
	}
	if s.bn == nil {
		return prices
	}
	if btc, ok := s.bn.GetPrice("BTCUSDT"); ok && btc > 0 {
		prices["BTCUSDT"] = btc
		prices["binance:BTC/USDT"] = btc
	}
	if eth, ok := s.bn.GetPrice("ETHUSDT"); ok && eth > 0 {
		prices["ETHUSDT"] = eth
		prices["binance:ETH/USDT"] = eth
	}
	return prices
}

func (h *WSHub) run() {
	for prices := range h.broadcast {
		h.mu.RLock()
		for conn, st := range h.clients {
			payload := filterPricesForClient(prices, st)
			msg, err := json.Marshal(payload)
			if err != nil {
				continue
			}
			conn.WriteMessage(websocket.TextMessage, msg)
		}
		h.mu.RUnlock()
	}
}

func filterPricesForClient(prices map[string]float64, st *wsClientState) map[string]float64 {
	if st == nil || len(st.pairs) == 0 {
		return prices
	}
	out := map[string]float64{
		"circuit_broken": prices["circuit_broken"],
	}
	for k, v := range prices {
		if st.pairs[k] {
			out[k] = v
		}
	}
	return out
}

// --- helpers ---

func flattenBinance(snap *domain.MarketSnapshot) map[string]any {
	out := make(map[string]any, len(snap.Binance))
	for sym, q := range snap.Binance {
		out[sym] = map[string]any{
			"mid":   q.Mid,
			"time":  q.Time.Unix(),
			"stale": q.Stale,
		}
	}
	return out
}

func flattenInstruments(snap *domain.MarketSnapshot) map[string]any {
	out := make(map[string]any, len(snap.Instruments))
	for id, is := range snap.Instruments {
		out[id] = map[string]any{
			"label":         is.Instrument.Label,
			"venue":         string(is.Instrument.Venue),
			"pool_price":    is.Pool.Mid,
			"pool_time":     is.Pool.Time.Unix(),
			"pool_fresh":    is.PoolFresh,
			"target_price":  is.Target.Mid,
			"target_source": string(is.Target.Source),
			"target_fresh":  is.TargetFresh,
			"deviation":     is.Deviation,
			"quality_level": qualityLevel(is),
		}
	}
	return out
}

func qualityLevel(is *domain.InstrumentSnapshot) string {
	if !is.PoolFresh || !is.TargetFresh {
		return "stale"
	}
	if is.Pool.Stale || is.Target.Stale {
		return "fallback"
	}
	if is.Pool.Mid <= 0 || is.Target.Mid <= 0 {
		return "invalid"
	}
	return "live"
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	json.NewEncoder(w).Encode(v)
}

func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
		if r.Method == http.MethodOptions {
			w.WriteHeader(200)
			return
		}
		next.ServeHTTP(w, r)
	})
}

type authClaims struct {
	Address string `json:"address"`
	Role    string `json:"role"`
	Exp     int64  `json:"exp"`
}

func (s *Server) requireAuth(w http.ResponseWriter, r *http.Request) (*authClaims, bool) {
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, "Bearer ") {
		http.Error(w, "missing bearer token", http.StatusUnauthorized)
		return nil, false
	}
	claims, err := s.parseToken(strings.TrimPrefix(h, "Bearer "))
	if err != nil {
		http.Error(w, "invalid token", http.StatusUnauthorized)
		return nil, false
	}
	if time.Now().Unix() >= claims.Exp {
		http.Error(w, "token expired", http.StatusUnauthorized)
		return nil, false
	}
	return claims, true
}

func (s *Server) signToken(address, role string, exp time.Time) (string, error) {
	payload := authClaims{Address: strings.ToLower(address), Role: role, Exp: exp.Unix()}
	raw, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	body := base64.RawURLEncoding.EncodeToString(raw)
	mac := hmac.New(sha256.New, s.auth.secret)
	mac.Write([]byte(body))
	sig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return body + "." + sig, nil
}

func (s *Server) parseToken(token string) (*authClaims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 2 {
		return nil, fmt.Errorf("bad token format")
	}
	body := parts[0]
	expectedMAC := hmac.New(sha256.New, s.auth.secret)
	expectedMAC.Write([]byte(body))
	expected := expectedMAC.Sum(nil)
	got, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || !hmac.Equal(expected, got) {
		return nil, fmt.Errorf("bad token signature")
	}
	payloadRaw, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil {
		return nil, err
	}
	var c authClaims
	if err := json.Unmarshal(payloadRaw, &c); err != nil {
		return nil, err
	}
	return &c, nil
}

func verifySignature(address, message, signatureHex string) bool {
	sig := common.FromHex(signatureHex)
	if len(sig) != 65 {
		return false
	}
	if sig[64] >= 27 {
		sig[64] -= 27
	}
	hash := accountsTextHash([]byte(message))
	pub, err := crypto.SigToPub(hash, sig)
	if err != nil {
		return false
	}
	recovered := strings.ToLower(crypto.PubkeyToAddress(*pub).Hex())
	return recovered == strings.ToLower(address)
}

func accountsTextHash(data []byte) []byte {
	msg := fmt.Sprintf("\x19Ethereum Signed Message:\n%d%s", len(data), data)
	return crypto.Keccak256([]byte(msg))
}

func defaultString(v, fallback string) string {
	if strings.TrimSpace(v) == "" {
		return fallback
	}
	return v
}

func isWhitelistedParam(k string) bool {
	switch k {
	case "trigger_bps", "cooldown_sec", "max_gas_usd_per_trade", "daily_budget_usd", "twap_slice_count", "twap_window_sec", "min_net_edge_usd", "max_gas_gwei":
		return true
	default:
		return false
	}
}

func isParamInSafeRange(k string, v float64) bool {
	switch k {
	case "trigger_bps":
		return v >= 1 && v <= 500
	case "cooldown_sec":
		return v >= 1 && v <= 3600
	case "max_gas_usd_per_trade":
		return v >= 0.1 && v <= 50
	case "daily_budget_usd":
		return v >= 1 && v <= 20000
	case "twap_slice_count":
		return v >= 1 && v <= 10
	case "twap_window_sec":
		return v >= 10 && v <= 300
	case "min_net_edge_usd":
		return v >= 0 && v <= 100
	case "max_gas_gwei":
		return v >= 1 && v <= 500
	default:
		return false
	}
}

func (s *Server) paramFallback(key string) float64 {
	if s.rtCfg == nil {
		return 0
	}
	switch key {
	case "trigger_bps":
		return s.rtCfg.TriggerBps
	case "cooldown_sec":
		return 30
	case "max_gas_usd_per_trade":
		return s.rtCfg.MaxGasUSD
	case "daily_budget_usd":
		return s.rtCfg.DailyBudgetUSD
	case "twap_slice_count":
		return float64(s.rtCfg.TWAPSliceCount)
	case "twap_window_sec":
		return float64(s.rtCfg.TWAPWindowSec)
	case "min_net_edge_usd":
		return s.rtCfg.MinNetEdgeUSD
	case "max_gas_gwei":
		return float64(s.rtCfg.GasCapGwei)
	default:
		return 0
	}
}

func boolToFloat(b bool) float64 {
	if b {
		return 1.0
	}
	return 0.0
}
