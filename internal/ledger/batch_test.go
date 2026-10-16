package ledger_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"test-task-dellos/internal/ledger"
	"test-task-dellos/internal/money"
	"test-task-dellos/internal/testdb"
)

func postBatch(t *testing.T, db ledger.DB, txns ...ledger.Transaction) []ledger.Result {
	t.Helper()
	results, err := ledger.PostBatch(t.Context(), db, txns...)
	if err != nil {
		t.Fatalf("PostBatch: %v", err)
	}
	return results
}

func depositTxn(key string, amount money.Amount) ledger.Transaction {
	return ledger.Transaction{Key: key, Legs: []ledger.Leg{deposit(amount)}}
}

func assertResults(t *testing.T, got []ledger.Result, want ...any) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%d results, want %d", len(got), len(want))
	}
	for i, w := range want {
		switch w := w.(type) {
		case bool:
			if got[i].Err != nil || got[i].Applied != w {
				t.Errorf("result %d = %+v, want applied=%v", i, got[i], w)
			}
		case error:
			if got[i].Applied || !errors.Is(got[i].Err, w) {
				t.Errorf("result %d = %+v, want error %v", i, got[i], w)
			}
		}
	}
}

func TestPostBatchAppliesAllTransactions(t *testing.T) {
	db := testdb.New(t)
	hold, available := ledger.PartnerHold(partner), ledger.PartnerAvailable(partner)

	results := postBatch(t, db,
		depositTxn("deposit:1:hold", 100),
		depositTxn("deposit:2:hold", 50),
		ledger.Transaction{Key: "deposit:1:settle", Legs: []ledger.Leg{
			{Debit: hold, Credit: available, Amount: 90},
			{Debit: hold, Credit: ledger.Fees, Amount: 10},
		}},
	)
	assertResults(t, results, true, true, true)
	if n := count(t, db, "ledger_transactions"); n != 3 {
		t.Errorf("%d ledger transactions, want 3", n)
	}
	assertBalances(t, db, map[ledger.Account]money.Amount{
		ledger.Custody: 150,
		hold:           50,
		available:      90,
		ledger.Fees:    10,
	})
}

func TestPostBatchIsIdempotentPerTransaction(t *testing.T) {
	db := testdb.New(t)
	hold := ledger.PartnerHold(partner)
	post(t, db, "deposit:1:hold", deposit(100))

	assertResults(t, postBatch(t, db, depositTxn("deposit:1:hold", 100), depositTxn("deposit:2:hold", 30)), false, true)
	assertBalances(t, db, map[ledger.Account]money.Amount{hold: 130})

	results := postBatch(t, db, depositTxn("deposit:3:hold", 5), depositTxn("deposit:1:hold", 99))
	assertResults(t, results, true, ledger.ErrIdempotencyConflict)
	if n := count(t, db, "ledger_transactions"); n != 3 {
		t.Errorf("%d ledger transactions, want 3", n)
	}
	assertBalances(t, db, map[ledger.Account]money.Amount{hold: 135})
}

func TestPostBatchRejectsOnlyFailingTransactions(t *testing.T) {
	db := testdb.New(t)
	hold, available := ledger.PartnerHold(partner), ledger.PartnerAvailable(partner)

	results := postBatch(t, db,
		depositTxn("a", 10),
		depositTxn("b", 0),
		depositTxn("a", 10),
		depositTxn("", 1),
		ledger.Transaction{Key: "c", Legs: []ledger.Leg{{Debit: ledger.Account{ID: hold.ID, Kind: ledger.Asset}, Credit: ledger.Fees, Amount: 1}}},
		ledger.Transaction{Key: "d", Legs: []ledger.Leg{{Debit: hold, Credit: available, Amount: 11}}},
		ledger.Transaction{Key: "e", Legs: []ledger.Leg{{Debit: hold, Credit: available, Amount: 4}}},
	)
	assertResults(t, results,
		true,
		ledger.ErrInvalidLeg,
		ledger.ErrDuplicateKey,
		ledger.ErrEmptyKey,
		ledger.ErrAccountKindMismatch,
		ledger.ErrInsufficientFunds,
		true,
	)
	if n := count(t, db, "ledger_transactions"); n != 2 {
		t.Errorf("%d ledger transactions, want 2", n)
	}
	assertBalances(t, db, map[ledger.Account]money.Amount{ledger.Custody: 10, hold: 6, available: 4, ledger.Fees: 0})

	assertResults(t, postBatch(t, db, ledger.Transaction{Key: "d", Legs: []ledger.Leg{{Debit: hold, Credit: available, Amount: 6}}}), true)
	assertBalances(t, db, map[ledger.Account]money.Amount{hold: 0, available: 10})
}

