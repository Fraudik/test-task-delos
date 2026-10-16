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
	"test-task-dellos/internal/processing/confirmations"
	"test-task-dellos/internal/processing/scanner"
	"test-task-dellos/internal/processing/screening"
	"test-task-dellos/internal/processing/settlement"
)

const (
	stageScan          = "scan"
	stageConfirmations = "confirmations"
	stageAML           = "aml"
	stageSettlement    = "settlement"
)

type Processor struct {
	cfg           Config
	log           *slog.Logger
	scanner       *scanner.Stage
	confirmations *confirmations.Stage
	screening     *screening.Stage
	settlement    *settlement.Stage
	wakeups       map[string]chan struct{}
}

func New(db *pgxpool.Pool, c chain.Client, checker aml.Checker, cfg Config, log *slog.Logger) (*Processor, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	if log == nil {
		log = slog.Default()
	}
	log = log.With("component", "processing")
	p := &Processor{cfg: cfg, log: log, wakeups: make(map[string]chan struct{})}
	for _, name := range []string{stageScan, stageConfirmations, stageAML, stageSettlement} {
		p.wakeups[name] = make(chan struct{}, 1)
	}
	wakeSettlement := func() { p.wake(stageSettlement) }

	p.scanner = scanner.New(db, c, scanner.Config{
		StartBlock:   cfg.StartBlock,
		ScanBatch:    cfg.ScanBatch,
		ReorgHistory: cfg.ReorgHistory,
	}, log)
	p.confirmations = confirmations.New(db, confirmations.Config{
		Confirmations: cfg.Confirmations,
		BatchSize:     cfg.BatchSize,
	}, log, wakeSettlement)
	p.screening = screening.New(db, checker, screening.Config{
		Concurrency: cfg.AMLConcurrency,
		Timeout:     cfg.AMLTimeout,
		Lease:       cfg.AMLLease,
		BackoffBase: cfg.AMLBackoffBase,
		BackoffMax:  cfg.AMLBackoffMax,
	}, log, wakeSettlement)
	p.settlement = settlement.New(db, settlement.Config{BatchSize: cfg.BatchSize}, log)
	return p, nil
}

func (p *Processor) ScanBlocks(ctx context.Context) (int, error) {
	return p.scanner.Run(ctx)
}

func (p *Processor) ProcessConfirmations(ctx context.Context) (int, error) {
	return p.confirmations.Run(ctx)
}

func (p *Processor) ProcessAML(ctx context.Context) (int, error) {
	return p.screening.Run(ctx)
}

func (p *Processor) ProcessSettlement(ctx context.Context) (int, error) {
	return p.settlement.Run(ctx)
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
