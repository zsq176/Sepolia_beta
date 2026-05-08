package uniswap

import (
	"context"
	"fmt"
	"math/big"
	"time"
	"market-maker-service/internal/domain"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
)

// V4Reader reads the packed slot0 of a V4 pool directly out of the
// PoolManager's `_pools` mapping (storage slot 6) without needing the
// (non-existent on Sepolia) StateView wrapper.
type V4Reader struct {
	client      *ethclient.Client
	poolManager common.Address
}

func (r *V4Reader) Read(ctx context.Context, inst *domain.Instrument) (float64, time.Time, error) {
	now := time.Now()
	if (r.poolManager == common.Address{}) {
		return 0, now, fmt.Errorf("v4 reader: pool manager not configured")
	}
	if (inst.V4Key.Currency0 == common.Address{}) {
		return 0, now, fmt.Errorf("v4 reader: instrument %q missing V4Key", inst.ID)
	}

	poolID := computeV4PoolID(inst.V4Key)

	// Pool.State for poolId is at keccak256(abi.encodePacked(poolId, uint256(6))).
	preimage := make([]byte, 64)
	copy(preimage[0:32], poolID.Bytes())
	preimage[63] = 6
	stateSlot := crypto.Keccak256Hash(preimage)

	data, err := r.client.StorageAt(ctx, r.poolManager, stateSlot, nil)
	if err != nil {
		return 0, now, err
	}
	if len(data) < 32 {
		return 0, now, nil
	}

	// Slot0 packed: [uint160 sqrtPriceX96][int24 tick][uint24 proto][uint24 lp][uint24 swap].
	// In PoolManager storage layout, sqrtPriceX96 is read from the rightmost
	// 20 bytes in the 32-byte slot (data[12:32]).
	sqrtPriceBytes := make([]byte, 32)
	copy(sqrtPriceBytes[12:], data[12:32])
	sqrtPriceX96 := new(big.Int).SetBytes(sqrtPriceBytes)
	if sqrtPriceX96.Sign() == 0 {
		return 0, now, nil
	}

	t0, t1 := inst.V4Key.Currency0, inst.V4Key.Currency1
	dec0, dec1 := decimalsFor(inst, t0), decimalsFor(inst, t1)
	raw := sqrtPriceToFloat(sqrtPriceX96, dec0, dec1)
	return applyQuoteDirection(raw, t0, t1, dec0, dec1, inst.Base.Address), now, nil
}

// computeV4PoolID is the keccak256 of the packed PoolKey, matching the
// Uniswap V4 PoolId.toId() convention exactly.
func computeV4PoolID(key domain.V4PoolKey) common.Hash {
	args := abi.Arguments{
		{Type: abi.Type{T: abi.AddressTy}},
		{Type: abi.Type{T: abi.AddressTy}},
		{Type: abi.Type{T: abi.UintTy, Size: 24}},
		{Type: abi.Type{T: abi.IntTy, Size: 24}},
		{Type: abi.Type{T: abi.AddressTy}},
	}
	packed, _ := args.Pack(
		key.Currency0,
		key.Currency1,
		big.NewInt(int64(key.Fee)),
		big.NewInt(int64(key.TickSpacing)),
		key.Hooks,
	)
	return crypto.Keccak256Hash(packed)
}
