package execution

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"
	"market-maker-service/internal/domain"

	ethereum "github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethclient"
)

// V4Executor sends Decisions through the canonical Uniswap V4 periphery
// PoolSwapTest contract.
//
// PoolSwapTest is the official "thin" swap entry point shipped with
// v4-periphery: it `unlock()`s the PoolManager, performs a single swap, and
// settles tokens via transferFrom on the caller. We use it instead of a custom
// helper so the bot stays on the well-audited Uniswap path.
//
// Contract reference (v4-periphery/src/test/PoolSwapTest.sol):
//
//	function swap(
//	    PoolKey memory key,
//	    SwapParams memory params,
//	    TestSettings memory testSettings,
//	    bytes memory hookData
//	) external payable returns (BalanceDelta delta);
//
//	struct TestSettings { bool takeClaims; bool settleUsingBurn; }
//
// The deployed Sepolia address is set in config (V4_SWAP_ROUTER) — defaulting
// to 0x9b6b46e2c869aa39918db7f52f5557fe577b6eee (Uniswap deployment).
type V4Executor struct {
	client       *ethclient.Client
	swapRouter   common.Address
	poolManager  common.Address
	confirmAfter time.Duration
	nonces       *Nonces
}

// NewV4Executor wires a V4 swap dispatcher.
func NewV4Executor(client *ethclient.Client, swapRouter common.Address, poolManager common.Address, nonces *Nonces) *V4Executor {
	return &V4Executor{
		client:       client,
		swapRouter:   swapRouter,
		poolManager:  poolManager,
		confirmAfter: 90 * time.Second,
		nonces:       nonces,
	}
}

// V4 PoolSwapTest ABI: swap(PoolKey, SwapParams, TestSettings, bytes hookData).
var v4PoolSwapTestABI, _ = abi.JSON(strings.NewReader(`[{
  "inputs":[
    {"components":[
      {"internalType":"address","name":"currency0","type":"address"},
      {"internalType":"address","name":"currency1","type":"address"},
      {"internalType":"uint24","name":"fee","type":"uint24"},
      {"internalType":"int24","name":"tickSpacing","type":"int24"},
      {"internalType":"address","name":"hooks","type":"address"}
    ],"internalType":"struct PoolKey","name":"key","type":"tuple"},
    {"components":[
      {"internalType":"bool","name":"zeroForOne","type":"bool"},
      {"internalType":"int256","name":"amountSpecified","type":"int256"},
      {"internalType":"uint160","name":"sqrtPriceLimitX96","type":"uint160"}
    ],"internalType":"struct SwapParams","name":"params","type":"tuple"},
    {"components":[
      {"internalType":"bool","name":"takeClaims","type":"bool"},
      {"internalType":"bool","name":"settleUsingBurn","type":"bool"}
    ],"internalType":"struct PoolSwapTest.TestSettings","name":"testSettings","type":"tuple"},
    {"internalType":"bytes","name":"hookData","type":"bytes"}
  ],
  "name":"swap",
  "outputs":[{"internalType":"int256","name":"delta","type":"int256"}],
  "stateMutability":"payable",
  "type":"function"
}]`))

// V4 sqrt price limits (these are tick bounds for V4, identical to V3).
var (
	minSqrtRatio = mustBigInt("4295128739")
	maxSqrtRatio = mustBigInt("1461446703485210103287273052203988822378723970342")
	maxApproveAmount = new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 256), big.NewInt(1))
)

func mustBigInt(s string) *big.Int {
	v, _ := new(big.Int).SetString(s, 10)
	return v
}

