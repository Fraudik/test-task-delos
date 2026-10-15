package ledger

import (
	"cmp"
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
	Custody        = Account{ID: "custody", Kind: Asset}
	Fees           = Account{ID: "fees", Kind: Revenue}
	RefundsPending = Account{ID: "refunds_payable", Kind: Liability}
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

var (
	ErrEmptyKey            = errors.New("ledger: empty idempotency key")
	ErrNoLegs              = errors.New("ledger: transaction has no legs")
	ErrInvalidLeg          = errors.New("ledger: invalid leg")
	ErrInsufficientFunds   = errors.New("ledger: insufficient funds")
	ErrIdempotencyConflict = errors.New("ledger: idempotency key reused with different legs")
	ErrUnbalanced          = errors.New("ledger: integrity check failed")
	ErrAccountKindMismatch = errors.New("ledger: account exists with another kind")
)

type DB interface {
	ledgerdb.DBTX
	Begin(ctx context.Context) (pgx.Tx, error)
}

func Post(ctx context.Context, db DB, key string, legs ...Leg) (bool, error) {
	if err := validate(key, legs); err != nil {
		return false, err
	}
	digest := digestOf(legs)

	tx, err := db.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("ledger: begin: %w", err)
	}
	defer tx.Rollback(ctx)
	q := ledgerdb.New(tx)

	txID, err := q.InsertTransaction(ctx, ledgerdb.InsertTransactionParams{IdempotencyKey: key, Digest: digest})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, checkDigest(ctx, q, key, digest)
	}
	if err != nil {
		return false, fmt.Errorf("ledger: insert transaction: %w", err)
	}

	deltas := make(map[Account]money.Amount)
	for _, l := range legs {
		deltas[l.Debit] += l.Amount
		deltas[l.Credit] -= l.Amount
	}
	accounts := slices.SortedFunc(maps.Keys(deltas), func(a, b Account) int { return cmp.Compare(a.ID, b.ID) })
	for _, a := range accounts {
		if err := applyDelta(ctx, q, a, deltas[a]); err != nil {
			return false, err
		}
	}

	entries := make([]ledgerdb.InsertEntriesParams, 0, 2*len(legs))
	for _, l := range legs {
		entries = append(entries,
			ledgerdb.InsertEntriesParams{TransactionID: txID, AccountID: l.Debit.ID, Amount: l.Amount},
			ledgerdb.InsertEntriesParams{TransactionID: txID, AccountID: l.Credit.ID, Amount: -l.Amount},
		)
	}
	if _, err := q.InsertEntries(ctx, entries); err != nil {
		return false, fmt.Errorf("ledger: insert entries: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return false, mapError(fmt.Errorf("ledger: commit: %w", err))
	}
	return true, nil
}

func Balance(ctx context.Context, db DB, a Account) (money.Amount, error) {
	balance, err := ledgerdb.New(db).GetAccountBalance(ctx, a.ID)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("ledger: balance %s: %w", a.ID, err)
	}
	if a.Kind == Asset {
		return balance, nil
	}
	return -balance, nil
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

func applyDelta(ctx context.Context, q *ledgerdb.Queries, a Account, delta money.Amount) error {
	if err := q.EnsureAccount(ctx, ledgerdb.EnsureAccountParams{ID: a.ID, Kind: a.Kind.String()}); err != nil {
		return fmt.Errorf("ledger: create account %s: %w", a.ID, err)
	}
	n, err := q.ApplyAccountDelta(ctx, ledgerdb.ApplyAccountDeltaParams{ID: a.ID, Kind: a.Kind.String(), Delta: delta})
	if err != nil {
		return mapError(fmt.Errorf("ledger: update account %s: %w", a.ID, err))
	}
	if n == 0 {
		return fmt.Errorf("%w: %s is not %s", ErrAccountKindMismatch, a.ID, a.Kind)
	}
	return nil
}

func validate(key string, legs []Leg) error {
	if key == "" {
		return ErrEmptyKey
	}
	if len(legs) == 0 {
		return ErrNoLegs
	}
	for i, l := range legs {
		switch {
		case l.Amount <= 0:
			return fmt.Errorf("%w %d: amount %d must be positive", ErrInvalidLeg, i, l.Amount)
		case l.Debit.ID == "" || l.Credit.ID == "":
			return fmt.Errorf("%w %d: empty account", ErrInvalidLeg, i)
		case l.Debit.ID == l.Credit.ID:
			return fmt.Errorf("%w %d: debit and credit are the same account %s", ErrInvalidLeg, i, l.Debit.ID)
		case !l.Debit.Kind.Valid() || !l.Credit.Kind.Valid():
			return fmt.Errorf("%w %d: unknown account kind %s/%s", ErrInvalidLeg, i, l.Debit.Kind, l.Credit.Kind)
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

func checkDigest(ctx context.Context, q *ledgerdb.Queries, key, digest string) error {
	existing, err := q.GetTransactionDigest(ctx, key)
	if err != nil {
		return fmt.Errorf("ledger: load transaction %s: %w", key, err)
	}
	if existing != digest {
		return fmt.Errorf("%w: %s", ErrIdempotencyConflict, key)
	}
	return nil
}

func mapError(err error) error {
	if pgErr, ok := errors.AsType[*pgconn.PgError](err); ok && pgErr.ConstraintName == "ledger_accounts_no_overdraft" {
		return fmt.Errorf("%w: %w", ErrInsufficientFunds, err)
	}
	return err
}
