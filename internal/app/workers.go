package app

import (
	"context"
	"time"
)

// RunWorkers starts the periodic jobs and blocks until ctx is cancelled.
// Every job is idempotent and claims rows atomically, so running two instances is safe.
func (a *App) RunWorkers(ctx context.Context) {
	jobs := []struct {
		name  string
		every time.Duration
		run   func(context.Context) (int, error)
	}{
		{"generar-clases", time.Hour, func(ctx context.Context) (int, error) { return a.Schedule.Generate(ctx, a.Cfg.GenerateDaysAhead) }},
		{"expirar-pagos", time.Minute, a.Payments.ExpireStale},
		{"recordatorios", time.Minute, a.Bookings.SendReminders},
		{"limpiar-sesiones", 24 * time.Hour, func(ctx context.Context) (int, error) { return 0, a.Auth.PurgeExpired(ctx) }},
	}
	done := make(chan struct{})
	for _, j := range jobs {
		go func() {
			defer func() { done <- struct{}{} }()
			tick := time.NewTicker(j.every)
			defer tick.Stop()
			for {
				runCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
				n, err := j.run(runCtx)
				cancel()
				if err != nil && ctx.Err() == nil {
					a.Log.Error("worker falló", "job", j.name, "err", err)
				} else if n > 0 {
					a.Log.Info("worker", "job", j.name, "items", n)
				}
				select {
				case <-ctx.Done():
					return
				case <-tick.C:
				}
			}
		}()
	}
	for range jobs {
		<-done
	}
}
