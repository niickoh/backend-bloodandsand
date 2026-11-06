// Package config loads settings from environment variables (and an optional .env file).
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Env  string // development | production
	Addr string

	DatabaseURL string

	JWTSecret     string
	AccessTTL     time.Duration
	RefreshTTL    time.Duration
	CookieSecure  bool
	CookieDomain  string
	AllowedOrigin []string

	FrontendURL  string // where users are sent back after paying, e.g. https://bloodandsand.cl
	PublicAPIURL string // public base of this API, used for payment callbacks

	Timezone *time.Location

	AdminEmail    string
	AdminPassword string

	PaymentProviders []string // fake, webpay, mercadopago

	WebpayEnv         string // integration | production
	WebpayCommerce    string
	WebpayAPISecret   string
	MPAccessToken     string
	MPWebhookSecret   string
	MPSandbox         bool

	VAPIDPublicKey  string
	VAPIDPrivateKey string
	VAPIDSubject    string

	BookingHold      time.Duration // seat held while a payment is pending
	CancelLimit      time.Duration // members recover the credit when cancelling earlier than this
	ReminderBefore   time.Duration
	GenerateDaysAhead int
}

func (c Config) IsProduction() bool { return c.Env == "production" }

func Load() (Config, error) {
	loc, err := time.LoadLocation(get("TZ_GYM", "America/Santiago"))
	if err != nil {
		return Config{}, fmt.Errorf("TZ_GYM: %w", err)
	}
	c := Config{
		Env:               get("APP_ENV", "development"),
		Addr:              ":" + get("PORT", "8080"),
		DatabaseURL:       get("DATABASE_URL", ""),
		JWTSecret:         get("JWT_SECRET", ""),
		AccessTTL:         duration("ACCESS_TTL", 15*time.Minute),
		RefreshTTL:        duration("REFRESH_TTL", 30*24*time.Hour),
		CookieDomain:      get("COOKIE_DOMAIN", ""),
		AllowedOrigin:     list("CORS_ORIGINS", "http://localhost:4200"),
		FrontendURL:       strings.TrimRight(get("FRONTEND_URL", "http://localhost:4200"), "/"),
		PublicAPIURL:      strings.TrimRight(get("PUBLIC_API_URL", "http://localhost:8080"), "/"),
		Timezone:          loc,
		AdminEmail:        get("ADMIN_EMAIL", ""),
		AdminPassword:     get("ADMIN_PASSWORD", ""),
		PaymentProviders:  list("PAYMENT_PROVIDERS", "fake"),
		WebpayEnv:         get("WEBPAY_ENV", "integration"),
		WebpayCommerce:    get("WEBPAY_COMMERCE_CODE", "597055555532"),
		WebpayAPISecret:   get("WEBPAY_API_SECRET", "579B532A7440BB0C9079DED94D31EA1615BACEB56610332264630D42D0A36B1C"),
		MPAccessToken:     get("MP_ACCESS_TOKEN", ""),
		MPWebhookSecret:   get("MP_WEBHOOK_SECRET", ""),
		MPSandbox:         boolean("MP_SANDBOX", true),
		VAPIDPublicKey:    get("VAPID_PUBLIC_KEY", ""),
		VAPIDPrivateKey:   get("VAPID_PRIVATE_KEY", ""),
		VAPIDSubject:      get("VAPID_SUBJECT", "mailto:hola@bloodandsand.cl"),
		BookingHold:       duration("BOOKING_HOLD", 15*time.Minute),
		CancelLimit:       duration("CANCEL_LIMIT", 3*time.Hour),
		ReminderBefore:    duration("REMINDER_BEFORE", 2*time.Hour),
		GenerateDaysAhead: integer("GENERATE_DAYS_AHEAD", 21),
	}
	c.CookieSecure = boolean("COOKIE_SECURE", c.IsProduction())

	var errs []error
	if c.DatabaseURL == "" {
		errs = append(errs, errors.New("DATABASE_URL es obligatorio"))
	}
	if len(c.JWTSecret) < 32 {
		errs = append(errs, errors.New("JWT_SECRET debe tener al menos 32 caracteres"))
	}
	for _, p := range c.PaymentProviders {
		switch p {
		case "fake", "webpay", "mercadopago":
		default:
			errs = append(errs, fmt.Errorf("PAYMENT_PROVIDERS: proveedor desconocido %q", p))
		}
		if p == "mercadopago" && c.MPAccessToken == "" {
			errs = append(errs, errors.New("MP_ACCESS_TOKEN es obligatorio para mercadopago"))
		}
		if p == "fake" && c.IsProduction() {
			errs = append(errs, errors.New("el proveedor fake no se puede usar en producción"))
		}
	}
	return c, errors.Join(errs...)
}

func get(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	return def
}

func list(key, def string) []string {
	var out []string
	for _, p := range strings.Split(get(key, def), ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func boolean(key string, def bool) bool {
	v, err := strconv.ParseBool(get(key, strconv.FormatBool(def)))
	if err != nil {
		return def
	}
	return v
}

func integer(key string, def int) int {
	v, err := strconv.Atoi(get(key, strconv.Itoa(def)))
	if err != nil {
		return def
	}
	return v
}

func duration(key string, def time.Duration) time.Duration {
	v, err := time.ParseDuration(get(key, def.String()))
	if err != nil {
		return def
	}
	return v
}
