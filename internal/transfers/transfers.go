package transfers

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"test-task-dellos/internal/money"
	"test-task-dellos/internal/transfers/transfersdb"
)

type RefundRequest struct {
	RefundID  int64
	DepositID int64
	ToAddress string
	Amount    money.Amount
}

type RefundInitiator interface {
	InitiateRefund(ctx context.Context, r RefundRequest) error
}

type LogInitiator struct {
	Log *slog.Logger
}

func (l LogInitiator) InitiateRefund(ctx context.Context, r RefundRequest) error {
	l.Log.Info("InitiateRefund", "refund_id", r.RefundID, "deposit_id", r.DepositID, "to", r.ToAddress, "amount", r.Amount.String())
	return nil
}

type Config struct {
	PollInterval time.Duration
	BatchSize    int32
	CallTimeout  time.Duration
	Lease        time.Duration
	BackoffBase  time.Duration
	BackoffMax   time.Duration
}

func DefaultConfig() Config {
	return Config{
		PollInterval: time.Second,
		BatchSize:    100,
		CallTimeout:  10 * time.Second,
		Lease:        30 * time.Second,
		BackoffBase:  time.Second,
		BackoffMax:   5 * time.Minute,
	}
}

type Transfers struct {
	db        *pgxpool.Pool
	initiator RefundInitiator
	cfg       Config
	log       *slog.Logger
}

func New(db *pgxpool.Pool, initiator RefundInitiator, cfg Config, log *slog.Logger) *Transfers {
	if log == nil {
		log = slog.Default()
	}
	return &Transfers{db: db, initiator: initiator, cfg: cfg, log: log.With("component", "transfers")}
}

func (t *Transfers) Run(ctx context.Context) {
	ticker := time.NewTicker(t.cfg.PollInterval)
	defer ticker.Stop()
	for {
		for {
			n, err := t.Tick(ctx)
			if err != nil && ctx.Err() == nil {
				t.log.Error("transfers failed", "err", err)
			}
			if err != nil || n == 0 {
				break
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (t *Transfers) Tick(ctx context.Context) (int, error) {
	return t.InitiateRefunds(ctx)
}

func (t *Transfers) InitiateRefunds(ctx context.Context) (int, error) {
	q := transfersdb.New(t.db)
	claimed, err := q.ClaimRefunds(ctx, transfersdb.ClaimRefundsParams{LeaseSeconds: t.cfg.Lease.Seconds(), MaxRows: t.cfg.BatchSize})
	if err != nil {
		return 0, fmt.Errorf("transfers: claim refunds: %w", err)
	}
	var (
		done    []int64
		retries transfersdb.ScheduleRefundRetriesParams
	)
	for _, c := range claimed {
		callCtx, cancel := context.WithTimeout(ctx, t.cfg.CallTimeout)
		err := t.initiator.InitiateRefund(callCtx, RefundRequest{RefundID: c.ID, DepositID: c.DepositID, ToAddress: c.ToAddress, Amount: c.Amount})
		cancel()
		if err != nil {
			delay := backoff(t.cfg, c.InitiateAttempts)
			t.log.Warn("InitiateRefund failed, will retry", "refund_id", c.ID, "attempt", c.InitiateAttempts+1, "retry_in", delay, "err", err)
			retries.Ids = append(retries.Ids, c.ID)
			retries.DelaysSeconds = append(retries.DelaysSeconds, delay.Seconds())
			continue
		}
		done = append(done, c.ID)
	}
	if ctx.Err() != nil {
		return 0, ctx.Err()
	}
	if len(done) > 0 {
		if err := q.MarkRefundsInitiated(ctx, done); err != nil {
			return 0, fmt.Errorf("transfers: mark refunds initiated: %w", err)
		}
	}
	if len(retries.Ids) > 0 {
		if err := q.ScheduleRefundRetries(ctx, retries); err != nil {
			return 0, fmt.Errorf("transfers: schedule refund retries: %w", err)
		}
	}
	return len(claimed), nil
}

func backoff(cfg Config, attempts int32) time.Duration {
	delay := cfg.BackoffBase
	for i := int32(0); i < attempts && delay < cfg.BackoffMax; i++ {
		delay *= 2
	}
	return min(delay, cfg.BackoffMax)
}
