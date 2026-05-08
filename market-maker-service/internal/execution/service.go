package execution

import (
	"context"
	"crypto/ecdsa"
	"fmt"
	"log"
	"math/big"
	"strconv"
	"strings"
	"sync"
	"time"
	"market-maker-service/internal/config"
	"market-maker-service/internal/db"
	"market-maker-service/internal/domain"
	"market-maker-service/internal/risk"
	"market-maker-service/internal/strategy"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/redis/go-redis/v9"
)

// Service orchestrates Strategy → RiskGuard → Executor for every Decision.
//
// One Service is shared across all instruments. Per-instrument concurrency is
// limited to one in-flight execution at a time (the running map) so a slow
// confirmation on one pool does not stall others.
type Service struct {
	strat    *strategy.Engine
	guard    *risk.Guard
	store    *db.Store
	client   *ethclient.Client
	executors map[domain.Venue]Executor

	mu      sync.Mutex
	running map[string]bool

	// resultSink, when non-nil, is invoked synchronously after every result so
	// the API layer / tests can inspect outcomes without polling the database.
	resultSink func(*domain.ExecutionResult)
	cfg        *config.Config
	rdb        *redis.Client
	redisQueue string
	redisLockTTL time.Duration
	auditCancel chan struct{}
}

// NewService constructs a Service. pk is the operator key the executors will
// sign with; chainID is required for EIP-155 signing.
func NewService(
	client *ethclient.Client,
	broadcasters []*ethclient.Client,
	pk *ecdsa.PrivateKey,
	chainID *big.Int,
	v3Router common.Address,
	v4SwapRouter common.Address,
	v4PoolManager common.Address,
	strat *strategy.Engine,
	guard *risk.Guard,
	store *db.Store,
	cfg *config.Config,
) *Service {
	nonces := NewNonces(client, broadcasters, pk, chainID)
	svc := &Service{
		strat: strat,
		guard: guard,
		store: store,
		client: client,
		executors: map[domain.Venue]Executor{
			domain.VenueV3: NewV3Executor(client, v3Router, nonces),
			domain.VenueV4: NewV4Executor(client, v4SwapRouter, v4PoolManager, nonces),
		},
		running: map[string]bool{},
		cfg:     cfg,
	}
	svc.initRedis()
	return svc
}

// SetResultSink installs an optional callback fired for every ExecutionResult.
func (s *Service) SetResultSink(f func(*domain.ExecutionResult)) {
	s.resultSink = f
}

// Address returns the operator's from-address.
func (s *Service) Address() common.Address {
	for _, e := range s.executors {
		switch v := e.(type) {
		case *V3Executor:
			return v.nonces.Address()
		case *V4Executor:
			return v.nonces.Address()
		}
	}
	return common.Address{}
}

// OnSnapshot is the entry point called by the market.Aggregator on every tick.
// It computes Decisions, gates them through the risk guard, and dispatches the
// surviving ones to the appropriate Executor (asynchronously per instrument).
func (s *Service) OnSnapshot(snap *domain.MarketSnapshot) {
	for _, d := range s.strat.Decide(snap) {
		d := d
		// One in-flight tx per instrument keeps nonce ordering simple and
		// prevents the strategy from "doubling up" on a still-converging pool.
		if !s.tryAcquire(d.InstrumentID) {
			continue
		}
		go func() {
			defer s.release(d.InstrumentID)
			s.execute(snap, &d)
		}()
	}
}

