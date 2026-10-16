// Package simulator needs to: create USDT transfer, add confirmations of transaction and create reorg situation
// Simple and close enough implementation will be real block with number (for confirmations),
// hash and parent hash (for checking reorgs), and transactions inside (for USDT transfer).
package simulator

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"

	"test-task-dellos/internal/chain"
	"test-task-dellos/internal/money"
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
		b.transfers = append(b.transfers, t)
	}
	s.blocks = append(s.blocks, b)
	return b
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

// BlockHashes returns the hashes of canonical blocks in [from, to]
func (s *Simulator) BlockHashes(_ context.Context, from, to uint64) ([]string, error) {
	if from > to {
		return nil, fmt.Errorf("simulator: invalid block range %d..%d", from, to)
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if to >= uint64(len(s.blocks)) {
		return nil, chain.ErrBlockNotFound
	}
	hashes := make([]string, 0, to-from+1)
	for n := from; n <= to; n++ {
		hashes = append(hashes, s.blocks[n].hash)
	}
	return hashes, nil
}

// GetTransfers returns transfers from canonical blocks in [from, to]; heights above head are skipped
func (s *Simulator) GetTransfers(_ context.Context, from, to uint64) ([]chain.Transfer, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []chain.Transfer
	for n := from; n <= to && n < uint64(len(s.blocks)); n++ {
		out = append(out, s.blocks[n].transfers...)
	}
	return out, nil
}

func (s *Simulator) SendTransfer(from, to string, value money.Amount) (chain.Transfer, error) {
	if value <= 0 {
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
	return b.transfers[0], nil
}

// AddBlocks adds empty blocks for confirmation simulation
func (s *Simulator) AddBlocks(n int) uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.addBlocksLocked(n)
}

// Reorg drops the last depth blocks and replaces them with depth new blocks, so the head height stays the same
func (s *Simulator) Reorg(depth int) (uint64, error) {
	return s.reorg(depth, false)
}

// ReorgWithReinclusion is Reorg that mines the transfers of the dropped blocks again into the first new block
func (s *Simulator) ReorgWithReinclusion(depth int) (uint64, error) {
	return s.reorg(depth, true)
}

func (s *Simulator) reorg(depth int, reinclude bool) (uint64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if depth <= 0 || depth >= len(s.blocks) {
		return 0, fmt.Errorf("simulator: invalid reorg depth %d (head %d)", depth, s.headLocked())
	}
	dropped := s.blocks[len(s.blocks)-depth:]
	s.blocks = s.blocks[:len(s.blocks)-depth]
	var transfers []chain.Transfer
	if reinclude {
		for _, b := range dropped {
			transfers = append(transfers, b.transfers...)
		}
	}
	s.createBlock(transfers)
	s.addBlocksLocked(depth - 1)
	return s.headLocked(), nil
}

type snapshot struct {
	Counter uint64          `json:"counter"`
	Blocks  []snapshotBlock `json:"blocks"`
}

type snapshotBlock struct {
	Number    uint64           `json:"number"`
	Hash      string           `json:"hash"`
	Parent    string           `json:"parent"`
	Transfers []chain.Transfer `json:"transfers"`
}

// SaveFile writes the chain to path atomically, so a restarted service continues the same chain
func (s *Simulator) SaveFile(path string) error {
	s.mu.RLock()
	snap := snapshot{Counter: s.counter, Blocks: make([]snapshotBlock, len(s.blocks))}
	for i, b := range s.blocks {
		snap.Blocks[i] = snapshotBlock{Number: b.number, Hash: b.hash, Parent: b.parent, Transfers: b.transfers}
	}
	s.mu.RUnlock()

	data, err := json.Marshal(snap)
	if err != nil {
		return fmt.Errorf("simulator: encode state: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("simulator: write state: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("simulator: replace state: %w", err)
	}
	return nil
}

// LoadFile restores a chain saved by SaveFile; a missing file gives a new chain
func LoadFile(path string) (*Simulator, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return New(), nil
	}
	if err != nil {
		return nil, fmt.Errorf("simulator: read state: %w", err)
	}
	var snap snapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		return nil, fmt.Errorf("simulator: decode state: %w", err)
	}
	if len(snap.Blocks) == 0 {
		return nil, errors.New("simulator: state has no blocks")
	}
	s := &Simulator{counter: snap.Counter, blocks: make([]*block, len(snap.Blocks))}
	for i, b := range snap.Blocks {
		s.blocks[i] = &block{number: b.Number, hash: b.Hash, parent: b.Parent, transfers: b.Transfers}
	}
	return s, nil
}
