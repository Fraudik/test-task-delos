package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"test-task-dellos/internal/app"
	"test-task-dellos/internal/postgres"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(log); err != nil {
		log.Error("server stopped", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, err := app.LoadConfig(os.Getenv("CONFIG_PATH"))
	if err != nil {
		return err
	}
	db, err := postgres.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer db.Close()
	if err := postgres.Migrate(ctx, db); err != nil {
		return err
	}

	a, err := app.New(ctx, db, cfg, log)
	if err != nil {
		return err
	}
	srv := &http.Server{Addr: cfg.HTTPAddr, Handler: a.Handler, ReadHeaderTimeout: 5 * time.Second}

	workersDone := make(chan struct{})
	go func() {
		a.Run(ctx)
		close(workersDone)
	}()
	serveErr := make(chan error, 1)
	go func() {
		log.Info("http server listening", "addr", cfg.HTTPAddr)
		serveErr <- srv.ListenAndServe()
	}()

	select {
	case <-ctx.Done():
	case err := <-serveErr:
		stop()
		<-workersDone
		return err
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err = srv.Shutdown(shutdownCtx)
	<-workersDone
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