func (s *Service) execute(snap *domain.MarketSnapshot, d *domain.Decision) {
	is := snap.Instruments[d.InstrumentID]
	if is == nil || is.Instrument == nil {
		return
	}
	inst := is.Instrument

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	// Risk gate.
	if v := s.guard.Allow(ctx, d); !v.Allow {
		log.Printf("[exec] %s: skipped by risk: %s", d.InstrumentID, v.Reason)
		s.persistDecision(is, d, false, v.Reason)
		s.recordSkip(d, v.Reason)
		return
	}
	s.persistDecision(is, d, true, "allowed")

	// Dispatch.
	exec, ok := s.executors[inst.Venue]
	if !ok {
		log.Printf("[exec] %s: no executor for venue %q", d.InstrumentID, inst.Venue)
		return
	}

	log.Printf("[exec] %s: %s notional=$%.0f dev=%.4f%% target=%.6f pool=%.6f",
		d.InstrumentID, d.Action, d.NotionalUSD, d.Deviation*100, d.TargetPrice, d.PoolPrice)

	res := s.executeTWAP(ctx, exec, inst, d)

	switch res.Status {
	case domain.ExecConfirmed:
		s.guard.RecordSuccess(d.InstrumentID)
		s.strat.MarkExecuted(d.InstrumentID, time.Now())
		log.Printf("[exec] %s: confirmed tx=%s gas=%d block=%d", d.InstrumentID, res.TxHash, res.GasUsed, res.BlockNumber)
	case domain.ExecFailed:
		s.guard.RecordFailure(d.InstrumentID, res.Reason)
		log.Printf("[exec] %s: failed: %s", d.InstrumentID, res.Reason)
	case domain.ExecSubmitted:
		// Mark cooldown as soon as tx is accepted by mempool; final status is handled asynchronously.
		s.strat.MarkExecuted(d.InstrumentID, time.Now())
		log.Printf("[exec] %s: submitted tx=%s (confirm async)", d.InstrumentID, res.TxHash)
		go s.awaitConfirmation(d.InstrumentID, &res)
	}

	s.persist(&res)
	if s.resultSink != nil {
		s.resultSink(&res)
	}
}

func (s *Service) awaitConfirmation(instrumentID string, submitted *domain.ExecutionResult) {
	if s.client == nil || submitted == nil || submitted.TxHash == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	rec, err := waitReceipt(ctx, s.client, common.HexToHash(submitted.TxHash), 3*time.Minute)
	if err != nil {
		log.Printf("[exec] %s: async confirm timeout tx=%s err=%v", instrumentID, submitted.TxHash, err)
		return
	}
	status := domain.ExecConfirmed
	reason := ""
	if rec.Status == 0 {
		status = domain.ExecFailed
		reason = "swap reverted"
		s.guard.RecordFailure(instrumentID, reason)
	} else {
		s.guard.RecordSuccess(instrumentID)
	}

	_ = s.store.UpdateTradeStatus(submitted.TxHash, string(status))
	if err := s.store.InsertExecutionLog(&db.ExecutionLog{
		Timestamp:    time.Now().Unix(),
		InstrumentID: submitted.InstrumentID,
		Wallet:       strings.ToLower(s.Address().Hex()),
		QualityLevel: qualityFromDecision(submitted.Decision),
		Action:       string(submitted.Decision.Action),
		AmountIn:     submitted.Decision.AmountIn,
		MinAmountOut: submitted.Decision.MinAmountOut,
		TxHash:       submitted.TxHash,
		Status:       string(status),
		Reason:       reason,
		GasUsed:      int64(rec.GasUsed),
	}); err != nil {
		log.Printf("[exec] async execution log error: %v", err)
	}
	log.Printf("[exec] %s: async confirm tx=%s status=%s gas=%d block=%d", instrumentID, submitted.TxHash, status, rec.GasUsed, rec.BlockNumber.Uint64())
}

