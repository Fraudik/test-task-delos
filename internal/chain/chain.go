package chain

import (
	"context"
	"math/big"
)

type Transfer struct {
	TxHash      string
	LogIndex    uint32
	BlockNumber uint64
	BlockHash   string
	From        string
	To          string
	Value       *big.Int
}

// Client is the read-only view of the chain
type Client interface {
	HeadBlock(ctx context.Context) (uint64, error)
	BlockHash(ctx context.Context, number uint64) (string, error)
	GetTransfers(ctx context.Context, from, to uint64) ([]Transfer, error)
}
