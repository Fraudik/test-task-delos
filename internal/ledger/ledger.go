package ledger

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"test-task-dellos/internal/ledger/ledgerdb"
	"test-task-dellos/internal/money"
)

type Kind uint8

const (
	Asset Kind = iota + 1
	Liability
	Revenue
)

func (k Kind) String() string {
	switch k {
	case Asset:
		return "asset"
	case Liability:
		return "liability"
	case Revenue:
		return "revenue"
	default:
		return fmt.Sprintf("Kind(%d)", uint8(k))
	}
}

func (k Kind) Valid() bool {
	return k >= Asset && k <= Revenue
}

type Account struct {
	ID   string
	Kind Kind
}

var (
	Custody       = Account{ID: "custody", Kind: Asset}
	Fees          = Account{ID: "fees", Kind: Revenue}
	RefundPending = Account{ID: "refund_pending", Kind: Liability}
)

func PartnerHold(partnerID int64) Account {
	return Account{ID: fmt.Sprintf("partner:%d:hold", partnerID), Kind: Liability}
}

func PartnerAvailable(partnerID int64) Account {
	return Account{ID: fmt.Sprintf("partner:%d:available", partnerID), Kind: Liability}
}

type Leg struct {
	Debit  Account
	Credit Account
	Amount money.Amount
}

type Transaction struct {
	Key  string
	Legs []Leg
}

type Result struct {
	Applied bool
	Err     error
}

var (
	ErrEmptyKey            = errors.New("ledger: empty idempotency key")
	ErrNoLegs              = errors.New("ledger: transaction has no legs")
	ErrInvalidLeg          = errors.New("ledger: invalid leg")
	ErrEmptyBatch          = errors.New("ledger: batch has no transactions")
	ErrDuplicateKey        = errors.New("ledger: idempotency key repeated in batch")
	ErrInsufficientFunds   = errors.New("ledger: insufficient funds")
	ErrIdempotencyConflict = errors.New("ledger: idempotency key reused with different legs")
	ErrUnbalanced          = errors.New("ledger: integrity check failed")
	ErrAccountKindMismatch = errors.New("ledger: account exists with another kind")
	ErrConcurrentPost      = errors.New("ledger: transaction key posted concurrently, retry the batch")
)

type DB interface {
	ledgerdb.DBTX
	Begin(ctx context.Context) (pgx.Tx, error)
}

func PostBatch(ctx context.Context, db DB, txns ...Transaction) ([]Result, error) {
	if len(txns) == 0 {
		return nil, ErrEmptyBatch
	}
	results := make([]Result, len(txns))
	candidates := screenTransactions(txns, results)
	if len(candidates) == 0 {
		return results, nil
	}

	tx, err := db.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("ledger: begin: %w", err)
	}
	defer tx.Rollback(ctx)
	q := ledgerdb.New(tx)

	stored, err := lockAccounts(ctx, q, txns, candidates)
	if err != nil {
		return nil, err
	}
	candidates, err = skipPosted(ctx, q, txns, candidates, results)
	if err != nil {
		return nil, err
	}
	if len(candidates) == 0 {
		return results, nil
	}
	accepted, err := applyBalances(ctx, q, txns, candidates, results, stored)
	if err != nil {
		return nil, err
	}
	if len(accepted) == 0 {
		return results, nil
	}
	ids, err := insertTransactions(ctx, q, txns, accepted)
	if err != nil {
		return nil, err
	}
	if err := insertEntries(ctx, q, txns, accepted, ids); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, mapError(fmt.Errorf("ledger: commit: %w", err))
	}
	for _, i := range accepted {
		results[i].Applied = true
	}
	return results, nil
}

func Balance(ctx context.Context, db DB, a Account) (money.Amount, error) {
	balance, err := ledgerdb.New(db).GetAccountBalance(ctx, a.ID)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("ledger: balance %s: %w", a.ID, err)
	}
	return normal(a.Kind, balance), nil
}

func CheckIntegrity(ctx context.Context, db DB) error {
	r, err := ledgerdb.New(db).IntegrityReport(ctx)
	if err != nil {
		return fmt.Errorf("ledger: integrity query: %w", err)
	}
	if r.Total != 0 || r.DriftedAccounts != 0 || r.UnbalancedTransactions != 0 {
		return fmt.Errorf("%w: total=%d drifted_accounts=%d unbalanced_transactions=%d",
			ErrUnbalanced, r.Total, r.DriftedAccounts, r.UnbalancedTransactions)
	}
	return nil
}

