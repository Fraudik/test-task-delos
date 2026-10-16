package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"sync"

	"github.com/jackc/pgx/v5/pgxpool"

	"test-task-dellos/internal/aml/mock"
	"test-task-dellos/internal/api"
	"test-task-dellos/internal/api/apidb"
	"test-task-dellos/internal/chain/simulator"
	"test-task-dellos/internal/money"
	"test-task-dellos/internal/notify"
	"test-task-dellos/internal/processing"
	"test-task-dellos/internal/transfers"
)

type PartnerConfig struct {
	Name     string `json:"name"`
	APIKey   string `json:"api_key"`
	FeeBps   int32  `json:"fee_bps"`
	FeeFixed string `json:"fee_fixed"`
}

type Config struct {
	DatabaseURL   string          `json:"database_url"`
	HTTPAddr      string          `json:"http_addr"`
	Confirmations uint64          `json:"confirmations"`
	AMLBlacklist  []string        `json:"aml_blacklist"`
	SimStatePath  string          `json:"sim_state_path"`
	Partners      []PartnerConfig `json:"partners"`
}

func LoadConfig(path string) (Config, error) {
	cfg := Config{
		DatabaseURL:   "postgres://dellos:dellos@localhost:5432/dellos?sslmode=disable",
		HTTPAddr:      ":8080",
		Confirmations: 5,
	}
	if path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return Config{}, fmt.Errorf("app: read config: %w", err)
		}
		if err := json.Unmarshal(data, &cfg); err != nil {
			return Config{}, fmt.Errorf("app: parse config %s: %w", path, err)
		}
	}
	if v := os.Getenv("DATABASE_URL"); v != "" {
		cfg.DatabaseURL = v
	}
	if v := os.Getenv("SIM_STATE_PATH"); v != "" {
		cfg.SimStatePath = v
	}
	if v := os.Getenv("HTTP_ADDR"); v != "" {
		cfg.HTTPAddr = v
	}
	if v := os.Getenv("CONFIRMATIONS"); v != "" {
		n, err := strconv.ParseUint(v, 10, 64)
		if err != nil {
			return Config{}, fmt.Errorf("app: CONFIRMATIONS: %w", err)
		}
		cfg.Confirmations = n
	}
	return cfg, nil
}

type App struct {
	Handler   http.Handler
	Processor *processing.Processor
	Notifier  *notify.Notifier
	Transfers *transfers.Transfers
	Simulator *simulator.Simulator
	AML       *mock.Mock
}

func New(ctx context.Context, db *pgxpool.Pool, cfg Config, log *slog.Logger) (*App, error) {
	if log == nil {
		log = slog.Default()
	}
	if err := seedPartners(ctx, db, cfg.Partners); err != nil {
		return nil, err
	}

	sim := simulator.New()
	var saveSim func() error
	if cfg.SimStatePath != "" {
		loaded, err := simulator.LoadFile(cfg.SimStatePath)
		if err != nil {
			return nil, err
		}
		sim = loaded
		saveSim = func() error { return sim.SaveFile(cfg.SimStatePath) }
	}
	checker := mock.New(cfg.AMLBlacklist)
	pcfg := processing.DefaultConfig()
	pcfg.Confirmations = cfg.Confirmations
	proc, err := processing.New(db, sim, checker, pcfg, log)
	if err != nil {
		return nil, err
	}
	notifier := notify.New(db, notify.LogNotifier{Log: log.With("component", "partner-gateway")}, notify.DefaultConfig(), log)
	outgoing := transfers.New(db, transfers.LogInitiator{Log: log.With("component", "wallet")}, transfers.DefaultConfig(), log)

	return &App{
		Handler:   api.New(db, log, api.WithSimulator(sim, checker, saveSim)).Handler(),
		Processor: proc,
		Notifier:  notifier,
		Transfers: outgoing,
		Simulator: sim,
		AML:       checker,
	}, nil
}

func (a *App) Run(ctx context.Context) {
	var wg sync.WaitGroup
	wg.Go(func() { a.Processor.Run(ctx) })
	wg.Go(func() { a.Notifier.Run(ctx) })
	wg.Go(func() { a.Transfers.Run(ctx) })
	wg.Wait()
}

func (a *App) Tick(ctx context.Context) error {
	perr := a.Processor.Tick(ctx)
	_, nerr := a.Notifier.Tick(ctx)
	_, terr := a.Transfers.Tick(ctx)
	return errors.Join(perr, nerr, terr)
}

func seedPartners(ctx context.Context, db *pgxpool.Pool, partners []PartnerConfig) error {
	q := apidb.New(db)
	for _, p := range partners {
		if p.Name == "" || p.APIKey == "" {
			return fmt.Errorf("app: partner %q: name and api_key are required", p.Name)
		}
		feeFixed := money.Amount(0)
		if p.FeeFixed != "" {
			var err error
			if feeFixed, err = money.Parse(p.FeeFixed); err != nil {
				return fmt.Errorf("app: partner %q fee_fixed: %w", p.Name, err)
			}
		}
		_, err := q.UpsertPartner(ctx, apidb.UpsertPartnerParams{
			Name:       p.Name,
			FeeBps:     p.FeeBps,
			FeeFixed:   feeFixed,
			ApiKeyHash: api.HashAPIKey(p.APIKey),
		})
		if err != nil {
			return fmt.Errorf("app: seed partner %q: %w", p.Name, err)
		}
	}
	return nil
}
