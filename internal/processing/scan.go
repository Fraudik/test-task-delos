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
		cur, err := q.LockCursor(ctx, cursorName)
		if err != nil {
			return fmt.Errorf("processing: lock cursor: %w", err)
		}

		next, lastHash, err := p.checkCursor(ctx, uint64(cur.NextBlock), cur.LastHash)
		if err != nil {
			return err
		}
		if next > head {
			if next == uint64(cur.NextBlock) && lastHash == cur.LastHash {
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

func (p *Processor) checkCursor(ctx context.Context, next uint64, lastHash string) (uint64, string, error) {
	if lastHash == "" || next == 0 {
		return next, lastHash, nil
	}
	hash, err := p.chain.BlockHash(ctx, next-1)
	if err != nil && !errors.Is(err, chain.ErrBlockNotFound) {
		return 0, "", fmt.Errorf("processing: block hash %d: %w", next-1, err)
	}
	if err == nil && hash == lastHash {
		return next, lastHash, nil
	}

	rewound := p.cfg.StartBlock
	if back := p.cfg.Confirmations + 1; next > rewound+back {
		rewound = next - back
	}
	p.log.Warn("reorg detected, rescanning", "block", next-1, "rescan_from", rewound)
	return rewound, "", nil
}

func saveCursor(ctx context.Context, q *processingdb.Queries, next uint64, lastHash string) error {
	if err := q.SaveCursor(ctx, processingdb.SaveCursorParams{Name: cursorName, NextBlock: int64(next), LastHash: lastHash}); err != nil {
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
		n, err := q.UpsertDeposit(ctx, processingdb.UpsertDepositParams{
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
		})
		if err != nil {
			return fmt.Errorf("processing: record deposit %s/%d: %w", t.TxHash, t.LogIndex, err)
		}
		if n > 0 {
			p.log.Info("deposit detected", "invoice_id", inv.ID, "tx", t.TxHash, "log_index", t.LogIndex, "block", t.BlockNumber, "amount", int64(t.Value))
		}
	}
	return nil
}
