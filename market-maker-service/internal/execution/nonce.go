package execution

import (
	"context"
	"crypto/ecdsa"
	"fmt"
	"math/big"
	"sync"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
)

// Nonces serializes nonce allocation across goroutines and signs every
// transaction. It is the single mutator of "in-flight nonce" state, so even
// when V3 and V4 executors run concurrently the chain only sees monotonically
// increasing nonces from this account.
type Nonces struct {
	client  *ethclient.Client
	pk      *ecdsa.PrivateKey
	address common.Address
	chainID *big.Int

	mu   sync.Mutex
	next uint64 // next nonce to assign; 0 means "ask the node"
}

// NewNonces resolves the from-address from the private key.
func NewNonces(client *ethclient.Client, pk *ecdsa.PrivateKey, chainID *big.Int) *Nonces {
	return &Nonces{
		client:  client,
		pk:      pk,
		address: crypto.PubkeyToAddress(pk.PublicKey),
		chainID: chainID,
	}
}

// Address returns the from-address derived from the configured private key.
func (n *Nonces) Address() common.Address { return n.address }

// SignAndSend allocates the next nonce, signs the dynamic-fee tx, and submits.
// It returns the signed tx (so the caller can wait for the receipt).
func (n *Nonces) SignAndSend(ctx context.Context, to common.Address, data []byte, value *big.Int, gasLimit uint64) (*types.Transaction, error) {
	n.mu.Lock()
	defer n.mu.Unlock()

	nonce, err := n.allocate(ctx)
	if err != nil {
		return nil, fmt.Errorf("nonce: %w", err)
	}

	tip, err := n.client.SuggestGasTipCap(ctx)
	if err != nil {
		return nil, fmt.Errorf("gas tip: %w", err)
	}
	feeCap, err := n.client.SuggestGasPrice(ctx)
	if err != nil {
		return nil, fmt.Errorf("gas price: %w", err)
	}

	tx := types.NewTx(&types.DynamicFeeTx{
		ChainID:   n.chainID,
		Nonce:     nonce,
		GasTipCap: tip,
		GasFeeCap: feeCap,
		Gas:       gasLimit,
		To:        &to,
		Value:     value,
		Data:      data,
	})
	signed, err := types.SignTx(tx, types.LatestSignerForChainID(n.chainID), n.pk)
	if err != nil {
		n.next = 0 // resync on failure
		return nil, fmt.Errorf("sign: %w", err)
	}
	if err := n.client.SendTransaction(ctx, signed); err != nil {
		n.next = 0 // resync on failure
		return nil, fmt.Errorf("send: %w", err)
	}
	n.next = nonce + 1
	return signed, nil
}

func (n *Nonces) allocate(ctx context.Context) (uint64, error) {
	if n.next == 0 {
		pending, err := n.client.PendingNonceAt(ctx, n.address)
		if err != nil {
			return 0, err
		}
		n.next = pending
	}
	return n.next, nil
}
