// Package membership tracks the plans members bought and their remaining class credits.
package membership

import (
	"context"
	"time"

	"github.com/bloodandsand/backend/internal/catalog"
	"github.com/bloodandsand/backend/internal/platform/apperr"
	"github.com/bloodandsand/backend/internal/platform/db"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Membership struct {
	ID           int64     `json:"id"`
	UserID       int64     `json:"userId"`
	PlanID       *int64    `json:"planId"`
	PlanName     string    `json:"planName"`
	CreditsTotal *int      `json:"creditsTotal"`
	CreditsLeft  *int      `json:"creditsLeft"` // nil = unlimited
	StartsAt     time.Time `json:"startsAt"`
	EndsAt       time.Time `json:"endsAt"`
	Status       string    `json:"status"`
	OrderID      *int64    `json:"orderId"`
	CreatedAt    time.Time `json:"createdAt"`
}

// HasCredit reports whether a class can be booked with it right now.
func (m *Membership) HasCredit(at time.Time) bool {
	return m != nil && m.Status == "active" && !at.After(m.EndsAt) && (m.CreditsLeft == nil || *m.CreditsLeft > 0)
}

const cols = `id, user_id, plan_id, plan_name, credits_total, credits_left, starts_at, ends_at, status, order_id, created_at`

func scan(r interface{ Scan(...any) error }) (*Membership, error) {
	var m Membership
	err := r.Scan(&m.ID, &m.UserID, &m.PlanID, &m.PlanName, &m.CreditsTotal, &m.CreditsLeft, &m.StartsAt, &m.EndsAt, &m.Status, &m.OrderID, &m.CreatedAt)
	return &m, err
}

type Service struct {
	pool    *pgxpool.Pool
	catalog *catalog.Service
}

func NewService(pool *pgxpool.Pool, cat *catalog.Service) *Service { return &Service{pool: pool, catalog: cat} }

// Current returns the user's active, unexpired membership (the newest), or nil.
func (s *Service) Current(ctx context.Context, q db.Querier, userID int64, lock bool) (*Membership, error) {
	sql := `SELECT ` + cols + ` FROM memberships WHERE user_id = $1 AND status = 'active' AND ends_at > now() ORDER BY ends_at DESC LIMIT 1`
	if lock {
		sql += ` FOR UPDATE`
	}
	m, err := scan(q.QueryRow(ctx, sql, userID))
	if db.NoRows(err) {
		return nil, nil
	}
	return m, err
}

func (s *Service) ForUser(ctx context.Context, userID int64) ([]Membership, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+cols+` FROM memberships WHERE user_id = $1 ORDER BY created_at DESC LIMIT 50`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Membership{}
	for rows.Next() {
		m, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *m)
	}
	return out, rows.Err()
}

// Grant starts a plan for a user, replacing any active one. Used by payments and by admins.
func (s *Service) Grant(ctx context.Context, q db.Querier, userID int64, plan catalog.Plan, orderID *int64) (*Membership, error) {
	if _, err := q.Exec(ctx, `UPDATE memberships SET status = 'cancelled' WHERE user_id = $1 AND status = 'active'`, userID); err != nil {
		return nil, err
	}
	now := time.Now()
	return scan(q.QueryRow(ctx, `INSERT INTO memberships (user_id, plan_id, plan_name, credits_total, credits_left, starts_at, ends_at, order_id)
		VALUES ($1, $2, $3, $4, $4, $5, $6, $7) RETURNING `+cols,
		userID, plan.ID, plan.Name, plan.Credits, now, now.AddDate(0, 0, plan.PeriodDays), orderID))
}

// UseCredit takes one class from a credit plan. Unlimited plans are untouched.
func (s *Service) UseCredit(ctx context.Context, q db.Querier, id int64) error {
	tag, err := q.Exec(ctx, `UPDATE memberships SET credits_left = credits_left - 1 WHERE id = $1 AND credits_left IS NOT NULL AND credits_left > 0`, id)
	if err != nil {
		return err
	}
	var unlimited bool
	if tag.RowsAffected() == 0 {
		if err := q.QueryRow(ctx, `SELECT credits_left IS NULL FROM memberships WHERE id = $1`, id).Scan(&unlimited); err != nil {
			return err
		}
		if !unlimited {
			return apperr.Conflict("no_credits", "Ya usaste todas las clases de tu plan.")
		}
	}
	return nil
}