func screenTransactions(txns []Transaction, results []Result) []int {
	seen := make(map[string]struct{}, len(txns))
	var candidates []int
	for i, t := range txns {
		if err := validate(t.Key, t.Legs); err != nil {
			results[i].Err = err
			continue
		}
		if _, dup := seen[t.Key]; dup {
			results[i].Err = fmt.Errorf("%w: %s", ErrDuplicateKey, t.Key)
			continue
		}
		seen[t.Key] = struct{}{}
		candidates = append(candidates, i)
	}
	return candidates
}

func skipPosted(ctx context.Context, q *ledgerdb.Queries, txns []Transaction, candidates []int, results []Result) ([]int, error) {
	keys := make([]string, len(candidates))
	for j, i := range candidates {
		keys[j] = txns[i].Key
	}
	stored, err := q.GetTransactionDigests(ctx, keys)
	if err != nil {
		return nil, fmt.Errorf("ledger: load posted transactions: %w", err)
	}
	if len(stored) == 0 {
		return candidates, nil
	}
	digests := make(map[string]string, len(stored))
	for _, s := range stored {
		digests[s.IdempotencyKey] = s.Digest
	}

	fresh := candidates[:0:0]
	for _, i := range candidates {
		digest, posted := digests[txns[i].Key]
		switch {
		case !posted:
			fresh = append(fresh, i)
		case digest != digestOf(txns[i].Legs):
			results[i].Err = fmt.Errorf("%w: %s", ErrIdempotencyConflict, txns[i].Key)
		}
	}
	return fresh, nil
}

func lockAccounts(ctx context.Context, q *ledgerdb.Queries, txns []Transaction, candidates []int) (map[string]ledgerdb.LedgerAccount, error) {
	kinds := make(map[string]Kind)
	for _, i := range candidates {
		for _, l := range txns[i].Legs {
			for _, a := range []Account{l.Debit, l.Credit} {
				if _, ok := kinds[a.ID]; !ok {
					kinds[a.ID] = a.Kind
				}
			}
		}
	}
	ids := slices.Sorted(maps.Keys(kinds))
	kindNames := make([]string, len(ids))
	for i, id := range ids {
		kindNames[i] = kinds[id].String()
	}
	if err := q.EnsureAccounts(ctx, ledgerdb.EnsureAccountsParams{Ids: ids, Kinds: kindNames}); err != nil {
		return nil, fmt.Errorf("ledger: create accounts: %w", err)
	}
	locked, err := q.LockAccounts(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("ledger: lock accounts: %w", err)
	}
	stored := make(map[string]ledgerdb.LedgerAccount, len(locked))
	for _, row := range locked {
		stored[row.ID] = row
	}
	return stored, nil
}

func applyBalances(ctx context.Context, q *ledgerdb.Queries, txns []Transaction, candidates []int, results []Result, stored map[string]ledgerdb.LedgerAccount) ([]int, error) {
	ids := slices.Sorted(maps.Keys(stored))
	deltas := make(map[string]money.Amount, len(ids))
	var accepted []int
	for _, i := range candidates {
		txDeltas, err := checkTransaction(txns[i], stored, deltas)
		if err != nil {
			results[i].Err = err
			continue
		}
		for id, d := range txDeltas {
			deltas[id] += d
		}
		accepted = append(accepted, i)
	}
	if len(accepted) == 0 {
		return nil, nil
	}

	changed := slices.DeleteFunc(slices.Clone(ids), func(id string) bool { return deltas[id] == 0 })
	amounts := make([]int64, len(changed))
	for i, id := range changed {
		amounts[i] = int64(deltas[id])
	}
	if err := q.ApplyAccountDeltas(ctx, ledgerdb.ApplyAccountDeltasParams{Ids: changed, Deltas: amounts}); err != nil {
		return nil, mapError(fmt.Errorf("ledger: update balances: %w", err))
	}
	return accepted, nil
}

