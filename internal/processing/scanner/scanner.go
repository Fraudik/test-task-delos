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
	Confirmations uint64
	StartBlock    uint64
	ScanBatch     uint64
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

		next, lastHash, reorged, err := s.checkCursor(ctx, uint64(cur.NextBlock), cur.LastHash)
		if err != nil {
			return err
		}
		if reorged {
			if err := s.recheckPending(ctx, q, next); err != nil {
				return err
			}
		}
		if next > head {
			if !reorged {
				return nil
			}
			return saveCursor(ctx, q, next, lastHash)
		}

		to := min(head, next+s.cfg.ScanBatch-1)
		transfers, err := s.chain.GetTransfers(ctx, next, to)
		if err != nil {
			return fmt.Errorf("scanner: transfers %d..%d: %w", next, to, err)
		}
		if err := s.recordDeposits(ctx, q, transfers); err != nil {
			return err
		}
		toHash, err := s.chain.BlockHash(ctx, to)
		if err != nil {
			return fmt.Errorf("scanner: block hash %d: %w", to, err)
		}
		scanned = int(to - next + 1)
		return saveCursor(ctx, q, to+1, toHash)
	})
	if err != nil {
		return 0, err
	}
	return scanned, nil
}

func (s *Stage) checkCursor(ctx context.Context, next uint64, lastHash string) (uint64, string, bool, error) {
	if lastHash == "" || next == 0 {
		return next, lastHash, false, nil
	}
	hash, err := s.chain.BlockHash(ctx, next-1)
	if err != nil && !errors.Is(err, chain.ErrBlockNotFound) {
		return 0, "", false, fmt.Errorf("scanner: block hash %d: %w", next-1, err)
	}
	if err == nil && hash == lastHash {
		return next, lastHash, false, nil
	}

	rewound := s.cfg.StartBlock
	if back := s.cfg.Confirmations + 1; next > rewound+back {
		rewound = next - back
	}
	s.log.Warn("reorg detected, rescanning", "block", next-1, "rescan_from", rewound)
	return rewound, "", true, nil
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

func saveCursor(ctx context.Context, q *scannerdb.Queries, next uint64, lastHash string) error {
	err := q.SaveCursor(ctx, scannerdb.SaveCursorParams{
		Name:      deposit.CursorName,
		NextBlock: int64(next),
		LastHash:  lastHash,
	})
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
