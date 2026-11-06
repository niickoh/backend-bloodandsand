// Command api runs the Blood & Sand HTTP API and its background workers.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
	_ "time/tzdata" // embed timezone data so America/Santiago works on minimal servers

	"github.com/bloodandsand/backend/internal/app"
	"github.com/bloodandsand/backend/internal/config"
	"github.com/joho/godotenv"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run() error {
	migrateOnly := flag.Bool("migrate", false, "aplica migraciones y termina")
	seedDemo := flag.Bool("seed-demo", false, "carga coaches, alumno y horario de ejemplo (solo desarrollo)")
	flag.Parse()

	_ = godotenv.Load() // .env is optional; real environment variables win
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	var handler slog.Handler = slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelDebug})
	if cfg.IsProduction() {
		handler = slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})
	}
	log := slog.New(handler)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	a, err := app.New(ctx, cfg, log)
	if err != nil {
		return err
	}
	defer a.Close()

	if *seedDemo {
		if cfg.IsProduction() {
			return errors.New("-seed-demo no se puede usar en producción")
		}
		if err := a.SeedDemo(ctx); err != nil {
			return err
		}
	}
	if *migrateOnly {
		log.Info("migraciones al día")
		return nil
	}

	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           a.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       90 * time.Second,
		ErrorLog:          slog.NewLogLogger(handler, slog.LevelWarn),
	}

	workersDone := make(chan struct{})
	go func() {
		a.RunWorkers(ctx)
		close(workersDone)
	}()

	errCh := make(chan error, 1)
	go func() {
		log.Info("API escuchando", "addr", cfg.Addr, "env", cfg.Env, "pagos", cfg.PaymentProviders)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
		close(errCh)
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
	}
	log.Info("apagando…")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return err
	}
	<-workersDone
	return nil
}
