package processing

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"test-task-dellos/internal/ledger"
	"test-task-dellos/internal/processing/processingdb"
)

func (p *Processor) ProcessConfirmations(ctx context.Context) (int, error) {
	var (
		claimed   int
		confirmed []int64
		bad       reviewBatch
	)
	err := pgx.BeginFunc(ctx, p.db, func(tx pgx.Tx) error {
		q := processingdb.New(tx)
		rows, err := q.ClaimReadyDeposits(ctx, processingdb.ClaimReadyDepositsParams{
			CursorName:    cursorName,
			Confirmations: int64(p.cfg.Confirmations),
			MaxRows:       p.cfg.BatchSize,
		})
		if err != nil {
			return fmt.Errorf("processing: claim ready deposits: %w", err)
		}
		claimed = len(rows)
		if claimed == 0 {
			return nil
		}

		txns := make([]ledger.Transaction, len(rows))
		for i, r := range rows {
			txns[i] = ledger.Transaction{
				Key:  depositKey(r.ID, "hold"),
				Legs: []ledger.Leg{{Debit: ledger.Custody, Credit: ledger.PartnerHold(r.PartnerID), Amount: r.Amount}},
			}
		}
		results, err := ledger.PostBatch(ctx, tx, txns...)
		if err != nil {
			return fmt.Errorf("processing: post holds: %w", err)
		}
		for i, res := range results {
			if res.Err != nil {
				bad.add(rows[i].ID, res.Err)
				continue
			}
			confirmed = append(confirmed, rows[i].ID)
		}

		if len(confirmed) > 0 {
			n, err := q.ConfirmDeposits(ctx, confirmed)
			if err != nil {
				return fmt.Errorf("processing: confirm deposits: %w", err)
			}
			if int(n) != len(confirmed) {
				return fmt.Errorf("processing: confirmed %d of %d deposits", n, len(confirmed))
			}
		}
		return bad.apply(ctx, q)
	})
	if err != nil {
		return 0, err
	}
	for _, id := range confirmed {
		p.log.Info("deposit confirmed, funds on hold", "deposit_id", id, "confirmations", p.cfg.Confirmations)
	}
	p.logFlagged("confirmations", bad)
	if len(confirmed) > 0 {
		p.wake(stageSettlement)
	}
	return claimed, nil
}
