// Package app wires modules together: config → services → HTTP routes → background workers.
package app

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/bloodandsand/backend/internal/auth"
	"github.com/bloodandsand/backend/internal/booking"
	"github.com/bloodandsand/backend/internal/catalog"
	"github.com/bloodandsand/backend/internal/config"
	"github.com/bloodandsand/backend/internal/dashboard"
	"github.com/bloodandsand/backend/internal/membership"
	"github.com/bloodandsand/backend/internal/notify"
	"github.com/bloodandsand/backend/internal/payment"
	"github.com/bloodandsand/backend/internal/platform/db"
	"github.com/bloodandsand/backend/internal/platform/httpx"
	"github.com/bloodandsand/backend/internal/schedule"
	"github.com/bloodandsand/backend/internal/user"
	"github.com/bloodandsand/backend/migrations"
	"github.com/jackc/pgx/v5/pgxpool"
)

type App struct {
	Cfg  config.Config
	Log  *slog.Logger
	Pool *pgxpool.Pool

	Users      *user.Service
	Auth       *auth.Service
	AuthHTTP   *auth.Handler
	Catalog    *catalog.Service
	Schedule   *schedule.Service
	Members    *membership.Service
	Push       *notify.Service
	Bookings   *booking.Service
	Payments   *payment.Service
	Dashboard  *dashboard.Service
}

// New connects to the database, runs migrations and builds every service.
func New(ctx context.Context, cfg config.Config, log *slog.Logger) (*App, error) {
	pool, err := db.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return nil, err
	}
	if err := db.Migrate(ctx, pool, migrations.FS, log); err != nil {
		pool.Close()
		return nil, err
	}

	a := &App{Cfg: cfg, Log: log, Pool: pool}
	a.Users = user.NewService(pool)
	a.Auth = auth.NewService(pool, a.Users, cfg.JWTSecret, cfg.AccessTTL, cfg.RefreshTTL)
	a.AuthHTTP = auth.NewHandler(a.Auth, a.Users, log, cfg.CookieSecure, cfg.CookieDomain)
	a.Catalog = catalog.NewService(pool)
	a.Schedule = schedule.NewService(pool, cfg.Timezone)
	a.Members = membership.NewService(pool, a.Catalog)
	a.Push = notify.NewService(pool, log, cfg.VAPIDPublicKey, cfg.VAPIDPrivateKey, cfg.VAPIDSubject)
	a.Bookings = booking.NewService(pool, log, a.Schedule, a.Members, a.Push, booking.Options{
		Hold: cfg.BookingHold, CancelLimit: cfg.CancelLimit, ReminderBefore: cfg.ReminderBefore,
	})
	a.Payments = payment.NewService(pool, log, a.Bookings, a.Members, a.Catalog, a.Push, payment.Options{
		Providers: cfg.PaymentProviders, WebpayEnv: cfg.WebpayEnv, WebpayCommerce: cfg.WebpayCommerce, WebpaySecret: cfg.WebpayAPISecret,
		MPAccessToken: cfg.MPAccessToken, MPWebhookSecret: cfg.MPWebhookSecret, MPSandbox: cfg.MPSandbox,
		PublicAPIURL: cfg.PublicAPIURL, FrontendURL: cfg.FrontendURL, Hold: cfg.BookingHold,
	})
	a.Dashboard = dashboard.NewService(pool, cfg.Timezone)

	if created, err := a.Users.EnsureAdmin(ctx, cfg.AdminEmail, cfg.AdminPassword); err != nil {
		log.Warn("no se pudo crear el admin inicial", "err", err)
	} else if created {
		log.Info("admin inicial creado", "email", cfg.AdminEmail)
	}
	if !a.Push.Enabled() {
		log.Warn("push desactivado: define VAPID_PUBLIC_KEY y VAPID_PRIVATE_KEY (genéralas con: go run ./cmd/vapidgen)")
	}
	return a, nil
}

func (a *App) Close() {
	a.Push.Wait()
	a.Pool.Close()
}

// Handler builds the full HTTP stack.
func (a *App) Handler() http.Handler {
	mux := http.NewServeMux()
	rt := httpx.Router{Mux: mux, Log: a.Log, Guard: a.AuthHTTP.Require}

	mux.HandleFunc("GET /api/health", func(w http.ResponseWriter, r *http.Request) {
		if err := a.Pool.Ping(r.Context()); err != nil {
			http.Error(w, `{"status":"db_down"}`, http.StatusServiceUnavailable)
			return
		}
		_ = httpx.OK(w, map[string]string{"status": "ok"})
	})

	a.AuthHTTP.Routes(mux)
	a.Users.Routes(rt)
	a.Catalog.Routes(rt)
	a.Schedule.Routes(rt, a.Cfg.GenerateDaysAhead)
	a.Members.Routes(rt, a.Cfg.Timezone)
	a.Bookings.Routes(rt)
	a.Payments.Routes(rt)
	a.Push.Routes(rt)
	a.Dashboard.Routes(rt)

	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		_ = httpx.JSON(w, http.StatusNotFound, map[string]string{"error": "not_found", "message": "La ruta no existe."})
	})

	return httpx.Chain(mux,
		httpx.Recover(a.Log),
		httpx.Logger(a.Log),
		httpx.SecureHeaders,
		httpx.CORS(a.Cfg.AllowedOrigin),
		httpx.RequireJSONForWrites(payment.CallbackPrefix),
		a.AuthHTTP.Authenticate,
	)
}
