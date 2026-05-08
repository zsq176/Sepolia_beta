package domain

import "time"

// ActionKind tells the executor what to do.
type ActionKind string

const (
	ActionHold ActionKind = "hold"
	ActionBuy  ActionKind = "buy"  // buy base with quote (push pool price up)
	ActionSell ActionKind = "sell" // sell base for quote (push pool price down)
)

// Decision is the strategy engine's verdict for a single instrument tick.
type Decision struct {
	InstrumentID string
	Action       ActionKind
	// AmountIn is denominated in the input token (raw decimals on-chain).
	AmountIn      string  // big.Int as decimal string
	AmountInHuman float64 // human-readable amount for logs/UI
	// MinAmountOut is amountOutMin honoring slippage tolerance.
	MinAmountOut string

	TargetPrice  float64
	PoolPrice    float64
	Deviation    float64
	NotionalUSD  float64
	SlippageBps  int
	Reason       string
	GeneratedAt  time.Time
}

// ExecutionStatus enumerates the lifecycle of an execution attempt.
type ExecutionStatus string

const (
	ExecPending   ExecutionStatus = "pending"
	ExecSubmitted ExecutionStatus = "submitted"
	ExecConfirmed ExecutionStatus = "confirmed"
	ExecFailed    ExecutionStatus = "failed"
	ExecSkipped   ExecutionStatus = "skipped"
)

// ExecutionResult captures the outcome of a Decision sent to an executor.
type ExecutionResult struct {
	InstrumentID string
	Decision     Decision
	Status       ExecutionStatus
	TxHash       string
	GasUsed      uint64
	GasPriceWei  string
	BlockNumber  uint64
	Reason       string
	SubmittedAt  time.Time
	ConfirmedAt  time.Time
}
