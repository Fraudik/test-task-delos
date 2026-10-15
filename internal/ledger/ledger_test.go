package ledger_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"test-task-dellos/internal/ledger"
	"test-task-dellos/internal/money"
	"test-task-dellos/internal/testdb"
)

const partner = 1

func post(t *testing.T, db ledger.DB, key string, legs ...ledger.Leg) bool {
	t.Helper()
	applied, err := ledger.Post(t.Context(), db, key, legs...)
	if err != nil {
		t.Fatalf("Post(%s): %v", key, err)
	}
	return applied
}

func deposit(amount money.Amount) ledger.Leg {
	return ledger.Leg{Debit: ledger.Custody, Credit: ledger.PartnerHold(partner), Amount: amount}
}

func assertBalances(t *testing.T, db ledger.DB, want map[ledger.Account]money.Amount) {
	t.Helper()
	for a, w := range want {
		got, err := ledger.Balance(t.Context(), db, a)
		if err != nil {
			t.Fatalf("Balance(%s): %v", a.ID, err)
		}
		if got != w {
			t.Errorf("balance %s = %d, want %d", a.ID, got, w)
		}
	}
	if err := ledger.CheckIntegrity(t.Context(), db); err != nil {
		t.Error(err)
	}
}

func count(t *testing.T, db *pgxpool.Pool, table string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(t.Context(), "SELECT count(*) FROM "+pgx.Identifier{table}.Sanitize()).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

func TestKind(t *testing.T) {
	tests := []struct {
		kind  ledger.Kind
		name  string
		valid bool
	}{
		{ledger.Asset, "asset", true},
		{ledger.Liability, "liability", true},
		{ledger.Revenue, "revenue", true},
		{0, "Kind(0)", false},
		{ledger.Revenue + 1, "Kind(4)", false},
		{ledger.Kind(255), "Kind(255)", false},
	}
	for _, tt := range tests {
		if got := tt.kind.String(); got != tt.name {
			t.Errorf("Kind(%d).String() = %q, want %q", uint8(tt.kind), got, tt.name)
		}
		if got := tt.kind.Valid(); got != tt.valid {
			t.Errorf("Kind(%d).Valid() = %v, want %v", uint8(tt.kind), got, tt.valid)
		}
	}
}

func TestDepositSettleAndRefund(t *testing.T) {
	db := testdb.New(t)
	hold, available := ledger.PartnerHold(partner), ledger.PartnerAvailable(partner)

	post(t, db, "deposit:1:confirm", deposit(100*money.OneUSDT))
	post(t, db, "deposit:2:confirm", deposit(50*money.OneUSDT))
	assertBalances(t, db, map[ledger.Account]money.Amount{
		ledger.Custody: 150 * money.OneUSDT,
		hold:           150 * money.OneUSDT,
		available:      0,
	})

	post(t, db, "deposit:1:settle",
		ledger.Leg{Debit: hold, Credit: available, Amount: 98_500_000},
		ledger.Leg{Debit: hold, Credit: ledger.Fees, Amount: 1_500_000},
	)
	post(t, db, "deposit:2:refund", ledger.Leg{Debit: hold, Credit: ledger.RefundsPending, Amount: 50 * money.OneUSDT})

	assertBalances(t, db, map[ledger.Account]money.Amount{
		ledger.Custody:        150 * money.OneUSDT,
		hold:                  0,
		available:             98_500_000,
		ledger.Fees:           1_500_000,
		ledger.RefundsPending: 50 * money.OneUSDT,
	})
}

func TestPostIsIdempotent(t *testing.T) {
	db := testdb.New(t)

	if !post(t, db, "deposit:1:confirm", deposit(10)) {
		t.Fatal("first Post not applied")
	}
	if post(t, db, "deposit:1:confirm", deposit(10)) {
		t.Error("second Post with the same key applied again")
	}
	assertBalances(t, db, map[ledger.Account]money.Amount{ledger.PartnerHold(partner): 10})

	_, err := ledger.Post(t.Context(), db, "deposit:1:confirm", deposit(11))
	if !errors.Is(err, ledger.ErrIdempotencyConflict) {
		t.Errorf("Post with same key and other legs: error = %v, want ErrIdempotencyConflict", err)
	}
	assertBalances(t, db, map[ledger.Account]money.Amount{ledger.PartnerHold(partner): 10})

	legs := []ledger.Leg{
		{Debit: ledger.Custody, Credit: ledger.PartnerHold(1), Amount: 1},
		{Debit: ledger.Custody, Credit: ledger.PartnerHold(2), Amount: 2},
	}
	post(t, db, "batch", legs...)
	if post(t, db, "batch", legs[1], legs[0]) {
		t.Error("same legs in other order applied again")
	}
}

func TestPostValidation(t *testing.T) {
	db := testdb.New(t)
	hold := ledger.PartnerHold(partner)

	tests := []struct {
		name string
		key  string
		legs []ledger.Leg
		want error
	}{
		{"empty key", "", []ledger.Leg{deposit(1)}, ledger.ErrEmptyKey},
		{"no legs", "k", nil, ledger.ErrNoLegs},
		{"zero amount", "k", []ledger.Leg{deposit(0)}, ledger.ErrInvalidLeg},
		{"negative amount", "k", []ledger.Leg{deposit(-1)}, ledger.ErrInvalidLeg},
		{"same account", "k", []ledger.Leg{{Debit: hold, Credit: hold, Amount: 1}}, ledger.ErrInvalidLeg},
		{"empty account", "k", []ledger.Leg{{Debit: ledger.Account{Kind: ledger.Asset}, Credit: hold, Amount: 1}}, ledger.ErrInvalidLeg},
		{"zero kind", "k", []ledger.Leg{{Debit: ledger.Account{ID: "x"}, Credit: hold, Amount: 1}}, ledger.ErrInvalidLeg},
		{"out of range kind", "k", []ledger.Leg{{Debit: hold, Credit: ledger.Account{ID: "x", Kind: ledger.Kind(42)}, Amount: 1}}, ledger.ErrInvalidLeg},
		{"one bad leg of many", "k", []ledger.Leg{deposit(1), deposit(0)}, ledger.ErrInvalidLeg},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := ledger.Post(t.Context(), db, tt.key, tt.legs...); !errors.Is(err, tt.want) {
				t.Errorf("error = %v, want %v", err, tt.want)
			}
		})
	}
	if n := count(t, db, "ledger_transactions"); n != 0 {
		t.Errorf("%d transactions written for invalid input", n)
	}
}

