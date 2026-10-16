package scanner

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"test-task-dellos/internal/chain"
	"test-task-dellos/internal/processing/deposit"
	"test-task-dellos/internal/processing/scanner/scannerdb"
)

type Config struct {
	StartBlock   uint64
	ScanBatch    uint64
	ReorgHistory uint64
}

type Stage struct {
	db    *pgxpool.Pool
	chain chain.Client
	cfg   Config
	log   *slog.Logger
}

func New(db *pgxpool.Pool, c chain.Client, cfg Config, log *slog.Logger) *Stage {
	return &Stage{db: db, chain: c, cfg: cfg, log: log.With("stage", "scan")}
}

// Run scans new block or unprocessed block with batches (if scanner lags behind)
func (s *Stage) Run(ctx context.Context) (int, error) {
	head, err := s.chain.HeadBlock(ctx)
	if err != nil {
		return 0, fmt.Errorf("scanner: head block: %w", err)
	}

	scanned := 0
	err = pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		q := scannerdb.New(tx)
		if err := q.EnsureCursor(ctx, scannerdb.EnsureCursorParams{Name: deposit.CursorName, NextBlock: int64(s.cfg.StartBlock)}); err != nil {
			return fmt.Errorf("scanner: ensure cursor: %w", err)
		}
		cur, err := q.GetCursor(ctx, deposit.CursorName)
		if err != nil {
			return fmt.Errorf("scanner: get cursor: %w", err)
		}

		next, reorged, err := s.checkCursor(ctx, q, uint64(cur), head)
		if err != nil {
			return err
		}
		if next > head {
			if !reorged {
				return nil
			}
			return saveCursor(ctx, q, next)
		}

		to := min(head, next+s.cfg.ScanBatch-1)
		b, consistent, err := s.fetch(ctx, q, next, to)
		if err != nil {
			return err
		}
		if !consistent {
			if !reorged {
				return nil
			}
			return saveCursor(ctx, q, next)
		}
		if err := s.recordDeposits(ctx, q, b.transfers); err != nil {
			return err
		}
		if err := s.saveBlocks(ctx, q, next, b.hashes); err != nil {
			return err
		}
		scanned = int(to - next + 1)
		return saveCursor(ctx, q, to+1)
	})
	if err != nil {
		return 0, err
	}
	return scanned, nil
}

func (s *Stage) storedHash(ctx context.Context, q *scannerdb.Queries, number uint64) (string, bool, error) {
	hash, err := q.StoredBlockHash(ctx, scannerdb.StoredBlockHashParams{CursorName: deposit.CursorName, Number: int64(number)})
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("scanner: stored hash of block %d: %w", number, err)
	}
	return hash, true, nil
}

func (s *Stage) checkCursor(ctx context.Context, q *scannerdb.Queries, next, head uint64) (uint64, bool, error) {
	if next == 0 {
		return next, false, nil
	}
	stored, ok, err := s.storedHash(ctx, q, next-1)
	if err != nil || !ok {
		return next, false, err
	}
	hash, err := s.chain.BlockHash(ctx, next-1)
	if err != nil && !errors.Is(err, chain.ErrBlockNotFound) {
		return 0, false, fmt.Errorf("scanner: block hash %d: %w", next-1, err)
	}
	if err == nil && hash == stored {
		return next, false, nil
	}

	rewound, err := s.forkPoint(ctx, q, next-1, head)
	if err != nil {
		return 0, false, err
	}
	if err := q.ForgetBlocksFrom(ctx, scannerdb.ForgetBlocksFromParams{CursorName: deposit.CursorName, FromBlock: int64(rewound)}); err != nil {
		return 0, false, fmt.Errorf("scanner: forget blocks from %d: %w", rewound, err)
	}
	if err := s.recheckPending(ctx, q, rewound); err != nil {
		return 0, false, err
	}
	return rewound, true, nil
}

