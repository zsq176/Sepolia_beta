package execution

import (
	"context"
	"fmt"
	"math/big"
	"strings"
	"time"
	"market-maker-service/internal/domain"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethclient"
)

// V3Executor sends Decisions through Uniswap V3 SwapRouter02.exactInputSingle.
//
// Flow per Decision:
//
//	1. ERC20 approve(router, amountIn)  — wait for receipt
//	2. exactInputSingle(params)         — wait for receipt
//
// Approvals are kept tight (per-trade) instead of an unlimited pre-approval so
// a leaked key has a smaller blast radius. The trade-off is one extra tx per
// rebalance, which is acceptable on Sepolia.
type V3Executor struct {
	client       *ethclient.Client
	router       common.Address
	defaultFee   uint32
	confirmAfter time.Duration
	nonces       *Nonces
}

// NewV3Executor configures a V3 swap dispatcher.
func NewV3Executor(client *ethclient.Client, router common.Address, nonces *Nonces) *V3Executor {
	return &V3Executor{
		client:       client,
		router:       router,
		defaultFee:   3000,
		confirmAfter: 90 * time.Second,
		nonces:       nonces,
	}
}

// V3 SwapRouter02 ABI for ExactInputSingle.
var v3RouterABI, _ = abi.JSON(strings.NewReader(`[{"inputs":[{"components":[{"internalType":"address","name":"tokenIn","type":"address"},{"internalType":"address","name":"tokenOut","type":"address"},{"internalType":"uint24","name":"fee","type":"uint24"},{"internalType":"address","name":"recipient","type":"address"},{"internalType":"uint256","name":"amountIn","type":"uint256"},{"internalType":"uint256","name":"amountOutMinimum","type":"uint256"},{"internalType":"uint160","name":"sqrtPriceLimitX96","type":"uint160"}],"internalType":"struct ISwapRouter02.ExactInputSingleParams","name":"params","type":"tuple"}],"name":"exactInputSingle","outputs":[{"internalType":"uint256","name":"amountOut","type":"uint256"}],"stateMutability":"payable","type":"function"}]`))

// Execute runs the approve + exactInputSingle pair.
func (e *V3Executor) Execute(ctx context.Context, inst *domain.Instrument, d *domain.Decision) domain.ExecutionResult {
	r := domain.ExecutionResult{
		InstrumentID: inst.ID,
		Decision:     *d,
		Status:       domain.ExecPending,
		SubmittedAt:  time.Now(),
	}

	tokenIn, tokenOut := tokensForAction(inst, d.Action)
	amountIn, _ := new(big.Int).SetString(d.AmountIn, 10)
	minOut, _ := new(big.Int).SetString(d.MinAmountOut, 10)
	if amountIn == nil || minOut == nil || amountIn.Sign() <= 0 {
		r.Status = domain.ExecFailed
		r.Reason = "invalid amounts"
		return r
	}

	if rec, err := e.approveAndWait(ctx, tokenIn, e.router, amountIn); err != nil || rec == nil || rec.Status == 0 {
		r.Status = domain.ExecFailed
		r.Reason = fmt.Sprintf("approve failed: %v", err)
		return r
	}

	params := struct {
		TokenIn           common.Address
		TokenOut          common.Address
		Fee               *big.Int
		Recipient         common.Address
		AmountIn          *big.Int
		AmountOutMinimum  *big.Int
		SqrtPriceLimitX96 *big.Int
	}{
		TokenIn:           tokenIn,
		TokenOut:          tokenOut,
		Fee:               big.NewInt(int64(e.defaultFee)),
		Recipient:         e.nonces.Address(),
		AmountIn:          amountIn,
		AmountOutMinimum:  minOut,
		SqrtPriceLimitX96: big.NewInt(0),
	}
	data, err := v3RouterABI.Pack("exactInputSingle", params)
	if err != nil {
		r.Status = domain.ExecFailed
		r.Reason = "pack swap: " + err.Error()
		return r
	}
	tx, err := e.nonces.SignAndSend(ctx, e.router, data, big.NewInt(0), 350_000)
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

func (e *V3Executor) approveAndWait(ctx context.Context, token, spender common.Address, amount *big.Int) (*types.Receipt, error) {
	if ok, err := hasSufficientAllowance(ctx, e.nonces, token, e.nonces.Address(), spender, amount); err == nil && ok {
		return &types.Receipt{Status: 1}, nil
	}
	data, err := erc20ApproveABI.Pack("approve", spender, amount)
	if err != nil {
		return nil, err
	}
	tx, err := e.nonces.SignAndSend(ctx, token, data, big.NewInt(0), 90_000)
	if err != nil {
		return nil, err
	}
	return waitReceipt(ctx, e.nonces, tx.Hash(), e.confirmAfter)
}

func (e *V3Executor) fillReceipt(r *domain.ExecutionResult, tx *types.Transaction, rec *types.Receipt) {
	r.GasUsed = rec.GasUsed
	r.BlockNumber = rec.BlockNumber.Uint64()
	r.GasPriceWei = tx.GasFeeCap().String()
	r.ConfirmedAt = time.Now()
}

// tokensForAction returns (tokenIn, tokenOut) given the strategy's intended
// action: SELL means we sell base, BUY means we sell quote.
func tokensForAction(inst *domain.Instrument, a domain.ActionKind) (common.Address, common.Address) {
	if a == domain.ActionSell {
		return inst.Base.Address, inst.Quote.Address
	}
	return inst.Quote.Address, inst.Base.Address
}