// Execute runs the approve + swap pair against PoolSwapTest.
//
// Direction:
//
//	SELL = sell base for quote: tokenIn = base, tokenOut = quote.
//	BUY  = sell quote for base: tokenIn = quote, tokenOut = base.
//
// `zeroForOne` is computed from the V4 sorted PoolKey (currency0 < currency1):
// it is true when tokenIn equals currency0.
func (e *V4Executor) Execute(ctx context.Context, inst *domain.Instrument, d *domain.Decision) domain.ExecutionResult {
	r := domain.ExecutionResult{
		InstrumentID: inst.ID,
		Decision:     *d,
		Status:       domain.ExecPending,
		SubmittedAt:  time.Now(),
	}

	tokenIn, _ := tokensForAction(inst, d.Action)
	amountIn, _ := new(big.Int).SetString(d.AmountIn, 10)
	minOut, _ := new(big.Int).SetString(d.MinAmountOut, 10)
	if amountIn == nil || amountIn.Sign() <= 0 {
		r.Status = domain.ExecFailed
		r.Reason = "invalid amount"
		return r
	}
	if minOut == nil || minOut.Sign() <= 0 {
		r.Status = domain.ExecFailed
		r.Reason = "missing minAmountOut protection"
		return r
	}
	// Conservative pre-trade protection for V4 path:
	// estimate output using current pool price and reject when it is below minOut.
	quotedOut := estimateOutByPrice(inst, d, amountIn)
	if quotedOut.Sign() <= 0 || quotedOut.Cmp(minOut) < 0 {
		r.Status = domain.ExecFailed
		r.Reason = fmt.Sprintf("v4 precheck quote %s < minOut %s", quotedOut.String(), minOut.String())
		return r
	}

	zeroForOne := tokenIn == inst.V4Key.Currency0
	sqrtLimit := maxSqrtRatio
	if zeroForOne {
		sqrtLimit = new(big.Int).Add(minSqrtRatio, big.NewInt(1))
	} else {
		sqrtLimit = new(big.Int).Sub(maxSqrtRatio, big.NewInt(1))
	}

	// Ensure swap router has enough allowance for token pull.
	if rec, err := e.approveAndWait(ctx, tokenIn, e.swapRouter, amountIn); err != nil || rec == nil || rec.Status == 0 {
		r.Status = domain.ExecFailed
		r.Reason = fmt.Sprintf("approve failed: %v", err)
		return r
	}

	// 2. PoolSwapTest.swap(key, params, settings, hookData)
	key := struct {
		Currency0   common.Address
		Currency1   common.Address
		Fee         *big.Int
		TickSpacing *big.Int
		Hooks       common.Address
	}{
		Currency0:   inst.V4Key.Currency0,
		Currency1:   inst.V4Key.Currency1,
		Fee:         big.NewInt(int64(inst.V4Key.Fee)),
		TickSpacing: big.NewInt(int64(inst.V4Key.TickSpacing)),
		Hooks:       inst.V4Key.Hooks,
	}
	params := struct {
		ZeroForOne        bool
		AmountSpecified   *big.Int
		SqrtPriceLimitX96 *big.Int
	}{
		ZeroForOne:        zeroForOne,
		// PoolSwapTest exact-input path requires amountSpecified < 0.
		AmountSpecified:   new(big.Int).Neg(new(big.Int).Set(amountIn)),
		SqrtPriceLimitX96: sqrtLimit,
	}
	settings := struct {
		TakeClaims      bool
		SettleUsingBurn bool
	}{
		TakeClaims:      false, // we want raw ERC20s, not 6909 claims
		SettleUsingBurn: false, // settle via transferFrom
	}

	data, err := v4PoolSwapTestABI.Pack("swap", key, params, settings, []byte{})
	if err != nil {
		r.Status = domain.ExecFailed
		r.Reason = "pack swap: " + err.Error()
		return r
	}
	if err := e.preflightSwap(ctx, data); err != nil {
		r.Status = domain.ExecFailed
		r.Reason = "preflight swap: " + err.Error()
		return r
	}
	tx, err := e.nonces.SignAndSend(ctx, e.swapRouter, data, big.NewInt(0), 600_000)
	if err != nil {
		r.Status = domain.ExecFailed
		r.Reason = "send swap: " + err.Error()
		return r
	}
	r.TxHash = tx.Hash().Hex()
	r.Status = domain.ExecSubmitted
	r.GasPriceWei = tx.GasFeeCap().String()
	return r
}

