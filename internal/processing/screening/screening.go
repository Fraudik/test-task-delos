package screening

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"test-task-dellos/internal/aml"
	"test-task-dellos/internal/processing/screening/screeningdb"
)

type Config struct {
	Concurrency int
	Timeout     time.Duration
	Lease       time.Duration
	BackoffBase time.Duration
	BackoffMax  time.Duration
}

type Stage struct {
	db         *pgxpool.Pool
	checker    aml.Checker
	cfg        Config
	log        *slog.Logger
	onRecorded func()
}

func New(db *pgxpool.Pool, checker aml.Checker, cfg Config, log *slog.Logger, onRecorded func()) *Stage {
	return &Stage{db: db, checker: checker, cfg: cfg, log: log.With("stage", "aml"), onRecorded: onRecorded}
}

type screening struct {
	deposit screeningdb.ClaimAMLChecksRow
	result  aml.Result
	err     error
}

func (s *Stage) Run(ctx context.Context) (int, error) {
	q := screeningdb.New(s.db)
	claimed, err := q.ClaimAMLChecks(ctx, screeningdb.ClaimAMLChecksParams{
		LeaseSeconds: s.cfg.Lease.Seconds(),
		MaxRows:      int32(s.cfg.Concurrency),
	})
	if err != nil {
		return 0, fmt.Errorf("screening: claim AML checks: %w", err)
	}
	if len(claimed) == 0 {
		return 0, nil
	}

	screenings := s.screenAll(ctx, claimed)
	if ctx.Err() != nil {
		return 0, ctx.Err()
	}

	var (
		results      []screening
		retries      screeningdb.ScheduleAMLRetriesParams
		providerDown bool
		errs         []error
	)
	for _, sc := range screenings {
		if sc.err == nil {
			results = append(results, sc)
			continue
		}
		delay := s.backoff(sc.deposit.Attempts)
		retries.Ids = append(retries.Ids, sc.deposit.DepositID)
		retries.DelaysSeconds = append(retries.DelaysSeconds, delay.Seconds())
		unavailable := errors.Is(sc.err, aml.ErrUnavailable)
		retries.WaitingForProvider = append(retries.WaitingForProvider, unavailable)
		providerDown = providerDown || unavailable
		level := slog.LevelError
		if unavailable {
			level = slog.LevelWarn
		}
		s.log.Log(ctx, level, "AML check failed, will retry",
			"deposit_id", sc.deposit.DepositID, "attempt", sc.deposit.Attempts+1, "retry_in", delay, "err", sc.err)
	}
	if len(retries.Ids) > 0 {
		if err := q.ScheduleAMLRetries(ctx, retries); err != nil {
			errs = append(errs, fmt.Errorf("screening: schedule AML retries: %w", err))
		}
	}
	if len(results) > 0 {
		if err := s.record(ctx, q, results); err != nil {
			errs = append(errs, err)
		} else if !providerDown {
			if err := s.releaseWaiting(ctx, q); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return len(claimed), errors.Join(errs...)
}

func (s *Stage) releaseWaiting(ctx context.Context, q *screeningdb.Queries) error {
	n, err := q.ReleaseWaitingAMLChecks(ctx)
	if err != nil {
		return fmt.Errorf("screening: release deferred AML checks: %w", err)
	}
	if n > 0 {
		s.log.Info("AML provider is back, retrying deferred checks now", "checks", n)
	}
	return nil
}

func (s *Stage) screenAll(ctx context.Context, claimed []screeningdb.ClaimAMLChecksRow) []screening {
	out := make([]screening, len(claimed))
	sem := make(chan struct{}, s.cfg.Concurrency)
	var wg sync.WaitGroup
	for i, d := range claimed {
		sem <- struct{}{}
		wg.Go(func() {
			defer func() { <-sem }()
			out[i] = s.screen(ctx, d)
		})
	}
	wg.Wait()
	return out
}

func (s *Stage) screen(ctx context.Context, d screeningdb.ClaimAMLChecksRow) screening {
	callCtx, cancel := context.WithTimeout(ctx, s.cfg.Timeout)
	defer cancel()
	res, err := s.checker.Check(callCtx, aml.Request{TxHash: d.TxHash, From: d.FromAddress, To: d.ToAddress, Amount: d.Amount})
	if err == nil && res.Risk != aml.RiskLow && res.Risk != aml.RiskHigh {
		err = fmt.Errorf("unknown AML risk %q", res.Risk)
	}
	return screening{deposit: d, result: res, err: err}
}

func (s *Stage) backoff(attempts int32) time.Duration {
	d := s.cfg.BackoffBase
	for i := int32(0); i < attempts && d < s.cfg.BackoffMax; i++ {
		d *= 2
	}
	return min(d, s.cfg.BackoffMax)
}

func (s *Stage) record(ctx context.Context, q *screeningdb.Queries, results []screening) error {
	params := screeningdb.RecordAMLResultsParams{
		Ids:     make([]int64, len(results)),
		Risks:   make([]string, len(results)),
		Reasons: make([]string, len(results)),
	}
	for i, sc := range results {
		params.Ids[i] = sc.deposit.DepositID
		params.Risks[i] = string(sc.result.Risk)
		params.Reasons[i] = sc.result.Reason
	}
	n, err := q.RecordAMLResults(ctx, params)
	if err != nil {
		return fmt.Errorf("screening: record AML results: %w", err)
	}
	for i, id := range params.Ids {
		s.log.Info("AML result recorded", "deposit_id", id, "risk", params.Risks[i], "reason", params.Reasons[i])
	}
	if n > 0 && s.onRecorded != nil {
		s.onRecorded()
	}
	return nil
}