func (s *Service) executeTWAP(ctx context.Context, exec Executor, inst *domain.Instrument, d *domain.Decision) domain.ExecutionResult {
	slices := int64(1)
	if s.cfg != nil && s.cfg.TWAPSliceCount > 1 {
		slices = int64(s.cfg.GetRuntimeFloat("twap_slice_count", float64(s.cfg.TWAPSliceCount)))
	}
	if slices <= 1 {
		return exec.Execute(ctx, inst, d)
	}
	amountIn, ok := new(big.Int).SetString(d.AmountIn, 10)
	if !ok || amountIn.Sign() <= 0 {
		return exec.Execute(ctx, inst, d)
	}
	minOut, _ := new(big.Int).SetString(d.MinAmountOut, 10)
	last := domain.ExecutionResult{
		InstrumentID: d.InstrumentID,
		Decision:     *d,
		Status:       domain.ExecSkipped,
		Reason:       "twap-no-slice",
		SubmittedAt:  time.Now(),
	}
	remainingIn := new(big.Int).Set(amountIn)
	remainingOut := new(big.Int)
	if minOut != nil {
		remainingOut.Set(minOut)
	}
	for i := int64(0); i < slices; i++ {
		partIn := new(big.Int).Div(remainingIn, big.NewInt(slices-i))
		partOut := big.NewInt(0)
		if remainingOut.Sign() > 0 {
			partOut = new(big.Int).Div(remainingOut, big.NewInt(slices-i))
		}
		dd := *d
		dd.AmountIn = partIn.String()
		dd.MinAmountOut = partOut.String()
		last = exec.Execute(ctx, inst, &dd)
		if last.Status == domain.ExecFailed {
			return last
		}
		remainingIn.Sub(remainingIn, partIn)
		if remainingOut.Sign() > 0 {
			remainingOut.Sub(remainingOut, partOut)
		}
		if i < slices-1 {
			select {
			case <-ctx.Done():
				last.Status = domain.ExecFailed
				last.Reason = "twap-cancelled"
				return last
			case <-time.After(1500 * time.Millisecond):
			}
		}
	}
	return last
}

func (s *Service) recordSkip(d *domain.Decision, reason string) {
	res := &domain.ExecutionResult{
		InstrumentID: d.InstrumentID,
		Decision:     *d,
		Status:       domain.ExecSkipped,
		Reason:       reason,
		SubmittedAt:  time.Now(),
	}
	s.persist(res)
	if s.resultSink != nil {
		s.resultSink(res)
	}
}

func (s *Service) persist(r *domain.ExecutionResult) {
	s.enqueueResult(r)
}

func (s *Service) persistSync(r *domain.ExecutionResult) {
	if s.store == nil {
		return
	}
	action := "BUY"
	if r.Decision.Action == domain.ActionSell {
		action = "SELL"
	}
	t := &db.Trade{
		Timestamp: r.SubmittedAt.Unix(),
		Wallet:    strings.ToLower(s.Address().Hex()),
		Pair:      r.InstrumentID,
		Action:    fmt.Sprintf("ARB_%s", action),
		Amount:    r.Decision.AmountIn,
		Price:     r.Decision.TargetPrice,
		TxHash:    r.TxHash,
		Status:    string(r.Status),
		Source:    "system",
	}
	if err := s.store.InsertTrade(t); err != nil {
		log.Printf("[exec] persist trade error: %v", err)
	}
	if err := s.store.InsertExecutionLog(&db.ExecutionLog{
		Timestamp:    r.SubmittedAt.Unix(),
		InstrumentID: r.InstrumentID,
		Wallet:       strings.ToLower(s.Address().Hex()),
		QualityLevel: qualityFromDecision(r.Decision),
		Action:       string(r.Decision.Action),
		AmountIn:     r.Decision.AmountIn,
		MinAmountOut: r.Decision.MinAmountOut,
		TxHash:       r.TxHash,
		Status:       string(r.Status),
		Reason:       r.Reason,
		GasUsed:      int64(r.GasUsed),
	}); err != nil {
		log.Printf("[exec] persist execution log error: %v", err)
	}
	s.updateAccountPositions(r)
}

func (s *Service) persistDecision(is *domain.InstrumentSnapshot, d *domain.Decision, allowed bool, reason string) {
	if is == nil || is.Instrument == nil {
		return
	}
	state := "Normal"
	if !is.PoolFresh || !is.TargetFresh {
		state = "Degraded"
	}
	s.enqueueDecision(&decisionPersistRequest{
		Timestamp:    time.Now().Unix(),
		InstrumentID: d.InstrumentID,
		QualityLevel: qualityFromSnapshot(is),
		PoolPrice:    d.PoolPrice,
		TargetPrice:  d.TargetPrice,
		Deviation:    d.Deviation,
		NotionalUSD:  d.NotionalUSD,
		State:        state,
		Allowed:      allowed,
		Reason:       reason,
	})
}

