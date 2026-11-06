// Package dashboard aggregates business numbers for the admin home.
package dashboard

import (
	"context"
	"net/http"
	"time"

	"github.com/bloodandsand/backend/internal/platform/httpx"
	"github.com/bloodandsand/backend/internal/schedule"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Service struct {
	pool *pgxpool.Pool
	loc  *time.Location
}

func NewService(pool *pgxpool.Pool, loc *time.Location) *Service { return &Service{pool: pool, loc: loc} }

type DayRevenue struct {
	Date   string `json:"date"`
	Amount int64  `json:"amount"`
	Orders int    `json:"orders"`
}

type Summary struct {
	RevenueToday     int64        `json:"revenueToday"`
	RevenueMonth     int64        `json:"revenueMonth"`
	RevenuePrevMonth int64        `json:"revenuePrevMonth"`
	BookingsToday    int          `json:"bookingsToday"`
	Occupancy7d      float64      `json:"occupancy7d"` // 0..1 over the next 7 days
	ActiveMembers    int          `json:"activeMembers"`
	NewStudents30d   int          `json:"newStudents30d"`
	PendingOrders    int          `json:"pendingOrders"`
	Revenue30d       []DayRevenue `json:"revenue30d"`
	ByDiscipline     []struct {
		Name     string `json:"name"`
		Bookings int    `json:"bookings"`
	} `json:"byDiscipline"`
}

func (s *Service) Summary(ctx context.Context) (Summary, error) {
	now := time.Now().In(s.loc)
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, s.loc)
	month := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, s.loc)
	prev := month.AddDate(0, -1, 0)

	var out Summary
	err := s.pool.QueryRow(ctx, `SELECT
		coalesce(sum(amount) FILTER (WHERE paid_at >= $1), 0),
		coalesce(sum(amount) FILTER (WHERE paid_at >= $2), 0),
		coalesce(sum(amount) FILTER (WHERE paid_at >= $3 AND paid_at < $2), 0)
		FROM orders WHERE status = 'paid'`, today, month, prev).Scan(&out.RevenueToday, &out.RevenueMonth, &out.RevenuePrevMonth)
	if err != nil {
		return out, err
	}
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM bookings b JOIN class_sessions s ON s.id = b.session_id
		WHERE s.starts_at >= $1 AND s.starts_at < $2 AND b.status IN ('confirmed', 'attended')`, today, today.AddDate(0, 0, 1)).Scan(&out.BookingsToday); err != nil {
		return out, err
	}
	var taken, capacity int
	if err := s.pool.QueryRow(ctx, `SELECT coalesce(sum((SELECT count(*) FROM bookings b WHERE b.session_id = s.id AND `+schedule.ActiveBookingSQL+`)), 0),
		coalesce(sum(s.capacity), 0) FROM class_sessions s WHERE s.status = 'scheduled' AND s.starts_at >= now() AND s.starts_at < now() + interval '7 days'`).Scan(&taken, &capacity); err != nil {
		return out, err
	}
	if capacity > 0 {
		out.Occupancy7d = float64(taken) / float64(capacity)
	}
	if err := s.pool.QueryRow(ctx, `SELECT
		(SELECT count(DISTINCT user_id) FROM memberships WHERE status = 'active' AND ends_at > now()),
		(SELECT count(*) FROM users WHERE role = 'student' AND created_at > now() - interval '30 days'),
		(SELECT count(*) FROM orders WHERE status = 'pending')`).Scan(&out.ActiveMembers, &out.NewStudents30d, &out.PendingOrders); err != nil {
		return out, err
	}

	rows, err := s.pool.Query(ctx, `SELECT to_char(d, 'YYYY-MM-DD'), coalesce(sum(o.amount), 0), count(o.id)
		FROM generate_series(($1::timestamptz AT TIME ZONE $2)::date - 29, ($1::timestamptz AT TIME ZONE $2)::date, interval '1 day') d
		LEFT JOIN orders o ON o.status = 'paid' AND (o.paid_at AT TIME ZONE $2)::date = d::date
		GROUP BY d ORDER BY d`, now, s.loc.String())
	if err != nil {
		return out, err
	}
	defer rows.Close()
	out.Revenue30d = []DayRevenue{}
	for rows.Next() {
		var d DayRevenue
		if err := rows.Scan(&d.Date, &d.Amount, &d.Orders); err != nil {
			return out, err
		}
		out.Revenue30d = append(out.Revenue30d, d)
	}
	rows.Close()

	rows, err = s.pool.Query(ctx, `SELECT d.name, count(b.id) FROM disciplines d
		LEFT JOIN class_sessions s ON s.discipline_id = d.id AND s.starts_at > now() - interval '30 days' AND s.starts_at <= now() + interval '14 days'
		LEFT JOIN bookings b ON b.session_id = s.id AND b.status IN ('confirmed', 'attended')
		GROUP BY d.id, d.name, d.sort ORDER BY d.sort`)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var x struct {
			Name     string `json:"name"`
			Bookings int    `json:"bookings"`
		}
		if err := rows.Scan(&x.Name, &x.Bookings); err != nil {
			return out, err
		}
		out.ByDiscipline = append(out.ByDiscipline, x)
	}
	return out, rows.Err()
}

func (s *Service) Routes(rt httpx.Router) {
	rt.Admin("GET /api/admin/dashboard", func(w http.ResponseWriter, r *http.Request) error {
		sum, err := s.Summary(r.Context())
		if err != nil {
			return err
		}
		return httpx.OK(w, sum)
	})
}
