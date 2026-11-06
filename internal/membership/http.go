package membership

import (
	"net/http"
	"time"

	"github.com/bloodandsand/backend/internal/platform/actor"
	"github.com/bloodandsand/backend/internal/platform/httpx"
)

func (s *Service) Routes(rt httpx.Router, loc *time.Location) {
	rt.Auth("GET /api/me/membership", func(w http.ResponseWriter, r *http.Request) error {
		uid := actor.From(r.Context()).ID
		cur, err := s.Current(r.Context(), s.pool, uid, false)
		if err != nil {
			return err
		}
		history, err := s.ForUser(r.Context(), uid)
		if err != nil {
			return err
		}
		return httpx.OK(w, map[string]any{"current": cur, "history": history})
	})

	rt.Admin("GET /api/admin/users/{id}/memberships", func(w http.ResponseWriter, r *http.Request) error {
		id, err := httpx.PathID(r, "id")
		if err != nil {
			return err
		}
		list, err := s.ForUser(r.Context(), id)
		if err != nil {
			return err
		}
		return httpx.OK(w, list)
	})
	rt.Admin("POST /api/admin/users/{id}/memberships", func(w http.ResponseWriter, r *http.Request) error {
		id, err := httpx.PathID(r, "id")
		if err != nil {
			return err
		}
		var in AssignInput
		if err := httpx.Decode(w, r, &in); err != nil {
			return err
		}
		m, err := s.Assign(r.Context(), id, in, loc)
		if err != nil {
			return err
		}
		return httpx.Created(w, m)
	})
	rt.Admin("PATCH /api/admin/memberships/{id}", func(w http.ResponseWriter, r *http.Request) error {
		id, err := httpx.PathID(r, "id")
		if err != nil {
			return err
		}
		var in AdjustInput
		if err := httpx.Decode(w, r, &in); err != nil {
			return err
		}
		m, err := s.Adjust(r.Context(), id, in, loc)
		if err != nil {
			return err
		}
		return httpx.OK(w, m)
	})
}
