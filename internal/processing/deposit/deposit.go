package deposit

import (
	"context"
	"fmt"
	"log/slog"

	"test-task-dellos/internal/processing/deposit/depositdb"
)

const CursorName = "usdt"

func LedgerKey(depositID int64, step string) string {
	return fmt.Sprintf("deposit:%d:%s", depositID, step)
}

type Review struct {
	params depositdb.FlagForReviewParams
}

func (r *Review) Add(depositID int64, reason error) {
	r.params.Ids = append(r.params.Ids, depositID)
	r.params.Reasons = append(r.params.Reasons, reason.Error())
}

func (r *Review) Apply(ctx context.Context, db depositdb.DBTX) error {
	if len(r.params.Ids) == 0 {
		return nil
	}
	n, err := depositdb.New(db).FlagForReview(ctx, r.params)
	if err != nil {
		return fmt.Errorf("deposit: flag for review: %w", err)
	}
	if int(n) != len(r.params.Ids) {
		return fmt.Errorf("deposit: flagged %d of %d deposits for review", n, len(r.params.Ids))
	}
	return nil
}

func (r *Review) Log(log *slog.Logger, stage string) {
	for i, id := range r.params.Ids {
		log.Error("deposit needs manual review", "stage", stage, "deposit_id", id, "reason", r.params.Reasons[i])
	}
}

func InvoiceLedgerKey(invoiceID int64, step string) string {
	return fmt.Sprintf("invoice:%d:%s", invoiceID, step)
}
