package confirmations

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"test-task-dellos/internal/ledger"
	"test-task-dellos/internal/processing/confirmations/confirmationsdb"
	"test-task-dellos/internal/processing/deposit"
)

type Config struct {
	Confirmations uint64
	BatchSize     int32
}

type Stage struct {
	db          *pgxpool.Pool
	cfg         Config
	log         *slog.Logger
	onConfirmed func()
}

func New(db *pgxpool.Pool, cfg Config, log *slog.Logger, onConfirmed func()) *Stage {
	return &Stage{db: db, cfg: cfg, log: log.With("stage", "confirmations"), onConfirmed: onConfirmed}
}

func (s *Stage) Run(ctx context.Context) (int, error) {
	var (
		claimed   int
		confirmed []int64
		review    deposit.Review
	)
	err := pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		q := confirmationsdb.New(tx)
		rows, err := q.ClaimReadyDeposits(ctx, confirmationsdb.ClaimReadyDepositsParams{
			CursorName:    deposit.CursorName,
			Confirmations: int64(s.cfg.Confirmations),
			MaxRows:       s.cfg.BatchSize,
		})
		if err != nil {
			return fmt.Errorf("confirmations: claim ready deposits: %w", err)
		}
		claimed = len(rows)
		if claimed == 0 {
			return nil
		}

		txns := make([]ledger.Transaction, len(rows))
		for i, r := range rows {
			txns[i] = ledger.Transaction{
				Key:  deposit.LedgerKey(r.ID, "hold"),
				Legs: []ledger.Leg{{Debit: ledger.Custody, Credit: ledger.PartnerHold(r.PartnerID), Amount: r.Amount}},
			}
		}
		results, err := ledger.PostBatch(ctx, tx, txns...)
		if err != nil {
			return fmt.Errorf("confirmations: post holds: %w", err)
		}
		for i, res := range results {
			if res.Err != nil {
				review.Add(rows[i].ID, res.Err)
				continue
			}
			confirmed = append(confirmed, rows[i].ID)
		}

		if len(confirmed) > 0 {
			n, err := q.ConfirmDeposits(ctx, confirmed)
			if err != nil {
				return fmt.Errorf("confirmations: confirm deposits: %w", err)
			}
			if int(n) != len(confirmed) {
				return fmt.Errorf("confirmations: confirmed %d of %d deposits", n, len(confirmed))
			}
		}
		return review.Apply(ctx, tx)
	})
	if err != nil {
		return 0, err
	}
	for _, id := range confirmed {
		s.log.Info("deposit confirmed, funds on hold", "deposit_id", id, "confirmations", s.cfg.Confirmations)
	}
	review.Log(s.log, "confirmations")
	if len(confirmed) > 0 && s.onConfirmed != nil {
		s.onConfirmed()
	}
	return claimed, nil
}