func (s *Service) persistDecisionSync(req *decisionPersistRequest) {
	if s.store == nil || req == nil {
		return
	}
	if err := s.store.InsertDecisionLog(&db.DecisionLog{
		Timestamp:    req.Timestamp,
		InstrumentID: req.InstrumentID,
		QualityLevel: req.QualityLevel,
		PoolPrice:    req.PoolPrice,
		TargetPrice:  req.TargetPrice,
		Deviation:    req.Deviation,
		NotionalUSD:  req.NotionalUSD,
		State:        req.State,
		Allowed:      req.Allowed,
		Reason:       req.Reason,
	}); err != nil {
		log.Printf("[exec] persist decision log error: %v", err)
	}
}

func qualityFromSnapshot(is *domain.InstrumentSnapshot) string {
	if !is.PoolFresh || !is.TargetFresh {
		return "stale"
	}
	if is.Pool.Mid <= 0 || is.Target.Mid <= 0 {
		return "invalid"
	}
	if is.Pool.Stale || is.Target.Stale {
		return "fallback"
	}
	return "live"
}

func qualityFromDecision(d domain.Decision) string {
	if d.PoolPrice <= 0 || d.TargetPrice <= 0 {
		return "invalid"
	}
	return "live"
}

func (s *Service) updateAccountPositions(r *domain.ExecutionResult) {
	if s.store == nil || (r.Status != domain.ExecConfirmed && r.Status != domain.ExecSubmitted) {
		return
	}
	wallet := strings.ToLower(s.Address().Hex())
	amountFloat, err := strconv.ParseFloat(r.Decision.AmountIn, 64)
	if err != nil || amountFloat <= 0 {
		return
	}
	// Minimal inventory approximation for dashboard isolation:
	// BUY => quote decreases, base increases. SELL => inverse.
	if r.Decision.Action == domain.ActionBuy {
		_ = s.bumpPosition(wallet, tokenFromInstrument(r.InstrumentID, true), amountFloat/r.Decision.PoolPrice, r.Decision.NotionalUSD)
		_ = s.bumpPosition(wallet, tokenFromInstrument(r.InstrumentID, false), -amountFloat, -r.Decision.NotionalUSD)
	} else if r.Decision.Action == domain.ActionSell {
		_ = s.bumpPosition(wallet, tokenFromInstrument(r.InstrumentID, true), -amountFloat, -r.Decision.NotionalUSD)
		_ = s.bumpPosition(wallet, tokenFromInstrument(r.InstrumentID, false), amountFloat*r.Decision.PoolPrice, r.Decision.NotionalUSD)
	}
}

func (s *Service) bumpPosition(wallet, token string, delta float64, deltaValue float64) error {
	pos, err := s.store.GetPositionsByWallet(wallet)
	if err != nil {
		return err
	}
	current := 0.0
	value := 0.0
	for _, p := range pos {
		if p.Token == token {
			current, _ = strconv.ParseFloat(p.Amount, 64)
			value = p.Value
			break
		}
	}
	current += delta
	value += deltaValue
	return s.store.UpsertAccountPosition(wallet, token, fmt.Sprintf("%.10f", current), value)
}

func tokenFromInstrument(instrumentID string, base bool) string {
	switch instrumentID {
	case "v3:BTCUSDT", "v4:BTCUSDT":
		if base {
			return "BTC-Beta"
		}
		return "USDT-Beta"
	case "v4:ETHUSDT":
		if base {
			return "ETH-Beta"
		}
		return "USDT-Beta"
	case "v4:ETHBTC":
		if base {
			return "ETH-Beta"
		}
		return "BTC-Beta"
	default:
		if base {
			return "BASE"
		}
		return "QUOTE"
	}
}

func (s *Service) tryAcquire(id string) bool {
	if s.rdb != nil {
		ok, err := s.tryAcquireRedisLock(id)
		if err == nil {
			return ok
		}
		log.Printf("[exec] redis lock degraded, fallback local lock: %v", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running[id] {
		return false
	}
	s.running[id] = true
	return true
}

func (s *Service) release(id string) {
	s.releaseRedisLock(id)
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.running, id)
}

func (s *Service) Close() {
	s.closeRedis()
}

// PrivateKeyFromHex is a convenience exposed so cmd/main can construct the
// service without importing crypto directly.
func PrivateKeyFromHex(hex string) (*ecdsa.PrivateKey, error) {
	return crypto.HexToECDSA(hex)
}
