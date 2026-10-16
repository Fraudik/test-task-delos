package processing

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"test-task-dellos/internal/aml"
	"test-task-dellos/internal/chain"
	"test-task-dellos/internal/money"
)

const (
	stageScan          = "scan"
	stageConfirmations = "confirmations"
	stageAML           = "aml"
	stageSettlement    = "settlement"
)

type Processor struct {
	db      *pgxpool.Pool
	chain   chain.Client
	aml     aml.Checker
	cfg     Config
	log     *slog.Logger
	wakeups map[string]chan struct{}
}

func New(db *pgxpool.Pool, c chain.Client, checker aml.Checker, cfg Config, log *slog.Logger) (*Processor, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	if log == nil {
		log = slog.Default()
	}
	wakeups := make(map[string]chan struct{})
	for _, name := range []string{stageScan, stageConfirmations, stageAML, stageSettlement} {
		wakeups[name] = make(chan struct{}, 1)
	}
	return &Processor{
		db:      db,
		chain:   c,
		aml:     checker,
		cfg:     cfg,
		log:     log.With("component", "processing"),
		wakeups: wakeups,
	}, nil
}

func (p *Processor) wake(stage string) {
	select {
	case p.wakeups[stage] <- struct{}{}:
	default:
	}
}

type stage struct {
	name string
	run  func(context.Context) (int, error)
}

func (p *Processor) scannerStages() []stage {
	return []stage{{stageScan, p.ScanBlocks}}
}

func (p *Processor) workerStages() []stage {
	return []stage{
		{stageConfirmations, p.ProcessConfirmations},
		{stageAML, p.ProcessAML},
		{stageSettlement, p.ProcessSettlement},
	}
}

func (p *Processor) stages() []stage {
	return append(p.scannerStages(), p.workerStages()...)
}

func (p *Processor) Run(ctx context.Context) {
	p.runStages(ctx, p.stages())
}

func (p *Processor) RunScanner(ctx context.Context) {
	p.runStages(ctx, p.scannerStages())
}

func (p *Processor) RunWorkers(ctx context.Context) {
	p.runStages(ctx, p.workerStages())
}

func (p *Processor) runStages(ctx context.Context, stages []stage) {
	var wg sync.WaitGroup
	for _, s := range stages {
		wg.Go(func() { p.loop(ctx, s) })
	}
	wg.Wait()
}

func (p *Processor) Tick(ctx context.Context) error {
	var errs []error
	for _, s := range p.stages() {
		if _, err := s.run(ctx); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", s.name, err))
		}
	}
	return errors.Join(errs...)
}

func (p *Processor) loop(ctx context.Context, s stage) {
	ticker := time.NewTicker(p.cfg.PollInterval)
	defer ticker.Stop()
	for {
		for {
			n, err := s.run(ctx)
			if ctx.Err() != nil {
				return
			}
			if err != nil {
				p.log.Error("stage failed", "stage", s.name, "err", err)
				break
			}
			if n == 0 {
				break
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-p.wakeups[s.name]:
		}
	}
}

func depositKey(depositID int64, step string) string {
	return fmt.Sprintf("deposit:%d:%s", depositID, step)
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
