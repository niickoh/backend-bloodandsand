package user

import (
	"net/http"
	"strconv"

	"github.com/bloodandsand/backend/internal/platform/actor"
	"github.com/bloodandsand/backend/internal/platform/httpx"
)

func (s *Service) Routes(rt httpx.Router) {
	rt.Admin("GET /api/admin/users", func(w http.ResponseWriter, r *http.Request) error {
		p := httpx.PageFrom(r)
		f := Filter{Q: r.URL.Query().Get("q"), Role: actor.Role(r.URL.Query().Get("role")), Limit: p.Limit, Offset: p.Offset}
		if v, err := strconv.ParseBool(r.URL.Query().Get("active")); err == nil {
			f.Active = &v
		}
		list, total, err := s.List(r.Context(), f)
		if err != nil {
			return err
		}
		return httpx.OK(w, httpx.List[User]{Items: list, Total: total})
	})
	rt.Admin("POST /api/admin/users", func(w http.ResponseWriter, r *http.Request) error {
		var in CreateInput
		if err := httpx.Decode(w, r, &in); err != nil {
			return err
		}
		u, err := s.Create(r.Context(), in)
		if err != nil {
			return err
		}
		return httpx.Created(w, u)
	})
	rt.Admin("GET /api/admin/users/{id}", func(w http.ResponseWriter, r *http.Request) error {
		id, err := httpx.PathID(r, "id")
		if err != nil {
			return err
		}
		u, err := s.ByID(r.Context(), id)
		if err != nil {
			return err
		}
		return httpx.OK(w, u)
	})
	rt.Admin("PATCH /api/admin/users/{id}", func(w http.ResponseWriter, r *http.Request) error {
		id, err := httpx.PathID(r, "id")
		if err != nil {
			return err
		}
		var in UpdateInput
		if err := httpx.Decode(w, r, &in); err != nil {
			return err
		}
		u, err := s.Update(r.Context(), actor.From(r.Context()), id, in)
		if err != nil {
			return err
		}
		return httpx.OK(w, u)
	})
	rt.Admin("POST /api/admin/users/{id}/password", func(w http.ResponseWriter, r *http.Request) error {
		id, err := httpx.PathID(r, "id")
		if err != nil {
			return err
		}
		var in struct {
			Password string `json:"password"`
		}
		if err := httpx.Decode(w, r, &in); err != nil {
			return err
		}
		if err := s.SetPassword(r.Context(), id, in.Password); err != nil {
			return err
		}
		return httpx.NoContent(w)
	})
	rt.Admin("DELETE /api/admin/users/{id}", func(w http.ResponseWriter, r *http.Request) error {
		id, err := httpx.PathID(r, "id")
		if err != nil {
			return err
		}
		if err := s.Delete(r.Context(), actor.From(r.Context()), id); err != nil {
			return err
		}
		return httpx.NoContent(w)
	})
	// Coaches for pickers in the timetable editor
	rt.Admin("GET /api/admin/coaches", func(w http.ResponseWriter, r *http.Request) error {
		active := true
		list, _, err := s.List(r.Context(), Filter{Role: actor.RoleCoach, Active: &active, Limit: 200})
		if err != nil {
			return err
		}
		return httpx.OK(w, list)
	})
}
