package settlement

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"test-task-dellos/internal/aml"
	"test-task-dellos/internal/ledger"
	"test-task-dellos/internal/money"
	"test-task-dellos/internal/processing/deposit"
	"test-task-dellos/internal/processing/settlement/settlementdb"
)

type Config struct {
	BatchSize int32
}

type Stage struct {
	db  *pgxpool.Pool
	cfg Config
	log *slog.Logger
}

func New(db *pgxpool.Pool, cfg Config, log *slog.Logger) *Stage {
	return &Stage{db: db, cfg: cfg, log: log.With("stage", "settlement")}
}

func (s *Stage) Run(ctx context.Context) (int, error) {
	var (
		claimed int
		done    finishBatch
		review  deposit.Review
	)
	err := pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		q := settlementdb.New(tx)
		rows, err := q.ClaimSettleableDeposits(ctx, s.cfg.BatchSize)
		if err != nil {
			return fmt.Errorf("settlement: claim settleable deposits: %w", err)
		}
		claimed = len(rows)
		if claimed == 0 {
			return nil
		}

		var (
			txns    []ledger.Transaction
			planned []settlementdb.ClaimSettleableDepositsRow
		)
		for _, r := range rows {
			t, err := settlementTxn(r)
			if err != nil {
				review.Add(r.ID, err)
				continue
			}
			txns = append(txns, t)
			planned = append(planned, r)
		}
		if len(txns) > 0 {
			results, err := ledger.PostBatch(ctx, tx, txns...)
			if err != nil {
				return fmt.Errorf("settlement: post settlements: %w", err)
			}
			for i, res := range results {
				if res.Err != nil {
					review.Add(planned[i].ID, res.Err)
					continue
				}
				done.add(planned[i])
			}
		}

		if err := done.apply(ctx, q); err != nil {
			return err
		}
		return review.Apply(ctx, tx)
	})
	if err != nil {
		return 0, err
	}
	for i, id := range done.finish.Ids {
		s.log.Info("deposit processed", "deposit_id", id, "status", done.finish.Statuses[i])
	}
	review.Log(s.log, "settlement")
	return claimed, nil
}

func settlementTxn(d settlementdb.ClaimSettleableDepositsRow) (ledger.Transaction, error) {
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
		return ledger.Transaction{Key: deposit.LedgerKey(d.ID, "settle"), Legs: legs}, nil
	case aml.RiskHigh:
		return ledger.Transaction{
			Key:  deposit.LedgerKey(d.ID, "refund"),
			Legs: []ledger.Leg{{Debit: hold, Credit: ledger.RefundPending, Amount: d.Amount}},
		}, nil
	default:
		return ledger.Transaction{}, fmt.Errorf("unknown AML risk %q", d.AmlRisk)
	}
}

func splitFee(amount money.Amount, feeBps int32, feeFixed money.Amount) (net, fee money.Amount) {
	const scale = 10_000
	bps := money.Amount(feeBps)
	fee = amount/scale*bps + amount%scale*bps/scale
	if feeFixed >= amount-fee {
		return 0, amount
	}
	fee += feeFixed
	return amount - fee, fee
}

type finishBatch struct {
	finish  settlementdb.FinishDepositsParams
	refunds settlementdb.InsertRefundsParams
}

func (b *finishBatch) add(d settlementdb.ClaimSettleableDepositsRow) {
	status := settlementdb.DepositStatusCredited
	if aml.Risk(d.AmlRisk) == aml.RiskHigh {
		status = settlementdb.DepositStatusRefunded
		b.refunds.DepositIds = append(b.refunds.DepositIds, d.ID)
		b.refunds.ToAddresses = append(b.refunds.ToAddresses, d.FromAddress)
		b.refunds.Amounts = append(b.refunds.Amounts, int64(d.Amount))
	}
	b.finish.Ids = append(b.finish.Ids, d.ID)
	b.finish.Statuses = append(b.finish.Statuses, string(status))
}

func (b *finishBatch) apply(ctx context.Context, q *settlementdb.Queries) error {
	if len(b.finish.Ids) == 0 {
		return nil
	}
	n, err := q.FinishDeposits(ctx, b.finish)
	if err != nil {
		return fmt.Errorf("settlement: finish deposits: %w", err)
	}
	if int(n) != len(b.finish.Ids) {
		return fmt.Errorf("settlement: finished %d of %d deposits", n, len(b.finish.Ids))
	}
	if len(b.refunds.DepositIds) > 0 {
		if err := q.InsertRefunds(ctx, b.refunds); err != nil {
			return fmt.Errorf("settlement: create refunds: %w", err)
		}
	}
	return nil
}