// forkPoint walks the stored hashes down from the changed block and returns the first block after the last one still on the chain
func (s *Stage) forkPoint(ctx context.Context, q *scannerdb.Queries, changed, head uint64) (uint64, error) {
	stored, err := q.StoredBlocksBelow(ctx, scannerdb.StoredBlocksBelowParams{CursorName: deposit.CursorName, Below: int64(changed)})
	if err != nil {
		return 0, fmt.Errorf("scanner: stored blocks below %d: %w", changed, err)
	}
	if len(stored) > 0 {
		lo, hi := uint64(stored[len(stored)-1].Number), min(uint64(stored[0].Number), head)
		if lo <= hi {
			hashes, err := s.chain.BlockHashes(ctx, lo, hi)
			if err != nil {
				return 0, fmt.Errorf("scanner: block hashes %d..%d: %w", lo, hi, err)
			}
			for _, b := range stored {
				n := uint64(b.Number)
				if n <= hi && hashes[n-lo] == b.Hash {
					s.log.Warn("reorg detected, rescanning", "block", changed, "common_ancestor", n, "rescan_from", n+1)
					return n + 1, nil
				}
			}
		}
	}
	s.log.Error("reorg deeper than stored block history, rescanning from start block",
		"block", changed, "stored_blocks", len(stored), "rescan_from", s.cfg.StartBlock)
	return s.cfg.StartBlock, nil
}

type batch struct {
	transfers []chain.Transfer
	hashes    []string
}

// fetch reads transfers and block hashes of [next, to] and reports whether they belong to one chain that extends the scanned blocks
func (s *Stage) fetch(ctx context.Context, q *scannerdb.Queries, next, to uint64) (batch, bool, error) {
	transfers, err := s.chain.GetTransfers(ctx, next, to)
	if err != nil {
		return batch{}, false, fmt.Errorf("scanner: transfers %d..%d: %w", next, to, err)
	}
	from, parent, hasParent := next, "", false
	if next > 0 {
		if parent, hasParent, err = s.storedHash(ctx, q, next-1); err != nil {
			return batch{}, false, err
		}
		if hasParent {
			from = next - 1
		}
	}
	hashes, err := s.chain.BlockHashes(ctx, from, to)
	if errors.Is(err, chain.ErrBlockNotFound) {
		s.log.Warn("chain changed during scan, retrying", "from", next, "to", to)
		return batch{}, false, nil
	}
	if err != nil {
		return batch{}, false, fmt.Errorf("scanner: block hashes %d..%d: %w", from, to, err)
	}
	if hasParent {
		if hashes[0] != parent {
			s.log.Warn("chain changed during scan, retrying", "from", next, "to", to)
			return batch{}, false, nil
		}
		hashes = hashes[1:]
	}
	for _, t := range transfers {
		if t.BlockNumber < next || t.BlockNumber > to || hashes[t.BlockNumber-next] != t.BlockHash {
			s.log.Warn("chain changed during scan, retrying", "from", next, "to", to, "tx", t.TxHash)
			return batch{}, false, nil
		}
	}
	return batch{transfers: transfers, hashes: hashes}, true, nil
}

func (s *Stage) saveBlocks(ctx context.Context, q *scannerdb.Queries, from uint64, hashes []string) error {
	numbers := make([]int64, len(hashes))
	for i := range hashes {
		numbers[i] = int64(from) + int64(i)
	}
	if err := q.SaveBlocks(ctx, scannerdb.SaveBlocksParams{CursorName: deposit.CursorName, Numbers: numbers, Hashes: hashes}); err != nil {
		return fmt.Errorf("scanner: save block hashes: %w", err)
	}
	end := from + uint64(len(hashes))
	if end <= s.cfg.ReorgHistory {
		return nil
	}
	if err := q.PruneBlocksBelow(ctx, scannerdb.PruneBlocksBelowParams{CursorName: deposit.CursorName, Below: int64(end - s.cfg.ReorgHistory)}); err != nil {
		return fmt.Errorf("scanner: prune block hashes: %w", err)
	}
	return nil
}