func (e *V4Executor) preflightSwap(ctx context.Context, data []byte) error {
	msg := ethereum.CallMsg{
		From: e.nonces.Address(),
		To:   &e.swapRouter,
		Data: data,
	}
	callCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	_, err := e.nonces.CallContractWithFailover(callCtx, msg)
	if err == nil {
		return nil
	}
	return humanizeCallErr(err)
}

func humanizeCallErr(err error) error {
	if err == nil {
		return nil
	}
	type dataErr interface{ ErrorData() interface{} }
	if de, ok := err.(dataErr); ok {
		if decoded := decodeRevertData(de.ErrorData()); decoded != "" {
			return errors.New(decoded)
		}
		if raw := rawRevertData(de.ErrorData()); raw != "" {
			return errors.New("execution reverted (data=" + raw + ")")
		}
	}
	msg := strings.TrimSpace(err.Error())
	if msg == "" {
		return errors.New("unknown call failure")
	}
	// geth style: "execution reverted: <reason>"
	if idx := strings.Index(msg, "execution reverted:"); idx >= 0 {
		return errors.New(strings.TrimSpace(msg[idx+len("execution reverted:"):]))
	}
	// fallback: keep the raw reason so logs are actionable.
	return errors.New(msg)
}

func decodeRevertData(v interface{}) string {
	raw, ok := v.(string)
	if !ok {
		return ""
	}
	raw = strings.TrimPrefix(raw, "0x")
	if raw == "" {
		return ""
	}
	b, err := hex.DecodeString(raw)
	if err != nil || len(b) < 4 {
		return ""
	}
	if reason, err := abi.UnpackRevert(b); err == nil && reason != "" {
		return "execution reverted: " + reason
	}
	return ""
}

func rawRevertData(v interface{}) string {
	s, ok := v.(string)
	if !ok {
		return ""
	}
	if s == "" {
		return ""
	}
	if strings.HasPrefix(s, "0x") {
		return s
	}
	return "0x" + s
}

func estimateOutByPrice(inst *domain.Instrument, d *domain.Decision, amountIn *big.Int) *big.Int {
	if d.PoolPrice <= 0 {
		return big.NewInt(0)
	}
	inF := new(big.Float).SetInt(amountIn)
	var out *big.Float
	switch d.Action {
	case domain.ActionSell:
		// base -> quote
		base := new(big.Float).Quo(inF, new(big.Float).SetFloat64(pow10(inst.Base.Decimals)))
		quote := new(big.Float).Mul(base, new(big.Float).SetFloat64(d.PoolPrice))
		out = new(big.Float).Mul(quote, new(big.Float).SetFloat64(pow10(inst.Quote.Decimals)))
	case domain.ActionBuy:
		// quote -> base
		quote := new(big.Float).Quo(inF, new(big.Float).SetFloat64(pow10(inst.Quote.Decimals)))
		base := new(big.Float).Quo(quote, new(big.Float).SetFloat64(d.PoolPrice))
		out = new(big.Float).Mul(base, new(big.Float).SetFloat64(pow10(inst.Base.Decimals)))
	default:
		return big.NewInt(0)
	}
	v, _ := out.Int(nil)
	if v == nil {
		return big.NewInt(0)
	}
	return v
}

func pow10(n int) float64 {
	r := 1.0
	for i := 0; i < n; i++ {
		r *= 10
	}
	return r
}

func (e *V4Executor) approveAndWait(ctx context.Context, token, spender common.Address, amount *big.Int) (*types.Receipt, error) {
	if ok, err := hasSufficientAllowance(ctx, e.nonces, token, e.nonces.Address(), spender, amount); err == nil && ok {
		return &types.Receipt{Status: 1}, nil
	}
	// Use max approval on V4 to avoid per-slice allowance races under async confirms.
	data, err := erc20ApproveABI.Pack("approve", spender, maxApproveAmount)
	if err != nil {
		return nil, err
	}
	tx, err := e.nonces.SignAndSend(ctx, token, data, big.NewInt(0), 90_000)
	if err != nil {
		return nil, err
	}
	return waitReceipt(ctx, e.nonces, tx.Hash(), e.confirmAfter)
}
