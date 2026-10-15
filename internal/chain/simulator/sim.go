// Package simulator needs to: create USDT transfer, add confirmations of transaction and create reorg situation
// Simple and close enough implementation will be real block with number (for confirmations),
// hash and parent hash (for checking reorgs), and transactions inside (for USDT transfer).
package simulator

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"sync"

	"test-task-dellos/internal/chain"
)

const zeroHash = "0x0000000000000000000000000000000000000000000000000000000000000000"

var ErrBlockNotFound = errors.New("block not found")

type Simulator struct {
	mu      sync.RWMutex
	blocks  []*chain.Block
	counter uint64
}

func (s *Simulator) nextHash(kind string) string {
	s.counter++
	sum := sha256.Sum256(binary.BigEndian.AppendUint64([]byte(kind), s.counter))
	return "0x" + hex.EncodeToString(sum[:])
}

func New() *Simulator {
	s := &Simulator{}
	s.blocks = []*chain.Block{{Number: 0, Hash: s.nextHash("block"), Parent: zeroHash}}
	return s
}

func (s *Simulator) createBlock(trans []chain.Transfer) *chain.Block {
	parent := s.blocks[len(s.blocks)-1]
	b := &chain.Block{Number: parent.Number + 1, Hash: s.nextHash("block"), Parent: parent.Hash}
	for i, t := range trans {
		t.BlockNumber = b.Number
		t.BlockHash = b.Hash
		t.LogIndex = uint32(i)
		t.Value = new(big.Int).Set(t.Value)
		b.Transfers = append(b.Transfers, t)
	}
	s.blocks = append(s.blocks, b)
	return b
}

func (s *Simulator) headLocked() uint64 { return s.blocks[len(s.blocks)-1].Number }

func (s *Simulator) addBlocksLocked(n int) uint64 {
	for range n {
		s.createBlock(nil)
	}
	return s.headLocked()
}

func (s *Simulator) HeadBlock(_ context.Context) (chain.Block, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return *s.blocks[len(s.blocks)-1], nil
}

func (s *Simulator) BlockByNumber(_ context.Context, number uint64) (chain.Block, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if number >= uint64(len(s.blocks)) {
		return chain.Block{}, ErrBlockNotFound
	}
	return *s.blocks[number], nil
}

func (s *Simulator) SendTransfer(from, to string, value *big.Int) (chain.Transfer, error) {
	if value == nil || value.Sign() <= 0 {
		return chain.Transfer{}, errors.New("simulator: amount must be positive")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	t := chain.Transfer{
		TxHash: s.nextHash("tx"),
		From:   from,
		To:     to,
		Value:  new(big.Int).Set(value),
	}
	b := s.createBlock([]chain.Transfer{t})
	return b.Transfers[0], nil
}

// AddBlocks adds empty blocks for confirmation simulation
func (s *Simulator) AddBlocks(n int) uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.addBlocksLocked(n)
}

// Reorg drops the last depth blocks and replaces them with depth+1 new blocks
func (s *Simulator) Reorg(depth int) (uint64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if depth <= 0 || depth >= len(s.blocks) {
		return 0, fmt.Errorf("simulator: invalid reorg depth %d (head %d)", depth, s.headLocked())
	}
	s.blocks = s.blocks[:len(s.blocks)-depth]
	s.addBlocksLocked(depth + 1)
	return s.headLocked(), nil
}
