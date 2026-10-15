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

type block struct {
	number    uint64
	hash      string
	parent    string
	transfers []chain.Transfer
}

type Simulator struct {
	mu      sync.RWMutex
	blocks  []*block
	counter uint64
}

func (s *Simulator) nextHash(kind string) string {
	s.counter++
	sum := sha256.Sum256(binary.BigEndian.AppendUint64([]byte(kind), s.counter))
	return "0x" + hex.EncodeToString(sum[:])
}

func New() *Simulator {
	s := &Simulator{}
	s.blocks = []*block{{number: 0, hash: s.nextHash("block"), parent: zeroHash}}
	return s
}

func (s *Simulator) createBlock(trans []chain.Transfer) *block {
	parent := s.blocks[len(s.blocks)-1]
	b := &block{number: parent.number + 1, hash: s.nextHash("block"), parent: parent.hash}
	for i, t := range trans {
		t.BlockNumber = b.number
		t.BlockHash = b.hash
		t.LogIndex = uint32(i)
		b.transfers = append(b.transfers, copyTransfer(t))
	}
	s.blocks = append(s.blocks, b)
	return b
}

// copyTransfer returns a copy of t with its own Value so callers can't mutate simulator state.
func copyTransfer(t chain.Transfer) chain.Transfer {
	t.Value = new(big.Int).Set(t.Value)
	return t
}

func (s *Simulator) headLocked() uint64 { return s.blocks[len(s.blocks)-1].number }

func (s *Simulator) addBlocksLocked(n int) uint64 {
	for range n {
		s.createBlock(nil)
	}
	return s.headLocked()
}

// HeadBlock returns the number of the latest block
func (s *Simulator) HeadBlock(_ context.Context) (uint64, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.headLocked(), nil
}

// BlockHash returns the hash of the canonical block at the given height
func (s *Simulator) BlockHash(_ context.Context, number uint64) (string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if number >= uint64(len(s.blocks)) {
		return "", chain.ErrBlockNotFound
	}
	return s.blocks[number].hash, nil
}

// GetTransfers returns transfers from canonical blocks in [from, to]; heights above head are skipped
func (s *Simulator) GetTransfers(_ context.Context, from, to uint64) ([]chain.Transfer, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []chain.Transfer
	for n := from; n <= to && n < uint64(len(s.blocks)); n++ {
		for _, t := range s.blocks[n].transfers {
			out = append(out, copyTransfer(t))
		}
	}
	return out, nil
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
		Value:  value,
	}
	b := s.createBlock([]chain.Transfer{t})
	return copyTransfer(b.transfers[0]), nil
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
