// Package uniswap exposes a small set of read-only helpers that translate
// on-chain Uniswap state into a venue-neutral PriceReader interface.
//
// The data layer treats every venue (V2, V3, V4) the same way: given an
// Instrument, return a price (base/quote) and the timestamp at which it was
// observed. Higher layers (market data, strategy) never need to know which
// flavor of Uniswap is behind a quote.
package uniswap

import (
	"context"
	"fmt"
	"strings"
	"time"
	"market-maker-service/internal/domain"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"math/big"
)

// PriceReader fetches a fresh price for a single instrument from the chain.
type PriceReader interface {
	// Read returns the mid price expressed as quote per base. ts is the
	// observation time (wall clock at the moment of the call).
	Read(ctx context.Context, inst *domain.Instrument) (price float64, ts time.Time, err error)
}

// Registry routes an instrument to the right reader by venue.
type Registry struct {
	readers map[domain.Venue]PriceReader
}

// ChainReader captures the minimum RPC surface needed by V2/V3/V4 readers.
type ChainReader interface {
	CallContract(ctx context.Context, msg ethereum.CallMsg, blockNumber *big.Int) ([]byte, error)
	StorageAt(ctx context.Context, account common.Address, key common.Hash, blockNumber *big.Int) ([]byte, error)
}

// NewRegistry wires up the canonical V2/V3/V4 readers.
func NewRegistry(client ChainReader, v4PoolManager common.Address) *Registry {
	return &Registry{
		readers: map[domain.Venue]PriceReader{
			domain.VenueV2: &V2Reader{client: client},
			domain.VenueV3: &V3Reader{client: client},
			domain.VenueV4: &V4Reader{client: client, poolManager: v4PoolManager},
		},
	}
}

// Read dispatches to the correct reader for the instrument's venue.
func (r *Registry) Read(ctx context.Context, inst *domain.Instrument) (float64, time.Time, error) {
	rd, ok := r.readers[inst.Venue]
	if !ok {
		return 0, time.Time{}, fmt.Errorf("no reader for venue %q", inst.Venue)
	}
	return rd.Read(ctx, inst)
}

// Shared ABIs used by the readers.
var (
	erc20ABI, _ = abi.JSON(strings.NewReader(
		`[{"constant":true,"inputs":[],"name":"decimals","outputs":[{"name":"","type":"uint8"}],"type":"function"},
		  {"constant":true,"inputs":[],"name":"token0","outputs":[{"name":"","type":"address"}],"type":"function"},
		  {"constant":true,"inputs":[],"name":"token1","outputs":[{"name":"","type":"address"}],"type":"function"}]`,
	))
	v2PairABI, _ = abi.JSON(strings.NewReader(
		`[{"constant":true,"inputs":[],"name":"getReserves","outputs":[{"internalType":"uint112","name":"_reserve0","type":"uint112"},{"internalType":"uint112","name":"_reserve1","type":"uint112"},{"internalType":"uint32","name":"_blockTimestampLast","type":"uint32"}],"stateMutability":"view","type":"function"}]`,
	))
	v3PoolABI, _ = abi.JSON(strings.NewReader(
		`[{"inputs":[],"name":"slot0","outputs":[{"internalType":"uint160","name":"sqrtPriceX96","type":"uint160"},{"internalType":"int24","name":"tick","type":"int24"},{"internalType":"uint16","name":"observationIndex","type":"uint16"},{"internalType":"uint16","name":"observationCardinality","type":"uint16"},{"internalType":"uint16","name":"observationCardinalityNext","type":"uint16"},{"internalType":"uint32","name":"feeProtocol","type":"uint32"},{"internalType":"bool","name":"unlocked","type":"bool"}],"stateMutability":"view","type":"function"}]`,
	))
)
