package notify_test

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"test-task-dellos/internal/notify"
	"test-task-dellos/internal/testdb"
)

type recorder struct {
	mu    sync.Mutex
	calls map[int64]int
	fail  bool
}

func (r *recorder) NotifyPartner(_ context.Context, n notify.Notification) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.fail {
		return errors.New("partner endpoint is down")
	}
	r.calls[n.DepositID]++
	return nil
}

func (r *recorder) setFail(fail bool) {
	r.mu.Lock()
	r.fail = fail
	r.mu.Unlock()
}

func newNotifier(t *testing.T, db *pgxpool.Pool, rec *recorder) *notify.Notifier {
	cfg := notify.DefaultConfig()
	cfg.BackoffBase = time.Hour
	cfg.BackoffMax = time.Hour
	return notify.New(db, rec, cfg, slog.New(slog.NewTextHandler(t.Output(), nil)))
}

func tick(t *testing.T, n *notify.Notifier) {
	t.Helper()
	if _, err := n.Tick(t.Context()); err != nil {
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

func seedDeposit(t *testing.T, db *pgxpool.Pool, status string) int64 {
	t.Helper()
	pid := scalar(t, db, `INSERT INTO partners (name, fee_bps) VALUES ('p-' || gen_random_uuid(), 0) RETURNING id`)
	inv := scalar(t, db, `INSERT INTO invoices (partner_id, address) VALUES ($1, '0x' || md5(random()::text)) RETURNING id`, pid)
	return scalar(t, db, `
		INSERT INTO deposits (invoice_id, partner_id, tx_hash, log_index, block_number, block_hash, from_address, to_address, amount, fee_bps, status)
		VALUES ($1, $2, '0x' || md5(random()::text), 0, 1, '0xblock', '0xfrom', '0xto', 1000000, 0, $3::deposit_status)
		RETURNING id`, inv, pid, status)
}

func TestNotifiesCreditedDepositsOnce(t *testing.T) {
	db := testdb.New(t)
	rec := &recorder{calls: map[int64]int{}}
	n := newNotifier(t, db, rec)
	credited := seedDeposit(t, db, "credited")
	pending := seedDeposit(t, db, "pending")
	seedDeposit(t, db, "refunded")

	for range 3 {
		tick(t, n)
	}
	if rec.calls[credited] != 1 || len(rec.calls) != 1 {
		t.Errorf("notifications = %v, want only deposit %d once", rec.calls, credited)
	}
	if c := scalar(t, db, `SELECT count(*) FROM deposits WHERE id = $1 AND notified_at IS NOT NULL`, credited); c != 1 {
		t.Error("credited deposit not marked as notified")
	}

	exec(t, db, `INSERT INTO deposit_aml_checks (deposit_id, risk, next_attempt_at, checked_at) VALUES ($1, 'low', NULL, now())`, pending)
	exec(t, db, `UPDATE deposits SET status = 'credited' WHERE id = $1`, pending)
	tick(t, n)
	if rec.calls[pending] != 1 || rec.calls[credited] != 1 {
		t.Errorf("notifications = %v, want each deposit once", rec.calls)
	}
}

func TestFailedNotificationsAreRetriedWithBackoff(t *testing.T) {
	db := testdb.New(t)
	rec := &recorder{calls: map[int64]int{}}
	n := newNotifier(t, db, rec)
	credited := seedDeposit(t, db, "credited")

	rec.setFail(true)
	tick(t, n)
	if c := scalar(t, db, `SELECT notify_attempts FROM deposits WHERE id = $1 AND notify_next_attempt_at > now() + interval '59 minutes'`, credited); c != 1 {
		t.Errorf("notify attempts = %d, want 1 with backoff", c)
	}

	rec.setFail(false)
	tick(t, n)
	if len(rec.calls) != 0 {
		t.Error("notification retried before backoff expired")
	}

	exec(t, db, `UPDATE deposits SET notify_next_attempt_at = now()`)
	tick(t, n)
	if rec.calls[credited] != 1 {
		t.Errorf("after recovery notifications = %v, want one call", rec.calls)
	}
}

func TestConcurrentNotifiersCallOnce(t *testing.T) {
	db := testdb.New(t)
	rec := &recorder{calls: map[int64]int{}}
	var deposits []int64
	for range 10 {
		deposits = append(deposits, seedDeposit(t, db, "credited"))
	}

	var wg sync.WaitGroup
	for range 4 {
		n := newNotifier(t, db, rec)
		wg.Go(func() {
			for range 3 {
				if _, err := n.Tick(context.Background()); err != nil {
					t.Error(err)
				}
			}
		})
	}
	wg.Wait()

	for _, id := range deposits {
		if rec.calls[id] != 1 {
			t.Errorf("deposit %d notified %d times, want 1", id, rec.calls[id])
		}
	}
}