func checkTransaction(t Transaction, stored map[string]ledgerdb.LedgerAccount, deltas map[string]money.Amount) (map[string]money.Amount, error) {
	txDeltas := make(map[string]money.Amount)
	accounts := make(map[string]Kind)
	for _, l := range t.Legs {
		for _, a := range []Account{l.Debit, l.Credit} {
			if stored[a.ID].Kind != a.Kind.String() {
				return nil, fmt.Errorf("%w: %s is %s, not %s", ErrAccountKindMismatch, a.ID, stored[a.ID].Kind, a.Kind)
			}
			accounts[a.ID] = a.Kind
		}
		txDeltas[l.Debit.ID] += l.Amount
		txDeltas[l.Credit.ID] -= l.Amount
	}
	for id, kind := range accounts {
		if normal(kind, stored[id].Balance+deltas[id]+txDeltas[id]) < 0 {
			return nil, fmt.Errorf("%w: transaction %s overdraws %s", ErrInsufficientFunds, t.Key, id)
		}
	}
	return txDeltas, nil
}

func insertTransactions(ctx context.Context, q *ledgerdb.Queries, txns []Transaction, accepted []int) (map[string]int64, error) {
	params := ledgerdb.InsertTransactionsParams{
		IdempotencyKeys: make([]string, len(accepted)),
		Digests:         make([]string, len(accepted)),
	}
	for j, i := range accepted {
		params.IdempotencyKeys[j] = txns[i].Key
		params.Digests[j] = digestOf(txns[i].Legs)
	}
	rows, err := q.InsertTransactions(ctx, params)
	if err != nil {
		return nil, fmt.Errorf("ledger: insert transactions: %w", err)
	}
	if len(rows) != len(accepted) {
		return nil, ErrConcurrentPost
	}
	ids := make(map[string]int64, len(rows))
	for _, r := range rows {
		ids[r.IdempotencyKey] = r.ID
	}
	return ids, nil
}

func insertEntries(ctx context.Context, q *ledgerdb.Queries, txns []Transaction, accepted []int, ids map[string]int64) error {
	var entries []ledgerdb.InsertEntriesParams
	for _, i := range accepted {
		t := txns[i]
		txID := ids[t.Key]
		for _, l := range t.Legs {
			entries = append(entries,
				ledgerdb.InsertEntriesParams{TransactionID: txID, AccountID: l.Debit.ID, Amount: l.Amount},
				ledgerdb.InsertEntriesParams{TransactionID: txID, AccountID: l.Credit.ID, Amount: -l.Amount},
			)
		}
	}
	if _, err := q.InsertEntries(ctx, entries); err != nil {
		return fmt.Errorf("ledger: insert entries: %w", err)
	}
	return nil
}

func normal(k Kind, raw money.Amount) money.Amount {
	if k == Asset {
		return raw
	}
	return -raw
}

func validate(key string, legs []Leg) error {
	if key == "" {
		return ErrEmptyKey
	}
	if len(legs) == 0 {
		return fmt.Errorf("%w: %s", ErrNoLegs, key)
	}
	for i, l := range legs {
		switch {
		case l.Amount <= 0:
			return fmt.Errorf("%w %d of %s: amount %d must be positive", ErrInvalidLeg, i, key, l.Amount)
		case l.Debit.ID == "" || l.Credit.ID == "":
			return fmt.Errorf("%w %d of %s: empty account", ErrInvalidLeg, i, key)
		case l.Debit.ID == l.Credit.ID:
			return fmt.Errorf("%w %d of %s: debit and credit are the same account %s", ErrInvalidLeg, i, key, l.Debit.ID)
		case !l.Debit.Kind.Valid() || !l.Credit.Kind.Valid():
			return fmt.Errorf("%w %d of %s: unknown account kind %s/%s", ErrInvalidLeg, i, key, l.Debit.Kind, l.Credit.Kind)
		}
	}
	return nil
}

func digestOf(legs []Leg) string {
	lines := make([]string, len(legs))
	for i, l := range legs {
		lines[i] = fmt.Sprintf("%s|%s|%d", l.Debit.ID, l.Credit.ID, l.Amount)
	}
	slices.Sort(lines)
	sum := sha256.Sum256([]byte(strings.Join(lines, "\n")))
	return hex.EncodeToString(sum[:])
}

func mapError(err error) error {
	if pgErr, ok := errors.AsType[*pgconn.PgError](err); ok && pgErr.ConstraintName == "ledger_accounts_no_overdraft" {
		return fmt.Errorf("%w: %w", ErrInsufficientFunds, err)
	}
	return err
}
