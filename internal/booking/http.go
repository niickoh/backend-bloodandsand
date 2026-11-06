package booking

import (
	"net/http"
	"time"

	"github.com/bloodandsand/backend/internal/platform/actor"
	"github.com/bloodandsand/backend/internal/platform/apperr"
	"github.com/bloodandsand/backend/internal/platform/httpx"
	"github.com/bloodandsand/backend/internal/schedule"
)

func (s *Service) Routes(rt httpx.Router) {
	// Public: anyone can price and book a free or plan-covered class (members need to be logged in for the plan).
	rt.Public("POST /api/bookings/quote", func(w http.ResponseWriter, r *http.Request) error {
		var in struct {
			SessionID int64  `json:"sessionId"`
			Email     string `json:"email"`
		}
		if err := httpx.Decode(w, r, &in); err != nil {
			return err
		}
		q, err := s.Quote(r.Context(), actor.From(r.Context()), in.SessionID, in.Email)
		if err != nil {
			return err
		}
		return httpx.OK(w, q)
	})
	rt.Public("POST /api/bookings", func(w http.ResponseWriter, r *http.Request) error {
		var in BookInput
		if err := httpx.Decode(w, r, &in); err != nil {
			return err
		}
		b, err := s.Book(r.Context(), actor.From(r.Context()), in)
		if err != nil {
			return err
		}
		return httpx.Created(w, b)
	})

	// Member
	rt.Auth("GET /api/me/bookings", func(w http.ResponseWriter, r *http.Request) error {
		list, err := s.ForUser(r.Context(), actor.From(r.Context()).ID)
		if err != nil {
			return err
		}
		return httpx.OK(w, list)
	})
	rt.Auth("POST /api/me/bookings/{id}/cancel", func(w http.ResponseWriter, r *http.Request) error {
		id, err := httpx.PathID(r, "id")
		if err != nil {
			return err
		}
		b, err := s.CancelOwn(r.Context(), actor.From(r.Context()), id)
		if err != nil {
			return err
		}
		return httpx.OK(w, b)
	})

	// Staff: coaches on their own classes, admins on all
	rt.Staff("GET /api/staff/sessions/{id}/roster", func(w http.ResponseWriter, r *http.Request) error {
		id, err := httpx.PathID(r, "id")
		if err != nil {
			return err
		}
		ro, err := s.Roster(r.Context(), actor.From(r.Context()), id)
		if err != nil {
			return err
		}
		return httpx.OK(w, ro)
	})
	rt.Staff("POST /api/staff/sessions/{id}/bookings", func(w http.ResponseWriter, r *http.Request) error {
		id, err := httpx.PathID(r, "id")
		if err != nil {
			return err
		}
		var in StaffCreateInput
		if err := httpx.Decode(w, r, &in); err != nil {
			return err
		}
		b, err := s.StaffCreate(r.Context(), actor.From(r.Context()), id, in)
		if err != nil {
			return err
		}
		return httpx.Created(w, b)
	})
	rt.Staff("PATCH /api/staff/bookings/{id}", func(w http.ResponseWriter, r *http.Request) error {
		id, err := httpx.PathID(r, "id")
		if err != nil {
			return err
		}
		var in StaffPatch
		if err := httpx.Decode(w, r, &in); err != nil {
			return err
		}
		b, err := s.StaffUpdate(r.Context(), actor.From(r.Context()), id, in)
		if err != nil {
			return err
		}
		return httpx.OK(w, b)
	})
	rt.Staff("DELETE /api/staff/bookings/{id}", func(w http.ResponseWriter, r *http.Request) error {
		id, err := httpx.PathID(r, "id")
		if err != nil {
			return err
		}
		if err := s.StaffDelete(r.Context(), actor.From(r.Context()), id); err != nil {
			return err
		}
		return httpx.NoContent(w)
	})
	rt.Staff("GET /api/staff/students", func(w http.ResponseWriter, r *http.Request) error {
		q := r.URL.Query().Get("q")
		if len([]rune(q)) < 2 {
			return httpx.OK(w, []any{})
		}
		rows, err := s.pool.Query(r.Context(), `SELECT u.id, u.full_name, u.email, u.phone,
			(SELECT m.plan_name FROM memberships m WHERE m.user_id = u.id AND m.status = 'active' AND m.ends_at > now() ORDER BY m.ends_at DESC LIMIT 1)
			FROM users u WHERE u.role = 'student' AND u.active AND (u.full_name ILIKE '%' || $1 || '%' OR u.email ILIKE '%' || $1 || '%')
			ORDER BY u.full_name LIMIT 10`, q)
		if err != nil {
			return err
		}
		defer rows.Close()
		type student struct {
			ID       int64   `json:"id"`
			FullName string  `json:"fullName"`
			Email    string  `json:"email"`
			Phone    string  `json:"phone"`
			Plan     *string `json:"plan"`
		}
		out := []student{}
		for rows.Next() {
			var st student
			if err := rows.Scan(&st.ID, &st.FullName, &st.Email, &st.Phone, &st.Plan); err != nil {
				return err
			}
			out = append(out, st)
		}
		return httpx.OK(w, out)
	})

	// Admin
	rt.Admin("GET /api/admin/bookings", func(w http.ResponseWriter, r *http.Request) error {
		now := time.Now().In(s.loc)
		today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, s.loc)
		from, err := httpx.QueryDate(r, "from", s.loc, today.AddDate(0, 0, -7))
		if err != nil {
			return err
		}
		to, err := httpx.QueryDate(r, "to", s.loc, today.AddDate(0, 0, 14))
		if err != nil {
			return err
		}
		p := httpx.PageFrom(r)
		list, total, err := s.AdminList(r.Context(), AdminFilter{
			From: from, To: to.AddDate(0, 0, 1), Status: r.URL.Query().Get("status"), Q: r.URL.Query().Get("q"),
			SessionID: httpx.QueryInt64(r, "session"), UserID: httpx.QueryInt64(r, "user"), Limit: p.Limit, Offset: p.Offset,
		})
		if err != nil {
			return err
		}
		return httpx.OK(w, httpx.List[Booking]{Items: list, Total: total})
	})
	rt.Admin("PATCH /api/admin/sessions/{id}", func(w http.ResponseWriter, r *http.Request) error {
		id, err := httpx.PathID(r, "id")
		if err != nil {
			return err
		}
		var in schedule.SessionPatch
		if err := httpx.Decode(w, r, &in); err != nil {
			return err
		}
		se, err := s.UpdateSession(r.Context(), id, in)
		if err != nil {
			return err
		}
		return httpx.OK(w, se)
	})
	rt.Admin("POST /api/admin/sessions/{id}/cancel", func(w http.ResponseWriter, r *http.Request) error {
		id, err := httpx.PathID(r, "id")
		if err != nil {
			return err
		}
		var in struct {
			Reason string `json:"reason"`
		}
		if err := httpx.Decode(w, r, &in); err != nil {
			return err
		}
		if len(in.Reason) > 200 {
			return apperr.Fields(map[string]string{"reason": "Usa como máximo 200 caracteres."})
		}
		n, err := s.CancelSession(r.Context(), id, in.Reason)
		if err != nil {
			return err
		}
		return httpx.OK(w, map[string]int{"cancelledBookings": n})
	})
}
