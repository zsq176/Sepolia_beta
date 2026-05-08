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

// V2Reader reads getReserves() from a UniswapV2Pair and turns it into a price.
type V2Reader struct {
	client ChainReader
}

func (r *V2Reader) Read(ctx context.Context, inst *domain.Instrument) (float64, time.Time, error) {
	if (inst.Pair == common.Address{}) {
		return 0, time.Time{}, fmt.Errorf("v2 reader: instrument %q has no Pair address", inst.ID)
	}
	pair := inst.Pair
	now := time.Now()

	data, err := r.client.CallContract(ctx, ethereum.CallMsg{
		To:   &pair,
		Data: common.FromHex("0x0902f1ac"), // getReserves()
	}, nil)
	if err != nil {
		return 0, now, err
	}
	results, err := v2PairABI.Unpack("getReserves", data)
	if err != nil {
		return 0, now, err
	}
	r0 := results[0].(*big.Int)
	r1 := results[1].(*big.Int)
	if r0.Sign() == 0 {
		return 0, now, nil
	}

	t0, t1 := sortTokens(inst.Base.Address, inst.Quote.Address)
	dec0, dec1 := decimalsFor(inst, t0), decimalsFor(inst, t1)

	raw := new(big.Float).Quo(
		new(big.Float).SetInt(r1),
		new(big.Float).SetInt(r0),
	)
	raw = new(big.Float).Mul(raw, new(big.Float).SetFloat64(pow10(dec0-dec1)))
	rawF, _ := raw.Float64()

	price := applyQuoteDirection(rawF, t0, t1, dec0, dec1, inst.Base.Address)
	return price, now, nil
}

// decimalsFor returns the decimals of either base or quote depending on which
// address the caller asks about. Keeping this lookup off-chain saves an RPC.
func decimalsFor(inst *domain.Instrument, addr common.Address) int {
	if addr == inst.Base.Address {
		return inst.Base.Decimals
	}
	if addr == inst.Quote.Address {
		return inst.Quote.Decimals
	}
	return 18
}