func TestPostBatchChecksFundsInTransactionOrder(t *testing.T) {
	db := testdb.New(t)
	hold, available := ledger.PartnerHold(partner), ledger.PartnerAvailable(partner)
	settle := ledger.Transaction{Key: "settle", Legs: []ledger.Leg{{Debit: hold, Credit: available, Amount: 100}}}

	assertResults(t, postBatch(t, db, settle, depositTxn("hold", 100)), ledger.ErrInsufficientFunds, true)
	assertBalances(t, db, map[ledger.Account]money.Amount{hold: 100, available: 0})

	assertResults(t, postBatch(t, db, depositTxn("hold", 100), settle), false, true)
	assertBalances(t, db, map[ledger.Account]money.Amount{hold: 0, available: 100})
}

func TestPostBatchWithNothingToPost(t *testing.T) {
	db := testdb.New(t)
	if _, err := ledger.PostBatch(t.Context(), db); !errors.Is(err, ledger.ErrEmptyBatch) {
		t.Errorf("empty batch error = %v, want ErrEmptyBatch", err)
	}
	assertResults(t, postBatch(t, db, depositTxn("bad", -1)), ledger.ErrInvalidLeg)
	if n := count(t, db, "ledger_transactions"); n != 0 {
		t.Errorf("%d ledger transactions, want 0", n)
	}
}

func TestConcurrentBatchesDoNotDeadlock(t *testing.T) {
	db := testdb.New(t)
	const partners, workers, rounds = 4, 8, 10
	for p := range partners {
		post(t, db, fmt.Sprintf("seed:%d", p), ledger.Leg{Debit: ledger.Custody, Credit: ledger.PartnerAvailable(int64(p)), Amount: 10_000})
	}

	var mu sync.Mutex
	want := make(map[ledger.Account]money.Amount)
	for p := range partners {
		want[ledger.PartnerAvailable(int64(p))] = 10_000
	}
	var wg sync.WaitGroup
	for w := range workers {
		wg.Go(func() {
			for r := range rounds {
				var txns []ledger.Transaction
				moves := make(map[ledger.Account]money.Amount)
				for i := range partners {
					from := ledger.PartnerAvailable(int64((w + i) % partners))
					to := ledger.PartnerAvailable(int64((w + i + 1 + r) % partners))
					if from == to {
						continue
					}
					txns = append(txns, ledger.Transaction{
						Key: fmt.Sprintf("move:%d:%d:%d", w, r, i),
						Legs: []ledger.Leg{
							{Debit: from, Credit: to, Amount: 10},
							{Debit: from, Credit: ledger.Fees, Amount: 1},
						},
					})
					moves[from] -= 11
					moves[to] += 10
					moves[ledger.Fees]++
				}
				if len(txns) == 0 {
					continue
				}
				results, err := ledger.PostBatch(context.Background(), db, txns...)
				if err != nil {
					t.Error(err)
					continue
				}
				for _, r := range results {
					if !r.Applied || r.Err != nil {
						t.Errorf("result = %+v, want applied", r)
					}
				}
				mu.Lock()
				for a, d := range moves {
					want[a] += d
				}
				mu.Unlock()
			}
		})
	}
	wg.Wait()

	want[ledger.Custody] = partners * 10_000
	assertBalances(t, db, want)
}
