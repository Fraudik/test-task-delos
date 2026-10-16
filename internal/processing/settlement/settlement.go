package settlement

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

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

type paidInvoice struct {
	invoice  settlementdb.ClaimPaidInvoicesRow
	deposits []int64
	received money.Amount
	net, fee money.Amount
}

type outcome struct {
	refunded []settlementdb.ClaimRefundableDepositsRow
	paid     []paidInvoice
	review   deposit.Review
}

func (s *Stage) Run(ctx context.Context) (int, error) {
	var (
		claimed int
		out     outcome
	)
	err := pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		out = outcome{}
		q := settlementdb.New(tx)
		refunds, err := q.ClaimRefundableDeposits(ctx, s.cfg.BatchSize)
		if err != nil {
			return fmt.Errorf("settlement: claim refundable deposits: %w", err)
		}
		invoices, err := s.lockPaidInvoices(ctx, q)
		if err != nil {
			return err
		}
		claimed = len(refunds) + len(invoices)
		if claimed == 0 {
			return nil
		}

		txns := make([]ledger.Transaction, 0, claimed)
		for _, r := range refunds {
			txns = append(txns, ledger.Transaction{
				Key:  deposit.LedgerKey(r.ID, "refund"),
				Legs: []ledger.Leg{{Debit: ledger.PartnerHold(r.PartnerID), Credit: ledger.RefundPending, Amount: r.Amount}},
			})
		}
		for _, p := range invoices {
			txns = append(txns, settleTxn(p))
		}
		results, err := ledger.PostBatch(ctx, tx, txns...)
		if err != nil {
			return fmt.Errorf("settlement: post to ledger: %w", err)
		}
		for i, r := range refunds {
			if err := results[i].Err; err != nil {
				out.review.Add(r.ID, err)
				continue
			}
			out.refunded = append(out.refunded, r)
		}
		for i, p := range invoices {
			if err := results[len(refunds)+i].Err; err != nil {
				for _, id := range p.deposits {
					out.review.Add(id, fmt.Errorf("invoice %s settlement: %w", p.invoice.ID, err))
				}
				continue
			}
			out.paid = append(out.paid, p)
		}
		return out.apply(ctx, tx, q)
	})
	if err != nil {
		return 0, err
	}
	for _, r := range out.refunded {
		s.log.Info("deposit refunded", "deposit_id", r.ID, "amount", r.Amount.String())
	}
	for _, p := range out.paid {
		s.log.Info("invoice paid, funds available", "invoice_id", p.invoice.ID, "deposits", p.deposits,
			"received", p.received.String(), "net", p.net.String(), "fee", p.fee.String())
	}
	out.review.Log(s.log, "settlement")
	return claimed, nil
}

func (s *Stage) lockPaidInvoices(ctx context.Context, q *settlementdb.Queries) ([]paidInvoice, error) {
	invoices, err := q.ClaimPaidInvoices(ctx, s.cfg.BatchSize)
	if err != nil {
		return nil, fmt.Errorf("settlement: claim paid invoices: %w", err)
	}
	if len(invoices) == 0 {
		return nil, nil
	}
	ids := make([]uuid.UUID, len(invoices))
	for i, inv := range invoices {
		ids[i] = inv.ID
	}
	rows, err := q.LockCleanDeposits(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("settlement: lock invoice deposits: %w", err)
	}
	byInvoice := make(map[uuid.UUID]*paidInvoice, len(invoices))
	paid := make([]paidInvoice, len(invoices))
	for i, inv := range invoices {
		paid[i] = paidInvoice{invoice: inv}
		byInvoice[inv.ID] = &paid[i]
	}
	for _, d := range rows {
		p := byInvoice[d.InvoiceID]
		p.deposits = append(p.deposits, d.ID)
		p.received += d.Amount
	}
	ready := paid[:0]
	for _, p := range paid {
		if p.received < p.invoice.Amount {
			continue
		}
		p.net, p.fee = splitFee(p.received, p.invoice.FeeBps, p.invoice.FeeFixed)
		ready = append(ready, p)
	}
	return ready, nil
}

func settleTxn(p paidInvoice) ledger.Transaction {
	hold := ledger.PartnerHold(p.invoice.PartnerID)
	var legs []ledger.Leg
	if p.net > 0 {
		legs = append(legs, ledger.Leg{Debit: hold, Credit: ledger.PartnerAvailable(p.invoice.PartnerID), Amount: p.net})
	}
	if p.fee > 0 {
		legs = append(legs, ledger.Leg{Debit: hold, Credit: ledger.Fees, Amount: p.fee})
	}
	return ledger.Transaction{Key: deposit.InvoiceLedgerKey(p.invoice.ID, "settle"), Legs: legs}
}

func (o *outcome) apply(ctx context.Context, tx pgx.Tx, q *settlementdb.Queries) error {
	if len(o.refunded) > 0 {
		ids := make([]int64, len(o.refunded))
		refunds := settlementdb.InsertRefundsParams{}
		for i, r := range o.refunded {
			ids[i] = r.ID
			refunds.DepositIds = append(refunds.DepositIds, r.ID)
			refunds.ToAddresses = append(refunds.ToAddresses, r.FromAddress)
			refunds.Amounts = append(refunds.Amounts, int64(r.Amount))
		}
		if err := finish(ctx, q, settlementdb.DepositStatusRefunded, ids); err != nil {
			return err
		}
		if err := q.InsertRefunds(ctx, refunds); err != nil {
			return fmt.Errorf("settlement: create refunds: %w", err)
		}
	}
	if len(o.paid) > 0 {
		var (
			depositIDs []int64
			invoiceIDs []uuid.UUID
		)
		for _, p := range o.paid {
			depositIDs = append(depositIDs, p.deposits...)
			invoiceIDs = append(invoiceIDs, p.invoice.ID)
		}
		if err := finish(ctx, q, settlementdb.DepositStatusCredited, depositIDs); err != nil {
			return err
		}
		n, err := q.MarkInvoicesPaid(ctx, invoiceIDs)
		if err != nil {
			return fmt.Errorf("settlement: mark invoices paid: %w", err)
		}
		if int(n) != len(invoiceIDs) {
			return fmt.Errorf("settlement: marked %d of %d invoices paid", n, len(invoiceIDs))
		}
	}
	return o.review.Apply(ctx, tx)
}

func finish(ctx context.Context, q *settlementdb.Queries, status settlementdb.DepositStatus, ids []int64) error {
	n, err := q.FinishDeposits(ctx, settlementdb.FinishDepositsParams{Status: status, Ids: ids})
	if err != nil {
		return fmt.Errorf("settlement: mark deposits %s: %w", status, err)
	}
	if int(n) != len(ids) {
		return fmt.Errorf("settlement: marked %d of %d deposits %s", n, len(ids), status)
	}
	return nil
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