func TestInsufficientFunds(t *testing.T) {
	db := testdb.New(t)
	hold, available := ledger.PartnerHold(partner), ledger.PartnerAvailable(partner)
	post(t, db, "deposit:1:confirm", deposit(100))

	settle := []ledger.Leg{
		{Debit: hold, Credit: available, Amount: 90},
		{Debit: hold, Credit: ledger.Fees, Amount: 20},
	}
	_, err := ledger.Post(t.Context(), db, "deposit:1:settle", settle...)
	if !errors.Is(err, ledger.ErrInsufficientFunds) {
		t.Fatalf("error = %v, want ErrInsufficientFunds", err)
	}
	assertBalances(t, db, map[ledger.Account]money.Amount{hold: 100, available: 0, ledger.Fees: 0})

	settle[1].Amount = 10
	if !post(t, db, "deposit:1:settle", settle...) {
		t.Error("key was consumed by the failed Post")
	}
	assertBalances(t, db, map[ledger.Account]money.Amount{hold: 0, available: 90, ledger.Fees: 10})

	_, err = ledger.Post(t.Context(), db, "withdraw", ledger.Leg{Debit: ledger.PartnerHold(2), Credit: ledger.Custody, Amount: 1})
	if !errors.Is(err, ledger.ErrInsufficientFunds) {
		t.Errorf("debit from new account: error = %v, want ErrInsufficientFunds", err)
	}
}

