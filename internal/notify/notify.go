// Package notify stores Web Push subscriptions and sends notifications to them.
package notify

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	webpush "github.com/SherClockHolmes/webpush-go"
	"github.com/bloodandsand/backend/internal/platform/apperr"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Message is what a person sees. URL is opened when they tap it.
type Message struct {
	Title string
	Body  string
	URL   string
	Tag   string
}

// Recipient is a member (UserID) or a guest identified by the email they booked with.
type Recipient struct {
	UserID *int64
	Email  string
}

type Service struct {
	pool            *pgxpool.Pool
	log             *slog.Logger
	publicKey       string
	privateKey      string
	subject         string
	sem             chan struct{}
	wg              sync.WaitGroup
}

func NewService(pool *pgxpool.Pool, log *slog.Logger, publicKey, privateKey, subject string) *Service {
	return &Service{pool: pool, log: log, publicKey: publicKey, privateKey: privateKey, subject: subject, sem: make(chan struct{}, 8)}
}

func (s *Service) Enabled() bool   { return s.publicKey != "" && s.privateKey != "" }
func (s *Service) PublicKey() string { return s.publicKey }

type SubscriptionInput struct {
	Endpoint string `json:"endpoint"`
	Keys     struct {
		P256dh string `json:"p256dh"`
		Auth   string `json:"auth"`
	} `json:"keys"`
	Email string `json:"email"` // guests only
}

func (s *Service) Subscribe(ctx context.Context, userID *int64, in SubscriptionInput, userAgent string) error {
	if !strings.HasPrefix(in.Endpoint, "https://") || in.Keys.P256dh == "" || in.Keys.Auth == "" {
		return apperr.Invalid("bad_subscription", "La suscripción de notificaciones no es válida.")
	}
	if len(userAgent) > 255 {
		userAgent = userAgent[:255]
	}
	email := strings.ToLower(strings.TrimSpace(in.Email))
	_, err := s.pool.Exec(ctx, `INSERT INTO push_subscriptions (user_id, email, endpoint, p256dh, auth, user_agent)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (endpoint) DO UPDATE SET user_id = COALESCE(EXCLUDED.user_id, push_subscriptions.user_id),
			email = CASE WHEN EXCLUDED.email <> '' THEN EXCLUDED.email ELSE push_subscriptions.email END,
			p256dh = EXCLUDED.p256dh, auth = EXCLUDED.auth, user_agent = EXCLUDED.user_agent`,
		userID, email, in.Endpoint, in.Keys.P256dh, in.Keys.Auth, userAgent)
	return err
}

func (s *Service) Unsubscribe(ctx context.Context, endpoint string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM push_subscriptions WHERE endpoint = $1`, endpoint)
	return err
}

type sub struct {
	id                     int64
	endpoint, p256dh, auth string
}

func (s *Service) subsFor(ctx context.Context, where string, args ...any) ([]sub, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, endpoint, p256dh, auth FROM push_subscriptions WHERE `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []sub
	for rows.Next() {
		var x sub
		if err := rows.Scan(&x.id, &x.endpoint, &x.p256dh, &x.auth); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// Notify sends to one person in the background. Errors are logged, never returned:
// a failed notification must not fail the booking that triggered it.
func (s *Service) Notify(to Recipient, m Message) {
	if !s.Enabled() {
		return
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		var subs []sub
		var err error
		if to.UserID != nil {
			subs, err = s.subsFor(ctx, `user_id = $1 OR ($2 <> '' AND user_id IS NULL AND lower(email) = lower($2))`, *to.UserID, to.Email)
		} else if to.Email != "" {
			subs, err = s.subsFor(ctx, `lower(email) = lower($1)`, to.Email)
		}
		if err != nil {
			s.log.Error("push: buscar suscripciones", "err", err)
			return
		}
		s.sendAll(ctx, subs, m)
	}()
}

// Broadcast sends to an audience: all | students | coaches. Returns how many devices were targeted.
func (s *Service) Broadcast(ctx context.Context, audience string, m Message) (int, error) {
	if !s.Enabled() {
		return 0, apperr.Conflict("push_disabled", "Las notificaciones push no están configuradas (faltan las claves VAPID).")
	}
	where := `TRUE`
	switch audience {
	case "students":
		where = `user_id IS NULL OR user_id IN (SELECT id FROM users WHERE role = 'student' AND active)`
	case "coaches":
		where = `user_id IN (SELECT id FROM users WHERE role IN ('coach', 'admin') AND active)`
	}
	subs, err := s.subsFor(ctx, where)
	if err != nil {
		return 0, err
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		bg, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		s.sendAll(bg, subs, m)
	}()
	return len(subs), nil
}

// payload follows the format Angular's service worker (ngsw) understands.
func payload(m Message) []byte {
	url := m.URL
	if url == "" {
		url = "/"
	}
	b, _ := json.Marshal(map[string]any{
		"notification": map[string]any{
			"title": m.Title,
			"body":  m.Body,
			"icon":  "icons/icon-192x192.png",
			"badge": "icons/icon-96x96.png",
			"tag":   m.Tag,
			"data": map[string]any{
				"url":           url,
				"onActionClick": map[string]any{"default": map[string]any{"operation": "navigateLastFocusedOrOpen", "url": url}},
			},
		},
	})
	return b
}

func (s *Service) sendAll(ctx context.Context, subs []sub, m Message) {
	body := payload(m)
	var wg sync.WaitGroup
	for _, x := range subs {
		wg.Add(1)
		s.sem <- struct{}{}
		go func() {
			defer func() { <-s.sem; wg.Done() }()
			resp, err := webpush.SendNotificationWithContext(ctx, body, &webpush.Subscription{
				Endpoint: x.endpoint, Keys: webpush.Keys{P256dh: x.p256dh, Auth: x.auth},
			}, &webpush.Options{
				Subscriber: s.subject, VAPIDPublicKey: s.publicKey, VAPIDPrivateKey: s.privateKey,
				TTL: 3600, Urgency: webpush.UrgencyHigh,
			})
			if err != nil {
				s.log.Warn("push: envío falló", "err", err)
				return
			}
			defer resp.Body.Close()
			switch {
			case resp.StatusCode == http.StatusGone || resp.StatusCode == http.StatusNotFound:
				_, _ = s.pool.Exec(ctx, `DELETE FROM push_subscriptions WHERE id = $1`, x.id)
			case resp.StatusCode >= 400:
				s.log.Warn("push: rechazado", "status", resp.StatusCode)
			default:
				_, _ = s.pool.Exec(ctx, `UPDATE push_subscriptions SET last_used_at = now() WHERE id = $1`, x.id)
			}
		}()
	}
	wg.Wait()
}

// Wait blocks until in-flight notifications finish (used on shutdown and in tests).
func (s *Service) Wait() { s.wg.Wait() }
