package uniswap

import (
	"math/big"

	"github.com/ethereum/go-ethereum/common"
)

// sortTokens returns the two addresses ordered as Uniswap stores them
// (currency0 < currency1 by uint160 value).
func sortTokens(a, b common.Address) (common.Address, common.Address) {
	if a.Big().Cmp(b.Big()) < 0 {
		return a, b
	}
	return b, a
}

// sqrtPriceToFloat converts a Q64.96 sqrt price into a human-readable price
// expressed as token1 per token0, accounting for token decimals.
func sqrtPriceToFloat(sqrtPriceX96 *big.Int, dec0, dec1 int) float64 {
	q96 := new(big.Float).SetInt(new(big.Int).Lsh(big.NewInt(1), 96))
	sqrtPrice := new(big.Float).Quo(new(big.Float).SetInt(sqrtPriceX96), q96)
	price := new(big.Float).Mul(sqrtPrice, sqrtPrice)
	price = new(big.Float).Mul(price, new(big.Float).SetFloat64(pow10(dec0-dec1)))
	f, _ := price.Float64()
	return f
}

// applyQuoteDirection takes a raw price expressed as token1/token0 and rotates
// it so that the result is "quote per base" for the requested base address.
func applyQuoteDirection(rawPrice float64, t0, t1 common.Address, dec0, dec1 int, baseToken common.Address) float64 {
	if rawPrice <= 0 {
		return 0
	}
	if t0 == baseToken {
		return rawPrice
	}
	// NOTE:
	// rawPrice is already decimal-adjusted to a human quote (token1 per token0)
	// by sqrtPriceToFloat/getReserves logic. Rotating direction therefore only
	// needs inversion; applying decimals again here double-scales the price and
	// can inflate values by 1e( dec1-dec0 ).
	_ = t1
	_ = dec0
	_ = dec1
	return 1.0 / rawPrice
}

// pow10 is a tiny helper that avoids pulling in math.Pow for integer exponents.
func pow10(n int) float64 {
	r := 1.0
	if n > 0 {
		for i := 0; i < n; i++ {
			r *= 10
		}
	} else {
		for i := 0; i < -n; i++ {
			r /= 10
		}
	}
	return r
}
