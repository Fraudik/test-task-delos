package processing

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"test-task-dellos/internal/chain"
	"test-task-dellos/internal/processing/processingdb"
)

const cursorName = "usdt"

// ScanBlocks scans new block or unprocessed block with batches (if scanner lags behind)
func (p *Processor) ScanBlocks(ctx context.Context) (int, error) {
	head, err := p.chain.HeadBlock(ctx)
	if err != nil {
		return 0, fmt.Errorf("processing: head block: %w", err)
	}

	scanned := 0
	err = pgx.BeginFunc(ctx, p.db, func(tx pgx.Tx) error {
		q := processingdb.New(tx)
		if err := q.EnsureCursor(ctx, processingdb.EnsureCursorParams{Name: cursorName, NextBlock: int64(p.cfg.StartBlock)}); err != nil {
			return fmt.Errorf("processing: ensure cursor: %w", err)
		}
		cur, err := q.GetCursor(ctx, cursorName)
		if err != nil {
			return fmt.Errorf("processing: get cursor: %w", err)
		}

		next, lastHash, reorged, err := p.checkCursor(ctx, uint64(cur.NextBlock), cur.LastHash)
		if err != nil {
			return err
		}
		if reorged {
			if err := p.recheckPending(ctx, q, next); err != nil {
				return err
			}
		}
		if next > head {
			if !reorged {
				return nil
			}
			return saveCursor(ctx, q, next, lastHash)
		}

		to := min(head, next+p.cfg.ScanBatch-1)
		transfers, err := p.chain.GetTransfers(ctx, next, to)
		if err != nil {
			return fmt.Errorf("processing: transfers %d..%d: %w", next, to, err)
		}
		if err := p.recordDeposits(ctx, q, transfers); err != nil {
			return err
		}
		toHash, err := p.chain.BlockHash(ctx, to)
		if err != nil {
			return fmt.Errorf("processing: block hash %d: %w", to, err)
		}
		scanned = int(to - next + 1)
		return saveCursor(ctx, q, to+1, toHash)
	})
	if err != nil {
		return 0, err
	}
	return scanned, nil
}

func (p *Processor) checkCursor(ctx context.Context, next uint64, lastHash string) (uint64, string, bool, error) {
	if lastHash == "" || next == 0 {
		return next, lastHash, false, nil
	}
	hash, err := p.chain.BlockHash(ctx, next-1)
	if err != nil && !errors.Is(err, chain.ErrBlockNotFound) {
		return 0, "", false, fmt.Errorf("processing: block hash %d: %w", next-1, err)
	}
	if err == nil && hash == lastHash {
		return next, lastHash, false, nil
	}

	rewound := p.cfg.StartBlock
	if back := p.cfg.Confirmations + 1; next > rewound+back {
		rewound = next - back
	}
	p.log.Warn("reorg detected, rescanning", "block", next-1, "rescan_from", rewound)
	return rewound, "", true, nil
}

func (p *Processor) recheckPending(ctx context.Context, q *processingdb.Queries, from uint64) error {
	pending, err := q.PendingDepositsFrom(ctx, int64(from))
	if err != nil {
		return fmt.Errorf("processing: pending deposits from %d: %w", from, err)
	}
	blocks := make([]int64, len(pending))
	for i, d := range pending {
		blocks[i] = d.BlockNumber
	}
	hashes, err := p.canonicalHashes(ctx, blocks)
	if err != nil {
		return err
	}
	for _, d := range pending {
		if hashes[d.BlockNumber] == d.BlockHash {
			continue
		}
		if _, err := p.orphan(ctx, q, d.ID, d.BlockNumber, d.BlockHash); err != nil {
			return err
		}
	}
	return nil
}

func (p *Processor) canonicalHashes(ctx context.Context, blocks []int64) (map[int64]string, error) {
	hashes := make(map[int64]string, len(blocks))
	for _, b := range blocks {
		if _, ok := hashes[b]; ok {
			continue
		}
		h, err := p.chain.BlockHash(ctx, uint64(b))
		if err != nil && !errors.Is(err, chain.ErrBlockNotFound) {
			return nil, fmt.Errorf("processing: block hash %d: %w", b, err)
		}
		hashes[b] = h
	}
	return hashes, nil
}

func (p *Processor) orphan(ctx context.Context, q *processingdb.Queries, depositID, block int64, blockHash string) (bool, error) {
	n, err := q.OrphanDeposit(ctx, processingdb.OrphanDepositParams{ID: depositID, BlockHash: blockHash})
	if err != nil {
		return false, fmt.Errorf("processing: orphan deposit %d: %w", depositID, err)
	}
	if n > 0 {
		p.log.Warn("deposit orphaned by reorg", "deposit_id", depositID, "block", block, "block_hash", blockHash)
	}
	return n > 0, nil
}

func saveCursor(ctx context.Context, q *processingdb.Queries, next uint64, lastHash string) error {
	err := q.SaveCursor(ctx, processingdb.SaveCursorParams{
		Name:      cursorName,
		NextBlock: int64(next),
		LastHash:  lastHash,
	})
	if err != nil {
		return fmt.Errorf("processing: save cursor: %w", err)
	}
	return nil
}

func (p *Processor) recordDeposits(ctx context.Context, q *processingdb.Queries, transfers []chain.Transfer) error {
	if len(transfers) == 0 {
		return nil
	}
	addresses := make([]string, 0, len(transfers))
	for _, t := range transfers {
		addresses = append(addresses, strings.ToLower(t.To))
	}
	invoices, err := q.InvoicesByAddresses(ctx, addresses)
	if err != nil {
		return fmt.Errorf("processing: invoices by address: %w", err)
	}
	byAddress := make(map[string]processingdb.InvoicesByAddressesRow, len(invoices))
	for _, inv := range invoices {
		byAddress[inv.Address] = inv
	}

	for _, t := range transfers {
		inv, ok := byAddress[strings.ToLower(t.To)]
		if !ok {
			continue
		}
		if t.Value <= 0 {
			p.log.Warn("skipping non-positive transfer", "tx", t.TxHash, "log_index", t.LogIndex, "value", int64(t.Value))
			continue
		}
		id, err := q.UpsertDeposit(ctx, processingdb.UpsertDepositParams{
			InvoiceID:   inv.ID,
			PartnerID:   inv.PartnerID,
			TxHash:      t.TxHash,
			LogIndex:    int32(t.LogIndex),
			BlockNumber: int64(t.BlockNumber),
			BlockHash:   t.BlockHash,
			FromAddress: strings.ToLower(t.From),
			ToAddress:   inv.Address,
			Amount:      t.Value,
			FeeBps:      inv.FeeBps,
			FeeFixed:    inv.FeeFixed,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return fmt.Errorf("processing: record deposit %s/%d: %w", t.TxHash, t.LogIndex, err)
		}
		if err := q.EnsureAMLCheck(ctx, id); err != nil {
			return fmt.Errorf("processing: schedule AML check for deposit %d: %w", id, err)
		}
		p.log.Info("deposit detected", "deposit_id", id, "invoice_id", inv.ID, "tx", t.TxHash, "log_index", t.LogIndex, "block", t.BlockNumber, "amount", int64(t.Value))
	}
	return nil
}
