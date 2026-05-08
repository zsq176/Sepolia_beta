package chain

import (
	"context"
	"fmt"
	"math/big"
	"strings"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient"
)

// FailoverReader routes read-only RPC calls across multiple endpoints.
// It tries endpoints in order and returns on the first success.
type FailoverReader struct {
	clients []*ethclient.Client
	names   []string
}

func NewFailoverReader(clients []*ethclient.Client, names []string) *FailoverReader {
	filteredClients := make([]*ethclient.Client, 0, len(clients))
	filteredNames := make([]string, 0, len(names))
	for i, c := range clients {
		if c == nil {
			continue
		}
		filteredClients = append(filteredClients, c)
		if i < len(names) && strings.TrimSpace(names[i]) != "" {
			filteredNames = append(filteredNames, names[i])
		} else {
			filteredNames = append(filteredNames, fmt.Sprintf("rpc-%d", i+1))
		}
	}
	return &FailoverReader{clients: filteredClients, names: filteredNames}
}

func (f *FailoverReader) CallContract(ctx context.Context, msg ethereum.CallMsg, blockNumber *big.Int) ([]byte, error) {
	var lastErr error
	for _, c := range f.clients {
		data, err := c.CallContract(ctx, msg, blockNumber)
		if err == nil {
			return data, nil
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no rpc clients configured")
	}
	return nil, lastErr
}

func (f *FailoverReader) StorageAt(ctx context.Context, account common.Address, key common.Hash, blockNumber *big.Int) ([]byte, error) {
	var lastErr error
	for _, c := range f.clients {
		data, err := c.StorageAt(ctx, account, key, blockNumber)
		if err == nil {
			return data, nil
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no rpc clients configured")
	}
	return nil, lastErr
}