// RefundCredit returns one class to a credit plan (never above the total).
func (s *Service) RefundCredit(ctx context.Context, q db.Querier, id int64) error {
	_, err := q.Exec(ctx, `UPDATE memberships SET credits_left = LEAST(credits_total, credits_left + 1) WHERE id = $1 AND credits_left IS NOT NULL`, id)
	return err
}

type AssignInput struct {
	PlanID      int64   `json:"planId"`
	CreditsLeft *int    `json:"creditsLeft"` // optional override
	EndsAt      *string `json:"endsAt"`      // optional yyyy-mm-dd
}

// Assign lets an admin give a plan without payment (cash at the front desk, courtesy, etc.).
func (s *Service) Assign(ctx context.Context, userID int64, in AssignInput, loc *time.Location) (*Membership, error) {
	plan, err := s.catalog.Plan(ctx, in.PlanID)
	if err != nil {
		return nil, err
	}
	var m *Membership
	err = db.WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		var err error
		if m, err = s.Grant(ctx, tx, userID, plan, nil); err != nil {
			if db.IsForeignKeyViolation(err) {
				return apperr.NotFound("El usuario")
			}
			return err
		}
		return s.applyOverrides(ctx, tx, m, in.CreditsLeft, in.EndsAt, loc)
	})
	return m, err
}

type AdjustInput struct {
	CreditsLeft *int    `json:"creditsLeft"`
	EndsAt      *string `json:"endsAt"`
	Status      *string `json:"status"` // active | cancelled
}

func (s *Service) Adjust(ctx context.Context, id int64, in AdjustInput, loc *time.Location) (*Membership, error) {
	var m *Membership
	err := db.WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		var err error
		m, err = scan(tx.QueryRow(ctx, `SELECT `+cols+` FROM memberships WHERE id = $1 FOR UPDATE`, id))
		if db.NoRows(err) {
			return apperr.NotFound("La membresía")
		}
		if err != nil {
			return err
		}
		if in.Status != nil {
			if *in.Status != "active" && *in.Status != "cancelled" {
				return apperr.Fields(map[string]string{"status": "Estado inválido."})
			}
			if _, err := tx.Exec(ctx, `UPDATE memberships SET status = $2 WHERE id = $1`, id, *in.Status); err != nil {
				return err
			}
			m.Status = *in.Status
		}
		return s.applyOverrides(ctx, tx, m, in.CreditsLeft, in.EndsAt, loc)
	})
	return m, err
}

func (s *Service) applyOverrides(ctx context.Context, q db.Querier, m *Membership, credits *int, endsAt *string, loc *time.Location) error {
	if credits != nil {
		if m.CreditsTotal == nil {
			return apperr.Fields(map[string]string{"creditsLeft": "Este plan es ilimitado."})
		}
		if *credits < 0 {
			return apperr.Fields(map[string]string{"creditsLeft": "No puede ser negativo."})
		}
		total := max(*m.CreditsTotal, *credits)
		if _, err := q.Exec(ctx, `UPDATE memberships SET credits_left = $2, credits_total = $3 WHERE id = $1`, m.ID, *credits, total); err != nil {
			return err
		}
		m.CreditsLeft, m.CreditsTotal = credits, &total
	}
	if endsAt != nil {
		d, err := time.ParseInLocation(time.DateOnly, *endsAt, loc)
		if err != nil {
			return apperr.Fields(map[string]string{"endsAt": "Usa el formato AAAA-MM-DD."})
		}
		end := d.Add(24*time.Hour - time.Second)
		if _, err := q.Exec(ctx, `UPDATE memberships SET ends_at = $2 WHERE id = $1`, m.ID, end); err != nil {
			return err
		}
		m.EndsAt = end
	}
	return nil
}