func TestPostJoinsOuterTransaction(t *testing.T) {
	db := testdb.New(t)
	hold := ledger.PartnerHold(partner)

	tx, err := db.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	post(t, tx, "deposit:1:confirm", deposit(100))
	if err := tx.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
	assertBalances(t, db, map[ledger.Account]money.Amount{hold: 0})

	err = pgx.BeginFunc(t.Context(), db, func(tx pgx.Tx) error {
		_, err := ledger.Post(t.Context(), tx, "deposit:1:confirm", deposit(100))
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	assertBalances(t, db, map[ledger.Account]money.Amount{hold: 100})
}

func TestBalanceOfUnknownAccount(t *testing.T) {
	db := testdb.New(t)
	got, err := ledger.Balance(t.Context(), db, ledger.PartnerAvailable(42))
	if err != nil || got != 0 {
		t.Errorf("Balance = %d, %v; want 0, nil", got, err)
	}
	if err := ledger.CheckIntegrity(t.Context(), db); err != nil {
		t.Errorf("empty ledger: %v", err)
	}
}

func TestConcurrentPostSameKey(t *testing.T) {
	db := testdb.New(t)
	var applied atomic.Int32
	var wg sync.WaitGroup

	for range 20 {
		wg.Go(func() {
			ok, err := ledger.Post(context.Background(), db, "deposit:1:confirm", deposit(100))
			if err != nil {
				t.Error(err)
			}
			if ok {
				applied.Add(1)
			}
		})
	}
	wg.Wait()

	if n := applied.Load(); n != 1 {
		t.Errorf("applied %d times, want 1", n)
	}
	assertBalances(t, db, map[ledger.Account]money.Amount{ledger.PartnerHold(partner): 100})
}

func TestConcurrentCrossPostsDoNotDeadlock(t *testing.T) {
	db := testdb.New(t)
	a, b := ledger.PartnerAvailable(1), ledger.PartnerAvailable(2)
	post(t, db, "seed:a", ledger.Leg{Debit: ledger.Custody, Credit: a, Amount: 1000})
	post(t, db, "seed:b", ledger.Leg{Debit: ledger.Custody, Credit: b, Amount: 1000})

	var wg sync.WaitGroup
	for i := range 50 {
		wg.Go(func() {
			from, to := a, b
			if i%2 == 1 {
				from, to = b, a
			}
			_, err := ledger.Post(context.Background(), db, fmt.Sprintf("move:%d", i),
				ledger.Leg{Debit: from, Credit: to, Amount: 10},
				ledger.Leg{Debit: ledger.Custody, Credit: from, Amount: 1},
			)
			if err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()

	assertBalances(t, db, map[ledger.Account]money.Amount{a: 1025, b: 1025, ledger.Custody: 2050})
}

func TestDatabaseGuards(t *testing.T) {
	db := testdb.New(t)
	post(t, db, "deposit:1:confirm", deposit(100))

	t.Run("append only", func(t *testing.T) {
		for _, q := range []string{
			`UPDATE ledger_entries SET amount = amount * 2`,
			`DELETE FROM ledger_entries`,
			`UPDATE ledger_transactions SET idempotency_key = 'x'`,
			`DELETE FROM ledger_transactions`,
		} {
			if _, err := db.Exec(t.Context(), q); err == nil {
				t.Errorf("%q succeeded", q)
			}
		}
	})

	t.Run("unbalanced transaction rejected on commit", func(t *testing.T) {
		err := pgx.BeginFunc(t.Context(), db, func(tx pgx.Tx) error {
			var id int64
			if err := tx.QueryRow(t.Context(),
				`INSERT INTO ledger_transactions (idempotency_key, digest) VALUES ('raw', '') RETURNING id`).Scan(&id); err != nil {
				return err
			}
			_, err := tx.Exec(t.Context(), `INSERT INTO ledger_entries (transaction_id, account_id, amount) VALUES ($1, 'custody', 5)`, id)
			return err
		})
		if err == nil {
			t.Error("unbalanced transaction committed")
		}
	})

	assertBalances(t, db, map[ledger.Account]money.Amount{ledger.Custody: 100, ledger.PartnerHold(partner): 100})
}

func TestAccountKindMismatch(t *testing.T) {
	db := testdb.New(t)
	hold := ledger.PartnerHold(partner)
	post(t, db, "deposit:1:confirm", deposit(100))

	wrong := ledger.Account{ID: hold.ID, Kind: ledger.Asset}
	_, err := ledger.Post(t.Context(), db, "bad", ledger.Leg{Debit: wrong, Credit: ledger.Fees, Amount: 10})
	if !errors.Is(err, ledger.ErrAccountKindMismatch) {
		t.Errorf("error = %v, want ErrAccountKindMismatch", err)
	}
	if n := count(t, db, "ledger_transactions"); n != 1 {
		t.Errorf("%d transactions after rejected Post, want 1", n)
	}
	assertBalances(t, db, map[ledger.Account]money.Amount{hold: 100, ledger.Fees: 0})
}
