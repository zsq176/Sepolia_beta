package uniswap

import (
	"context"
	"fmt"
	"math/big"
	"time"
	"market-maker-service/internal/domain"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
)

// V3Reader reads slot0() from a UniswapV3Pool to compute the current mid price.
type V3Reader struct {
	client ChainReader
}

func (r *V3Reader) Read(ctx context.Context, inst *domain.Instrument) (float64, time.Time, error) {
	if (inst.Pool == common.Address{}) {
		return 0, time.Time{}, fmt.Errorf("v3 reader: instrument %q has no Pool address", inst.ID)
	}
	pool := inst.Pool
	now := time.Now()

	data, err := r.client.CallContract(ctx, ethereum.CallMsg{
		To:   &pool,
		Data: common.FromHex("0x3850c7bd"), // slot0()
	}, nil)
	if err != nil {
		return 0, now, err
	}
	out, err := v3PoolABI.Unpack("slot0", data)
	if err != nil {
		return 0, now, err
	}
	sqrtPriceX96 := out[0].(*big.Int)
	if sqrtPriceX96.Sign() == 0 {
		return 0, now, nil
	}

	t0, t1 := sortTokens(inst.Base.Address, inst.Quote.Address)
	dec0, dec1 := decimalsFor(inst, t0), decimalsFor(inst, t1)
	raw := sqrtPriceToFloat(sqrtPriceX96, dec0, dec1)
	return applyQuoteDirection(raw, t0, t1, dec0, dec1, inst.Base.Address), now, nil
}
