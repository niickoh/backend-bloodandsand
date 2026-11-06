package schedule

import (
	"net/http"
	"time"

	"github.com/bloodandsand/backend/internal/platform/actor"
	"github.com/bloodandsand/backend/internal/platform/apperr"
	"github.com/bloodandsand/backend/internal/platform/httpx"
)

// Routes registers schedule endpoints. Session edits/cancellations live in the booking module
// because they affect bookings and send notifications.
func (s *Service) Routes(rt httpx.Router, daysAhead int) {
	rt.Public("GET /api/timetable", func(w http.ResponseWriter, r *http.Request) error {
		list, err := s.Templates(r.Context(), true)
		if err != nil {
			return err
		}
		return httpx.OK(w, list)
	})

	rt.Public("GET /api/sessions", func(w http.ResponseWriter, r *http.Request) error {
		f, err := s.rangeFilter(r, 31)
		if err != nil {
			return err
		}
		f.DisciplineSlug = r.URL.Query().Get("discipline")
		list, err := s.Sessions(r.Context(), f)
		if err != nil {
			return err
		}
		return httpx.OK(w, list)
	})

	rt.Auth("GET /api/coach/sessions", func(w http.ResponseWriter, r *http.Request) error {
		f, err := s.rangeFilter(r, 62)
		if err != nil {
			return err
		}
		f.CoachID = actor.From(r.Context()).ID
		f.IncludeAll = true
		list, err := s.Sessions(r.Context(), f)
		if err != nil {
			return err
		}
		return httpx.OK(w, list)
	}, actor.RoleCoach, actor.RoleAdmin)

	rt.Admin("GET /api/admin/templates", func(w http.ResponseWriter, r *http.Request) error {
		list, err := s.Templates(r.Context(), false)
		if err != nil {
			return err
		}
		return httpx.OK(w, list)
	})
	rt.Admin("POST /api/admin/templates", func(w http.ResponseWriter, r *http.Request) error {
		var in TemplateInput
		if err := httpx.Decode(w, r, &in); err != nil {
			return err
		}
		t, err := s.CreateTemplate(r.Context(), in)
		if err != nil {
			return err
		}
		if _, err := s.Generate(r.Context(), daysAhead); err != nil {
			return err
		}
		return httpx.Created(w, t)
	})
	rt.Admin("PUT /api/admin/templates/{id}", func(w http.ResponseWriter, r *http.Request) error {
		id, err := httpx.PathID(r, "id")
		if err != nil {
			return err
		}
		var in TemplateInput
		if err := httpx.Decode(w, r, &in); err != nil {
			return err
		}
		t, kept, err := s.UpdateTemplate(r.Context(), id, in, daysAhead)
		if err != nil {
			return err
		}
		return httpx.OK(w, map[string]any{"template": t, "keptWithBookings": kept})
	})
	rt.Admin("DELETE /api/admin/templates/{id}", func(w http.ResponseWriter, r *http.Request) error {
		id, err := httpx.PathID(r, "id")
		if err != nil {
			return err
		}
		kept, err := s.DeleteTemplate(r.Context(), id)
		if err != nil {
			return err
		}
		return httpx.OK(w, map[string]any{"keptWithBookings": kept})
	})
	rt.Admin("POST /api/admin/sessions/generate", func(w http.ResponseWriter, r *http.Request) error {
		n, err := s.Generate(r.Context(), daysAhead)
		if err != nil {
			return err
		}
		return httpx.OK(w, map[string]int{"created": n})
	})
	rt.Admin("GET /api/admin/sessions", func(w http.ResponseWriter, r *http.Request) error {
		f, err := s.rangeFilter(r, 62)
		if err != nil {
			return err
		}
		f.CoachID = httpx.QueryInt64(r, "coach")
		f.DisciplineSlug = r.URL.Query().Get("discipline")
		f.IncludeAll = true
		list, err := s.Sessions(r.Context(), f)
		if err != nil {
			return err
		}
		return httpx.OK(w, list)
	})
	rt.Admin("POST /api/admin/sessions", func(w http.ResponseWriter, r *http.Request) error {
		var in SessionInput
		if err := httpx.Decode(w, r, &in); err != nil {
			return err
		}
		se, err := s.CreateSession(r.Context(), in)
		if err != nil {
			return err
		}
		return httpx.Created(w, se)
	})
}

// rangeFilter reads ?from=&to= (local dates, to inclusive). Defaults to today + 14 days.
func (s *Service) rangeFilter(r *http.Request, maxDays int) (SessionFilter, error) {
	now := time.Now().In(s.loc)
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, s.loc)
	from, err := httpx.QueryDate(r, "from", s.loc, today)
	if err != nil {
		return SessionFilter{}, err
	}
	to, err := httpx.QueryDate(r, "to", s.loc, from.AddDate(0, 0, 13))
	if err != nil {
		return SessionFilter{}, err
	}
	to = to.AddDate(0, 0, 1)
	if !to.After(from) || to.Sub(from) > time.Duration(maxDays)*24*time.Hour {
		return SessionFilter{}, apperr.Invalid("bad_range", "El rango de fechas no es válido.")
	}
	f := SessionFilter{From: from, To: to}
	if a := actor.From(r.Context()); a != nil {
		f.ViewerID = a.ID
	}
	return f, nil
}
