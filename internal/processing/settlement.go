package processing

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"test-task-dellos/internal/aml"
	"test-task-dellos/internal/ledger"
	"test-task-dellos/internal/processing/processingdb"
)

func (p *Processor) ProcessSettlement(ctx context.Context) (int, error) {
	var (
		claimed int
		done    finishBatch
		bad     reviewBatch
	)
	err := pgx.BeginFunc(ctx, p.db, func(tx pgx.Tx) error {
		q := processingdb.New(tx)
		rows, err := q.ClaimSettleableDeposits(ctx, p.cfg.BatchSize)
		if err != nil {
			return fmt.Errorf("processing: claim settleable deposits: %w", err)
		}
		claimed = len(rows)
		if claimed == 0 {
			return nil
		}

		var (
			txns    []ledger.Transaction
			planned []processingdb.ClaimSettleableDepositsRow
		)
		for _, r := range rows {
			t, err := settlementTxn(r)
			if err != nil {
				bad.add(r.ID, err)
				continue
			}
			txns = append(txns, t)
			planned = append(planned, r)
		}
		if len(txns) > 0 {
			results, err := ledger.PostBatch(ctx, tx, txns...)
			if err != nil {
				return fmt.Errorf("processing: post settlements: %w", err)
			}
			for i, res := range results {
				if res.Err != nil {
					bad.add(planned[i].ID, res.Err)
					continue
				}
				done.add(planned[i])
			}
		}

		if err := done.apply(ctx, q); err != nil {
			return err
		}
		return bad.apply(ctx, q)
	})
	if err != nil {
		return 0, err
	}
	for i, id := range done.finish.Ids {
		p.log.Info("deposit processed", "deposit_id", id, "status", done.finish.Statuses[i])
	}
	p.logFlagged("settlement", bad)
	return claimed, nil
}

func settlementTxn(d processingdb.ClaimSettleableDepositsRow) (ledger.Transaction, error) {
	hold := ledger.PartnerHold(d.PartnerID)
	switch aml.Risk(d.AmlRisk) {
	case aml.RiskLow:
		net, fee := splitFee(d.Amount, d.FeeBps, d.FeeFixed)
		var legs []ledger.Leg
		if net > 0 {
			legs = append(legs, ledger.Leg{Debit: hold, Credit: ledger.PartnerAvailable(d.PartnerID), Amount: net})
		}
		if fee > 0 {
			legs = append(legs, ledger.Leg{Debit: hold, Credit: ledger.Fees, Amount: fee})
		}
		return ledger.Transaction{Key: depositKey(d.ID, "settle"), Legs: legs}, nil
	case aml.RiskHigh:
		return ledger.Transaction{
			Key:  depositKey(d.ID, "refund"),
			Legs: []ledger.Leg{{Debit: hold, Credit: ledger.RefundsPending, Amount: d.Amount}},
		}, nil
	default:
		return ledger.Transaction{}, fmt.Errorf("unknown AML risk %q", d.AmlRisk)
	}
}

type finishBatch struct {
	finish  processingdb.FinishDepositsParams
	refunds processingdb.InsertRefundsParams
}

func (b *finishBatch) add(d processingdb.ClaimSettleableDepositsRow) {
	status := processingdb.DepositStatusCredited
	if aml.Risk(d.AmlRisk) == aml.RiskHigh {
		status = processingdb.DepositStatusRefunded
		b.refunds.DepositIds = append(b.refunds.DepositIds, d.ID)
		b.refunds.ToAddresses = append(b.refunds.ToAddresses, d.FromAddress)
		b.refunds.Amounts = append(b.refunds.Amounts, int64(d.Amount))
	}
	b.finish.Ids = append(b.finish.Ids, d.ID)
	b.finish.Statuses = append(b.finish.Statuses, string(status))
}

func (b *finishBatch) apply(ctx context.Context, q *processingdb.Queries) error {
	if len(b.finish.Ids) == 0 {
		return nil
	}
	n, err := q.FinishDeposits(ctx, b.finish)
	if err != nil {
		return fmt.Errorf("processing: finish deposits: %w", err)
	}
	if int(n) != len(b.finish.Ids) {
		return fmt.Errorf("processing: finished %d of %d deposits", n, len(b.finish.Ids))
	}
	if len(b.refunds.DepositIds) > 0 {
		if err := q.InsertRefunds(ctx, b.refunds); err != nil {
			return fmt.Errorf("processing: create refunds: %w", err)
		}
	}
	return nil
}

type reviewBatch struct {
	params processingdb.FlagForReviewParams
}

func (b *reviewBatch) add(depositID int64, reason error) {
	b.params.Ids = append(b.params.Ids, depositID)
	b.params.Reasons = append(b.params.Reasons, reason.Error())
}

func (b *reviewBatch) apply(ctx context.Context, q *processingdb.Queries) error {
	if len(b.params.Ids) == 0 {
		return nil
	}
	n, err := q.FlagForReview(ctx, b.params)
	if err != nil {
		return fmt.Errorf("processing: flag deposits for review: %w", err)
	}
	if int(n) != len(b.params.Ids) {
		return fmt.Errorf("processing: flagged %d of %d deposits for review", n, len(b.params.Ids))
	}
	return nil
}

func (p *Processor) logFlagged(stage string, b reviewBatch) {
	for i, id := range b.params.Ids {
		p.log.Error("deposit needs manual review", "stage", stage, "deposit_id", id, "reason", b.params.Reasons[i])
	}
}
