package chain

import (
	"context"
	"math/big"
)

type Block struct {
	Number    uint64
	Hash      string
	Parent    string
	Transfers []Transfer
}

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
	HeadBlock(ctx context.Context) (Block, error)
	BlockByNumber(ctx context.Context, number uint64) (Block, error)
}
