// Package payment turns purchases (a drop-in class or a plan) into orders and settles them with a provider.
package payment

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/bloodandsand/backend/internal/booking"
	"github.com/bloodandsand/backend/internal/catalog"
	"github.com/bloodandsand/backend/internal/membership"
	"github.com/bloodandsand/backend/internal/notify"
	"github.com/bloodandsand/backend/internal/platform/actor"
	"github.com/bloodandsand/backend/internal/platform/apperr"
	"github.com/bloodandsand/backend/internal/platform/db"
	"github.com/bloodandsand/backend/internal/platform/security"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Order struct {
	ID          int64          `json:"id"`
	PublicToken string         `json:"-"`
	UserID      *int64         `json:"userId"`
	Kind        string         `json:"kind"` // plan | class
	PlanID      *int64         `json:"planId"`
	BookingID   *int64         `json:"bookingId"`
	Description string         `json:"description"`
	Amount      int64          `json:"amount"`
	Currency    string         `json:"currency"`
	Status      string         `json:"status"`
	Provider    string         `json:"provider"`
	ProviderRef string         `json:"-"`
	BuyerName   string         `json:"buyerName"`
	BuyerEmail  string         `json:"buyerEmail"`
	BuyerPhone  string         `json:"buyerPhone"`
	Detail      map[string]any `json:"detail,omitempty"`
	PaidAt      *time.Time     `json:"paidAt"`
	CreatedAt   time.Time      `json:"createdAt"`
	// Filled for the result page
	BookingCode *string `json:"bookingCode,omitempty"`
}

const cols = `id, public_token, user_id, kind, plan_id, booking_id, description, amount, currency, status, provider, provider_ref,
	buyer_name, buyer_email, buyer_phone, detail, paid_at, created_at`

func scan(r interface{ Scan(...any) error }) (Order, error) {
	var o Order
	var detail []byte
	err := r.Scan(&o.ID, &o.PublicToken, &o.UserID, &o.Kind, &o.PlanID, &o.BookingID, &o.Description, &o.Amount, &o.Currency, &o.Status,
		&o.Provider, &o.ProviderRef, &o.BuyerName, &o.BuyerEmail, &o.BuyerPhone, &detail, &o.PaidAt, &o.CreatedAt)
	if err == nil && len(detail) > 0 {
		_ = json.Unmarshal(detail, &o.Detail)
	}
	return o, err
}

type Service struct {
	pool      *pgxpool.Pool
	log       *slog.Logger
	bookings  *booking.Service
	members   *membership.Service
	catalog   *catalog.Service
	push      *notify.Service
	providers map[string]Provider
	order     []string
	apiURL    string
	frontURL  string
	hold      time.Duration
}

type Options struct {
	Providers        []string
	WebpayEnv        string
	WebpayCommerce   string
	WebpaySecret     string
	MPAccessToken    string
	MPWebhookSecret  string
	MPSandbox        bool
	PublicAPIURL     string
	FrontendURL      string
	Hold             time.Duration
}

func NewService(pool *pgxpool.Pool, log *slog.Logger, b *booking.Service, m *membership.Service, c *catalog.Service, push *notify.Service, o Options) *Service {
	s := &Service{pool: pool, log: log, bookings: b, members: m, catalog: c, push: push, providers: map[string]Provider{},
		apiURL: o.PublicAPIURL, frontURL: o.FrontendURL, hold: o.Hold}
	for _, name := range o.Providers {
		switch name {
		case "fake":
			s.providers[name] = fakeProvider{}
		case "webpay":
			s.providers[name] = newWebpay(o.WebpayEnv, o.WebpayCommerce, o.WebpaySecret)
		case "mercadopago":
			s.providers[name] = newMercadoPago(o.MPAccessToken, o.MPWebhookSecret, o.MPSandbox)
		}
		s.order = append(s.order, name)
	}
	return s
}

type ProviderInfo struct {
	Name  string `json:"name"`
	Label string `json:"label"`
}

func (s *Service) Providers() []ProviderInfo {
	out := make([]ProviderInfo, 0, len(s.order))
	for _, n := range s.order {
		out = append(out, ProviderInfo{Name: n, Label: s.providers[n].Label()})
	}
	return out
}

