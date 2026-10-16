package notify

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"test-task-dellos/internal/money"
	"test-task-dellos/internal/notify/notifydb"
)

type Notification struct {
	DepositID int64
	InvoiceID int64
	PartnerID int64
	TxHash    string
	Amount    money.Amount
}

type PartnerNotifier interface {
	NotifyPartner(ctx context.Context, n Notification) error
}

type LogNotifier struct {
	Log *slog.Logger
}

func (l LogNotifier) NotifyPartner(ctx context.Context, n Notification) error {
	l.Log.Info("NotifyPartner", "deposit_id", n.DepositID, "invoice_id", n.InvoiceID, "partner_id", n.PartnerID, "tx", n.TxHash, "amount", n.Amount.String())
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

type Notifier struct {
	db       *pgxpool.Pool
	notifier PartnerNotifier
	cfg      Config
	log      *slog.Logger
}

func New(db *pgxpool.Pool, notifier PartnerNotifier, cfg Config, log *slog.Logger) *Notifier {
	if log == nil {
		log = slog.Default()
	}
	return &Notifier{db: db, notifier: notifier, cfg: cfg, log: log.With("component", "notify")}
}

func (n *Notifier) Run(ctx context.Context) {
	ticker := time.NewTicker(n.cfg.PollInterval)
	defer ticker.Stop()
	for {
		for {
			sent, err := n.Tick(ctx)
			if err != nil && ctx.Err() == nil {
				n.log.Error("notify failed", "err", err)
			}
			if err != nil || sent == 0 {
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

func (n *Notifier) Tick(ctx context.Context) (int, error) {
	q := notifydb.New(n.db)
	claimed, err := q.ClaimNotifications(ctx, notifydb.ClaimNotificationsParams{LeaseSeconds: n.cfg.Lease.Seconds(), MaxRows: n.cfg.BatchSize})
	if err != nil {
		return 0, fmt.Errorf("notify: claim notifications: %w", err)
	}
	var (
		done    []int64
		retries notifydb.ScheduleNotifyRetriesParams
	)
	for _, c := range claimed {
		callCtx, cancel := context.WithTimeout(ctx, n.cfg.CallTimeout)
		err := n.notifier.NotifyPartner(callCtx, Notification{
			DepositID: c.ID, InvoiceID: c.InvoiceID, PartnerID: c.PartnerID, TxHash: c.TxHash, Amount: c.Amount,
		})
		cancel()
		if err != nil {
			delay := backoff(n.cfg, c.NotifyAttempts)
			n.log.Warn("NotifyPartner failed, will retry", "deposit_id", c.ID, "attempt", c.NotifyAttempts+1, "retry_in", delay, "err", err)
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
		if err := q.MarkNotified(ctx, done); err != nil {
			return 0, fmt.Errorf("notify: mark notified: %w", err)
		}
	}
	if len(retries.Ids) > 0 {
		if err := q.ScheduleNotifyRetries(ctx, retries); err != nil {
			return 0, fmt.Errorf("notify: schedule retries: %w", err)
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
