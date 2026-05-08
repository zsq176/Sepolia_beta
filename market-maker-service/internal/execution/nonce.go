package execution

import (
	"context"
	"crypto/ecdsa"
	"fmt"
	"math/big"
	"strings"
	"sync"

	ethereum "github.com/ethereum/go-ethereum"
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
	client      *ethclient.Client
	broadcasters []*ethclient.Client
	pk          *ecdsa.PrivateKey
	address     common.Address
	chainID     *big.Int

	mu   sync.Mutex
	next uint64 // next nonce to assign; 0 means "ask the node"
}

// NewNonces resolves the from-address from the private key.
func NewNonces(client *ethclient.Client, broadcasters []*ethclient.Client, pk *ecdsa.PrivateKey, chainID *big.Int) *Nonces {
	bs := make([]*ethclient.Client, 0, len(broadcasters))
	for _, c := range broadcasters {
		if c != nil {
			bs = append(bs, c)
		}
	}
	return &Nonces{
		client:       client,
		broadcasters: bs,
		pk:           pk,
		address:      crypto.PubkeyToAddress(pk.PublicKey),
		chainID:      chainID,
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

	tip, err := n.suggestGasTipCapWithFailover(ctx)
	if err != nil {
		return nil, fmt.Errorf("gas tip: %w", err)
	}
	feeCap, err := n.suggestGasPriceWithFailover(ctx)
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
	if err := n.sendWithFailover(ctx, signed); err != nil {
		n.next = 0 // resync on failure
		return nil, fmt.Errorf("send: %w", err)
	}
	n.next = nonce + 1
	return signed, nil
}

func (n *Nonces) sendWithFailover(ctx context.Context, tx *types.Transaction) error {
	if len(n.broadcasters) == 0 {
		return fmt.Errorf("no broadcaster configured")
	}
	var lastErr error
	for _, c := range n.broadcasters {
		if c == nil {
			continue
		}
		err := c.SendTransaction(ctx, tx)
		if err == nil || isAlreadyKnownErr(err) {
			return nil
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("all rpc broadcasters failed")
	}
	return lastErr
}

func (n *Nonces) suggestGasTipCapWithFailover(ctx context.Context) (*big.Int, error) {
	var lastErr error
	for _, c := range n.broadcasters {
		if c == nil {
			continue
		}
		v, err := c.SuggestGasTipCap(ctx)
		if err == nil && v != nil && v.Sign() > 0 {
			return v, nil
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("failed to suggest gas tip from all rpc endpoints")
	}
	return nil, lastErr
}

func (n *Nonces) suggestGasPriceWithFailover(ctx context.Context) (*big.Int, error) {
	var lastErr error
	for _, c := range n.broadcasters {
		if c == nil {
			continue
		}
		v, err := c.SuggestGasPrice(ctx)
		if err == nil && v != nil && v.Sign() > 0 {
			return v, nil
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("failed to suggest gas price from all rpc endpoints")
	}
	return nil, lastErr
}

func (n *Nonces) CallContractWithFailover(ctx context.Context, msg ethereum.CallMsg) ([]byte, error) {
	var lastErr error
	for _, c := range n.broadcasters {
		if c == nil {
			continue
		}
		out, err := c.CallContract(ctx, msg, nil)
		if err == nil {
			return out, nil
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("call contract failed on all rpc endpoints")
	}
	return nil, lastErr
}

// CallContract satisfies execution read interfaces and routes calls via failover.
func (n *Nonces) CallContract(ctx context.Context, msg ethereum.CallMsg, _ *big.Int) ([]byte, error) {
	return n.CallContractWithFailover(ctx, msg)
}

func isAlreadyKnownErr(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "already known") ||
		strings.Contains(msg, "known transaction")
}

func (n *Nonces) allocate(ctx context.Context) (uint64, error) {
	if n.next == 0 {
		pending, err := n.pendingNonceWithFailover(ctx)
		if err != nil {
			return 0, err
		}
		n.next = pending
	}
	return n.next, nil
}

func (n *Nonces) pendingNonceWithFailover(ctx context.Context) (uint64, error) {
	var lastErr error
	for _, c := range n.broadcasters {
		if c == nil {
			continue
		}
		v, err := c.PendingNonceAt(ctx, n.address)
		if err == nil {
			return v, nil
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("pending nonce failed on all rpc endpoints")
	}
	return 0, lastErr
}

func (n *Nonces) TransactionReceipt(ctx context.Context, txHash common.Hash) (*types.Receipt, error) {
	var lastErr error
	for _, c := range n.broadcasters {
		if c == nil {
			continue
		}
		rec, err := c.TransactionReceipt(ctx, txHash)
		if err == nil && rec != nil {
			return rec, nil
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("tx receipt failed on all rpc endpoints")
	}
	return nil, lastErr
}