func (s *Service) provider(name string) (Provider, error) {
	if name == "" && len(s.order) > 0 {
		name = s.order[0]
	}
	p, ok := s.providers[name]
	if !ok {
		return nil, apperr.Fields(map[string]string{"provider": "Elige un medio de pago disponible."})
	}
	return p, nil
}

func (s *Service) Get(ctx context.Context, q db.Querier, id int64) (Order, error) {
	o, err := scan(q.QueryRow(ctx, `SELECT `+cols+` FROM orders WHERE id = $1`, id))
	if db.NoRows(err) {
		return o, apperr.NotFound("La orden")
	}
	if err == nil && o.BookingID != nil {
		var code string
		if s.pool.QueryRow(ctx, `SELECT code FROM bookings WHERE id = $1`, *o.BookingID).Scan(&code) == nil {
			o.BookingCode = &code
		}
	}
	return o, err
}

// Checkout is returned when a purchase starts: the order plus what the frontend must do to pay.
type Checkout struct {
	Order  Order  `json:"order"`
	Token  string `json:"token"` // lets the buyer (even a guest) poll this order
	Action Action `json:"action"`
}

func (s *Service) insertOrder(ctx context.Context, q db.Querier, o Order) (Order, error) {
	o.PublicToken = security.RandomToken(24)
	return scan(q.QueryRow(ctx, `INSERT INTO orders (public_token, user_id, kind, plan_id, booking_id, description, amount, provider, buyer_name, buyer_email, buyer_phone)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11) RETURNING `+cols,
		o.PublicToken, o.UserID, o.Kind, o.PlanID, o.BookingID, o.Description, o.Amount, o.Provider, o.BuyerName, o.BuyerEmail, o.BuyerPhone))
}

func (s *Service) start(ctx context.Context, p Provider, o Order) (Checkout, error) {
	cb := Callbacks{
		ReturnURL:  fmt.Sprintf("%s/api/payments/%s/return?order=%d", s.apiURL, p.Name(), o.ID),
		WebhookURL: fmt.Sprintf("%s/api/payments/%s/webhook", s.apiURL, p.Name()),
	}
	act, err := p.Start(ctx, o, cb)
	if err != nil {
		s.log.Error("pago: no se pudo iniciar", "provider", p.Name(), "order", o.ID, "err", err)
		_ = s.Fail(ctx, o.ID, "failed", map[string]any{"error": "start_failed"})
		return Checkout{}, apperr.Conflict("provider_unavailable", "No pudimos conectar con "+p.Label()+". Intenta de nuevo o elige otro medio de pago.")
	}
	if act.Ref != "" {
		if _, err := s.pool.Exec(ctx, `UPDATE orders SET provider_ref = $2, updated_at = now() WHERE id = $1`, o.ID, act.Ref); err != nil {
			return Checkout{}, err
		}
		o.ProviderRef = act.Ref
	}
	return Checkout{Order: o, Token: o.PublicToken, Action: act}, nil
}

type ClassCheckoutInput struct {
	booking.BookInput
	Provider string `json:"provider"`
}

