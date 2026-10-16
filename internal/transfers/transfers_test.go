package transfers_test

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"test-task-dellos/internal/testdb"
	"test-task-dellos/internal/transfers"
)

type recorder struct {
	mu    sync.Mutex
	calls map[int64]int
	fail  bool
}

func (r *recorder) InitiateRefund(_ context.Context, req transfers.RefundRequest) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.fail {
		return errors.New("wallet is down")
	}
	r.calls[req.RefundID]++
	return nil
}

func (r *recorder) setFail(fail bool) {
	r.mu.Lock()
	r.fail = fail
	r.mu.Unlock()
}

func newTransfers(t *testing.T, db *pgxpool.Pool, rec *recorder) *transfers.Transfers {
	cfg := transfers.DefaultConfig()
	cfg.BackoffBase = time.Hour
	cfg.BackoffMax = time.Hour
	return transfers.New(db, rec, cfg, slog.New(slog.NewTextHandler(t.Output(), nil)))
}

func tick(t *testing.T, tr *transfers.Transfers) {
	t.Helper()
	if _, err := tr.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func exec(t *testing.T, db *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := db.Exec(t.Context(), sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

func scalar(t *testing.T, db *pgxpool.Pool, sql string, args ...any) int64 {
	t.Helper()
	var n int64
	if err := db.QueryRow(t.Context(), sql, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return n
}

func seedRefund(t *testing.T, db *pgxpool.Pool) int64 {
	t.Helper()
	pid := scalar(t, db, `INSERT INTO partners (name, fee_bps) VALUES ('p-' || gen_random_uuid(), 0) RETURNING id`)
	inv := scalar(t, db, `INSERT INTO invoices (partner_id, address, amount, fee_bps, fee_fixed) VALUES ($1, '0x' || md5(random()::text), 1000000, 0, 0) RETURNING id`, pid)
	dep := scalar(t, db, `
		INSERT INTO deposits (invoice_id, partner_id, tx_hash, log_index, block_number, block_hash, from_address, to_address, amount, status)
		VALUES ($1, $2, '0x' || md5(random()::text), 0, 1, '0xblock', '0xfrom', '0xto', 1000000, 'refunded')
		RETURNING id`, inv, pid)
	return scalar(t, db, `INSERT INTO refunds (deposit_id, to_address, amount) VALUES ($1, '0xfrom', 1000000) RETURNING id`, dep)
}

func refundStatus(t *testing.T, db *pgxpool.Pool, id int64) string {
	t.Helper()
	var status string
	if err := db.QueryRow(t.Context(), `SELECT status::text FROM refunds WHERE id = $1`, id).Scan(&status); err != nil {
		t.Fatal(err)
	}
	return status
}

func TestInitiatesRefundsOnce(t *testing.T) {
	db := testdb.New(t)
	rec := &recorder{calls: map[int64]int{}}
	tr := newTransfers(t, db, rec)
	refund := seedRefund(t, db)

	for range 3 {
		tick(t, tr)
	}
	if rec.calls[refund] != 1 || len(rec.calls) != 1 {
		t.Errorf("refunds = %v, want refund %d once", rec.calls, refund)
	}
	if s := refundStatus(t, db, refund); s != "initiated" {
		t.Errorf("refund status = %s, want initiated", s)
	}
}

func TestFailedRefundsAreRetriedWithBackoff(t *testing.T) {
	db := testdb.New(t)
	rec := &recorder{calls: map[int64]int{}}
	tr := newTransfers(t, db, rec)
	refund := seedRefund(t, db)

	rec.setFail(true)
	tick(t, tr)
	if n := scalar(t, db, `SELECT initiate_attempts FROM refunds WHERE id = $1 AND initiate_next_attempt_at > now() + interval '59 minutes'`, refund); n != 1 {
		t.Errorf("refund attempts = %d, want 1 with backoff", n)
	}
	if s := refundStatus(t, db, refund); s != "ready_to_send" {
		t.Errorf("refund status after failure = %s, want ready_to_send", s)
	}

	rec.setFail(false)
	tick(t, tr)
	if len(rec.calls) != 0 {
		t.Error("refund retried before backoff expired")
	}

	exec(t, db, `UPDATE refunds SET initiate_next_attempt_at = now()`)
	tick(t, tr)
	if rec.calls[refund] != 1 {
		t.Errorf("after recovery refunds = %v, want one call", rec.calls)
	}
}

func TestConcurrentTransfersInitiateOnce(t *testing.T) {
	db := testdb.New(t)
	rec := &recorder{calls: map[int64]int{}}
	var refunds []int64
	for range 10 {
		refunds = append(refunds, seedRefund(t, db))
	}

	var wg sync.WaitGroup
	for range 4 {
		tr := newTransfers(t, db, rec)
		wg.Go(func() {
			for range 3 {
				if _, err := tr.Tick(context.Background()); err != nil {
					t.Error(err)
				}
			}
		})
	}
	wg.Wait()

	for _, id := range refunds {
		if rec.calls[id] != 1 {
			t.Errorf("refund %d initiated %d times, want 1", id, rec.calls[id])
		}
	}
	if n := scalar(t, db, `SELECT count(*) FROM refunds WHERE status <> 'initiated'`); n != 0 {
		t.Errorf("%d refunds not initiated", n)
	}
}
