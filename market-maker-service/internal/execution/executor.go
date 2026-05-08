// Package execution sends Decisions to the chain.
//
// Two executors are provided:
//   - V3Executor: Uniswap V3 SwapRouter02.exactInputSingle
//   - V4Executor: Uniswap V4 periphery PoolSwapTest.swap (the canonical V4
//     swap entry point on Sepolia, deployed by Uniswap as part of v4-periphery)
//
// Both implement the Executor interface so the Service can route a Decision to
// the right venue without knowing the on-chain details.
package execution

import (
	"context"
	"market-maker-service/internal/domain"
)

// Executor is implemented by every venue-specific swap dispatcher.
type Executor interface {
	// Execute synchronously builds, signs, sends and waits for a swap tx for
	// the given Decision. The returned ExecutionResult fully describes the
	// outcome (status, txHash, gas, error reason).
	Execute(ctx context.Context, inst *domain.Instrument, d *domain.Decision) domain.ExecutionResult
}