func (s *Stage) recheckPending(ctx context.Context, q *scannerdb.Queries, from uint64) error {
	pending, err := q.PendingDepositsFrom(ctx, int64(from))
	if err != nil {
		return fmt.Errorf("scanner: pending deposits from %d: %w", from, err)
	}
	blocks := make([]int64, len(pending))
	for i, d := range pending {
		blocks[i] = d.BlockNumber
	}
	hashes, err := s.canonicalHashes(ctx, blocks)
	if err != nil {
		return err
	}
	for _, d := range pending {
		if hashes[d.BlockNumber] == d.BlockHash {
			continue
		}
		if _, err := s.orphan(ctx, q, d.ID, d.BlockNumber, d.BlockHash); err != nil {
			return err
		}
	}
	return nil
}

func (s *Stage) canonicalHashes(ctx context.Context, blocks []int64) (map[int64]string, error) {
	hashes := make(map[int64]string, len(blocks))
	for _, b := range blocks {
		if _, ok := hashes[b]; ok {
			continue
		}
		h, err := s.chain.BlockHash(ctx, uint64(b))
		if err != nil && !errors.Is(err, chain.ErrBlockNotFound) {
			return nil, fmt.Errorf("scanner: block hash %d: %w", b, err)
		}
		hashes[b] = h
	}
	return hashes, nil
}

func (s *Stage) orphan(ctx context.Context, q *scannerdb.Queries, depositID, block int64, blockHash string) (bool, error) {
	n, err := q.OrphanDeposit(ctx, scannerdb.OrphanDepositParams{ID: depositID, BlockHash: blockHash})
	if err != nil {
		return false, fmt.Errorf("scanner: orphan deposit %d: %w", depositID, err)
	}
	if n > 0 {
		s.log.Warn("deposit orphaned by reorg", "deposit_id", depositID, "block", block, "block_hash", blockHash)
	}
	return n > 0, nil
}

func saveCursor(ctx context.Context, q *scannerdb.Queries, next uint64) error {
	err := q.SaveCursor(ctx, scannerdb.SaveCursorParams{Name: deposit.CursorName, NextBlock: int64(next)})
	if err != nil {
		return fmt.Errorf("scanner: save cursor: %w", err)
	}
	return nil
}

func (s *Stage) recordDeposits(ctx context.Context, q *scannerdb.Queries, transfers []chain.Transfer) error {
	if len(transfers) == 0 {
		return nil
	}
	addresses := make([]string, 0, len(transfers))
	for _, t := range transfers {
		addresses = append(addresses, strings.ToLower(t.To))
	}
	invoices, err := q.InvoicesByAddresses(ctx, addresses)
	if err != nil {
		return fmt.Errorf("scanner: invoices by address: %w", err)
	}
	byAddress := make(map[string]scannerdb.InvoicesByAddressesRow, len(invoices))
	for _, inv := range invoices {
		byAddress[inv.Address] = inv
	}

	for _, t := range transfers {
		inv, ok := byAddress[strings.ToLower(t.To)]
		if !ok {
			continue
		}
		if t.Value <= 0 {
			s.log.Warn("skipping non-positive transfer", "tx", t.TxHash, "log_index", t.LogIndex, "value", int64(t.Value))
			continue
		}
		id, err := q.UpsertDeposit(ctx, scannerdb.UpsertDepositParams{
			InvoiceID:   inv.ID,
			PartnerID:   inv.PartnerID,
			TxHash:      t.TxHash,
			LogIndex:    int32(t.LogIndex),
			BlockNumber: int64(t.BlockNumber),
			BlockHash:   t.BlockHash,
			FromAddress: strings.ToLower(t.From),
			ToAddress:   inv.Address,
			Amount:      t.Value,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return fmt.Errorf("scanner: record deposit %s/%d: %w", t.TxHash, t.LogIndex, err)
		}
		if err := q.EnsureAMLCheck(ctx, id); err != nil {
			return fmt.Errorf("scanner: schedule AML check for deposit %d: %w", id, err)
		}
		s.log.Info("deposit detected", "deposit_id", id, "invoice_id", inv.ID, "tx", t.TxHash, "log_index", t.LogIndex, "block", t.BlockNumber, "amount", int64(t.Value))
	}
	return nil
}
