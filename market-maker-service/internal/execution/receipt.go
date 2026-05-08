package execution

import (
	"context"
	"errors"
	"math/big"
	"strings"
	"time"

	ethereum "github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

// erc20ApproveABI is shared by both executors.
var erc20ApproveABI, _ = abi.JSON(strings.NewReader(
	`[{"constant":false,"inputs":[{"name":"spender","type":"address"},{"name":"amount","type":"uint256"}],"name":"approve","outputs":[{"name":"","type":"bool"}],"type":"function"}]`,
))

var erc20AllowanceABI, _ = abi.JSON(strings.NewReader(
	`[{"constant":true,"inputs":[{"name":"owner","type":"address"},{"name":"spender","type":"address"}],"name":"allowance","outputs":[{"name":"","type":"uint256"}],"type":"function"}]`,
))

// waitReceipt polls TransactionReceipt until either we get one or `timeout`
// has passed. Returning a typed *types.Receipt lets callers inspect Status,
// GasUsed and BlockNumber uniformly.
func waitReceipt(ctx context.Context, client interface {
	TransactionReceipt(ctx context.Context, txHash common.Hash) (*types.Receipt, error)
}, hash common.Hash, timeout time.Duration) (*types.Receipt, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
		rec, err := client.TransactionReceipt(ctx, hash)
		if err == nil && rec != nil {
			return rec, nil
		}
		time.Sleep(2 * time.Second)
	}
	return nil, errors.New("receipt timeout")
}

func hasSufficientAllowance(
	ctx context.Context,
	caller interface {
		CallContract(ctx context.Context, call ethereum.CallMsg, blockNumber *big.Int) ([]byte, error)
	},
	token common.Address,
	owner common.Address,
	spender common.Address,
	required *big.Int,
) (bool, error) {
	if required == nil || required.Sign() <= 0 {
		return true, nil
	}
	data, err := erc20AllowanceABI.Pack("allowance", owner, spender)
	if err != nil {
		return false, err
	}
	out, err := caller.CallContract(ctx, ethereum.CallMsg{To: &token, Data: data}, nil)
	if err != nil {
		return false, err
	}
	vals, err := erc20AllowanceABI.Unpack("allowance", out)
	if err != nil || len(vals) != 1 {
		return false, err
	}
	allowance, ok := vals[0].(*big.Int)
	if !ok || allowance == nil {
		return false, errors.New("invalid allowance return type")
	}
	return allowance.Cmp(required) >= 0, nil
}