// CheckoutClass holds a seat and creates the order to pay a drop-in class. Guests welcome.
func (s *Service) CheckoutClass(ctx context.Context, who *actor.Actor, in ClassCheckoutInput) (Checkout, error) {
	p, err := s.provider(in.Provider)
	if err != nil {
		return Checkout{}, err
	}
	var o Order
	err = db.WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		b, _, err := s.bookings.Hold(ctx, tx, who, in.BookInput)
		if err != nil {
			return err
		}
		o = Order{Kind: "class", BookingID: &b.ID, Amount: b.Amount, Provider: p.Name(), UserID: b.UserID,
			Description: fmt.Sprintf("%s %s %s", b.Discipline, b.Date, b.Start), BuyerName: b.Name, BuyerEmail: b.Email, BuyerPhone: b.Phone}
		if o, err = s.insertOrder(ctx, tx, o); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE bookings SET order_id = $2 WHERE id = $1`, b.ID, o.ID)
		return err
	})
	if err != nil {
		return Checkout{}, err
	}
	return s.start(ctx, p, o)
}

type PlanCheckoutInput struct {
	PlanID   int64  `json:"planId"`
	Provider string `json:"provider"`
}

// CheckoutPlan creates the order to buy a plan. Requires an account (credits belong to a member).
func (s *Service) CheckoutPlan(ctx context.Context, who *actor.Actor, in PlanCheckoutInput) (Checkout, error) {
	p, err := s.provider(in.Provider)
	if err != nil {
		return Checkout{}, err
	}
	plan, err := s.catalog.Plan(ctx, in.PlanID)
	if err != nil {
		return Checkout{}, err
	}
	if !plan.Active || !plan.Sellable {
		return Checkout{}, apperr.Conflict("plan_unavailable", "Este plan no está a la venta.")
	}
	var phone string
	_ = s.pool.QueryRow(ctx, `SELECT phone FROM users WHERE id = $1`, who.ID).Scan(&phone)
	o, err := s.insertOrder(ctx, s.pool, Order{Kind: "plan", PlanID: &plan.ID, Amount: plan.Price, Provider: p.Name(), UserID: &who.ID,
		Description: "Plan " + strings.TrimPrefix(plan.Name, "Plan "), BuyerName: who.Name, BuyerEmail: who.Email, BuyerPhone: phone})
	if err != nil {
		return Checkout{}, err
	}
	return s.start(ctx, p, o)
}

// MarkPaid settles an order: confirms the booking or grants the plan. Safe to call more than once.
func (s *Service) MarkPaid(ctx context.Context, id int64, amount int64, detail map[string]any) (Order, error) {
	var o Order
	var newlyPaid bool
	err := db.WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		var err error
		o, err = scan(tx.QueryRow(ctx, `SELECT `+cols+` FROM orders WHERE id = $1 FOR UPDATE`, id))
		if db.NoRows(err) {
			return apperr.NotFound("La orden")
		}
		if err != nil {
			return err
		}
		if o.Status == "paid" || o.Status == "refunded" {
			return nil
		}
		if amount != o.Amount {
			s.log.Error("pago: monto no coincide", "order", id, "expected", o.Amount, "got", amount)
			return s.setStatus(ctx, tx, &o, "failed", merge(detail, map[string]any{"error": "amount_mismatch"}))
		}
		if o.Status == "expired" || o.Status == "cancelled" || o.Status == "failed" {
			s.log.Warn("pago: llegó un pago para una orden cerrada; se acepta igual", "order", id, "status", o.Status)
		}
		if err := s.setStatus(ctx, tx, &o, "paid", detail); err != nil {
			return err
		}
		newlyPaid = true
		switch o.Kind {
		case "class":
			if o.BookingID != nil {
				_, err = s.bookings.ConfirmPaid(ctx, tx, *o.BookingID, o.ID)
			}
		case "plan":
			if o.UserID == nil || o.PlanID == nil {
				return errors.New("orden de plan sin usuario o plan")
			}
			var plan catalog.Plan
			if plan, err = s.catalog.Plan(ctx, *o.PlanID); err == nil {
				_, err = s.members.Grant(ctx, tx, *o.UserID, plan, &o.ID)
			}
		}
		return err
	})
	if err != nil {
		return o, err
	}
	if newlyPaid {
		o, _ = s.Get(ctx, s.pool, id)
		to := notify.Recipient{UserID: o.UserID, Email: o.BuyerEmail}
		if o.Kind == "plan" {
			s.push.Notify(to, notify.Message{Title: "Plan activo", Body: o.Description + " pagado. Ya puedes reservar tus clases.", URL: "/reservar"})
		} else {
			s.push.Notify(to, notify.Message{Title: "Pago recibido", Body: fmt.Sprintf("Tu clase quedó confirmada. Código %s.", deref(o.BookingCode)), URL: "/mis-reservas"})
		}
	}
	return o, nil
}

// Fail closes a pending order (failed | cancelled | expired) and frees the held seat.
func (s *Service) Fail(ctx context.Context, id int64, status string, detail map[string]any) error {
	return db.WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		o, err := scan(tx.QueryRow(ctx, `SELECT `+cols+` FROM orders WHERE id = $1 FOR UPDATE`, id))
		if err != nil {
			return err
		}
		if o.Status != "pending" {
			return nil
		}
		if err := s.setStatus(ctx, tx, &o, status, detail); err != nil {
			return err
		}
		if o.BookingID != nil {
			return s.bookings.ReleaseHold(ctx, tx, *o.BookingID)
		}
		return nil
	})
}

func (s *Service) setStatus(ctx context.Context, q db.Querier, o *Order, status string, detail map[string]any) error {
	o.Detail = merge(o.Detail, detail)
	raw, _ := json.Marshal(o.Detail)
	_, err := q.Exec(ctx, `UPDATE orders SET status = $2, detail = $3, paid_at = CASE WHEN $2 = 'paid' THEN now() ELSE paid_at END, updated_at = now() WHERE id = $1`,
		o.ID, status, raw)
	o.Status = status
	return err
}

// Refund (admin) returns the money through the provider when possible, then undoes the purchase.
func (s *Service) Refund(ctx context.Context, id int64, note string) (Order, error) {
	o, err := s.Get(ctx, s.pool, id)
	if err != nil {
		return o, err
	}
	if o.Status != "paid" {
		return o, apperr.Conflict("not_paid", "Solo se pueden reembolsar órdenes pagadas.")
	}
	manual := false
	if p, ok := s.providers[o.Provider]; ok {
		if err := p.Refund(ctx, o); err != nil {
			if !errors.Is(err, ErrRefundUnsupported) {
				s.log.Error("reembolso falló", "order", id, "err", err)
				return o, apperr.Conflict("refund_failed", "El proveedor rechazó el reembolso. Revisa la transacción en su portal.")
			}
			manual = true
		}
	} else {
		manual = true
	}
	err = db.WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		if err := s.setStatus(ctx, tx, &o, "refunded", map[string]any{"refundNote": note, "refundManual": manual, "refundedAt": time.Now()}); err != nil {
			return err
		}
		if o.BookingID != nil {
			if _, err := tx.Exec(ctx, `UPDATE bookings SET status = 'cancelled', cancelled_at = now() WHERE id = $1 AND status IN ('confirmed', 'pending_payment')`, *o.BookingID); err != nil {
				return err
			}
		}
		_, err := tx.Exec(ctx, `UPDATE memberships SET status = 'cancelled' WHERE order_id = $1`, o.ID)
		return err
	})
	return o, err
}

// ExpireStale closes orders whose seat hold ran out without payment.
func (s *Service) ExpireStale(ctx context.Context) (int, error) {
	rows, err := s.pool.Query(ctx, `SELECT id FROM orders WHERE status = 'pending' AND created_at < now() - $1 * interval '1 second'`, int(s.hold.Seconds())+60)
	if err != nil {
		return 0, err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		return 0, err
	}
	for _, id := range ids {
		if err := s.Fail(ctx, id, "expired", map[string]any{"reason": "timeout"}); err != nil {
			return 0, err
		}
	}
	return len(ids), nil
}

type ListFilter struct {
	Status, Kind, Q string
	UserID          int64
	Limit, Offset   int
}

func (s *Service) List(ctx context.Context, f ListFilter) ([]Order, int, error) {
	where := `WHERE ($1 = '' OR status = $1) AND ($2 = '' OR kind = $2) AND ($3 = 0 OR user_id = $3)
		AND ($4 = '' OR buyer_name ILIKE '%' || $4 || '%' OR buyer_email ILIKE '%' || $4 || '%' OR description ILIKE '%' || $4 || '%')`
	args := []any{f.Status, f.Kind, f.UserID, strings.TrimSpace(f.Q)}
	var total int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM orders `+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := s.pool.Query(ctx, `SELECT `+cols+` FROM orders `+where+` ORDER BY created_at DESC LIMIT $5 OFFSET $6`, append(args, f.Limit, f.Offset)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []Order{}
	for rows.Next() {
		o, err := scan(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, o)
	}
	return out, total, rows.Err()
}

func merge(a, b map[string]any) map[string]any {
	if a == nil {
		a = map[string]any{}
	}
	for k, v := range b {
		a[k] = v
	}
	return a
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
