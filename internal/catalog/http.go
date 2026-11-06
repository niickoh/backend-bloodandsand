package catalog

import (
	"net/http"

	"github.com/bloodandsand/backend/internal/platform/httpx"
)

func (s *Service) Routes(rt httpx.Router) {
	rt.Public("GET /api/catalog", s.handlePublic)

	rt.Admin("GET /api/admin/disciplines", func(w http.ResponseWriter, r *http.Request) error {
		list, err := s.Disciplines(r.Context(), false)
		if err != nil {
			return err
		}
		return httpx.OK(w, list)
	})
	rt.Admin("POST /api/admin/disciplines", func(w http.ResponseWriter, r *http.Request) error {
		var d Discipline
		if err := httpx.Decode(w, r, &d); err != nil {
			return err
		}
		d.ID = 0
		out, err := s.SaveDiscipline(r.Context(), d)
		if err != nil {
			return err
		}
		return httpx.Created(w, out)
	})
	rt.Admin("PUT /api/admin/disciplines/{id}", func(w http.ResponseWriter, r *http.Request) error {
		id, err := httpx.PathID(r, "id")
		if err != nil {
			return err
		}
		var d Discipline
		if err := httpx.Decode(w, r, &d); err != nil {
			return err
		}
		d.ID = id
		out, err := s.SaveDiscipline(r.Context(), d)
		if err != nil {
			return err
		}
		return httpx.OK(w, out)
	})
	rt.Admin("DELETE /api/admin/disciplines/{id}", func(w http.ResponseWriter, r *http.Request) error {
		id, err := httpx.PathID(r, "id")
		if err != nil {
			return err
		}
		if err := s.DeleteDiscipline(r.Context(), id); err != nil {
			return err
		}
		return httpx.NoContent(w)
	})

	rt.Admin("GET /api/admin/plans", func(w http.ResponseWriter, r *http.Request) error {
		list, err := s.Plans(r.Context(), false)
		if err != nil {
			return err
		}
		return httpx.OK(w, list)
	})
	rt.Admin("POST /api/admin/plans", func(w http.ResponseWriter, r *http.Request) error {
		var p Plan
		if err := httpx.Decode(w, r, &p); err != nil {
			return err
		}
		p.ID = 0
		out, err := s.SavePlan(r.Context(), p)
		if err != nil {
			return err
		}
		return httpx.Created(w, out)
	})
	rt.Admin("PUT /api/admin/plans/{id}", func(w http.ResponseWriter, r *http.Request) error {
		id, err := httpx.PathID(r, "id")
		if err != nil {
			return err
		}
		var p Plan
		if err := httpx.Decode(w, r, &p); err != nil {
			return err
		}
		p.ID = id
		out, err := s.SavePlan(r.Context(), p)
		if err != nil {
			return err
		}
		return httpx.OK(w, out)
	})
	rt.Admin("DELETE /api/admin/plans/{id}", func(w http.ResponseWriter, r *http.Request) error {
		id, err := httpx.PathID(r, "id")
		if err != nil {
			return err
		}
		if err := s.DeletePlan(r.Context(), id); err != nil {
			return err
		}
		return httpx.NoContent(w)
	})
}

func (s *Service) handlePublic(w http.ResponseWriter, r *http.Request) error {
	disc, err := s.Disciplines(r.Context(), true)
	if err != nil {
		return err
	}
	plans, err := s.Plans(r.Context(), true)
	if err != nil {
		return err
	}
	w.Header().Set("Cache-Control", "public, max-age=60")
	return httpx.OK(w, map[string]any{"disciplines": disc, "plans": plans})
}
