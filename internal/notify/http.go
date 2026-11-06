package notify

import (
	"net/http"
	"strings"

	"github.com/bloodandsand/backend/internal/platform/actor"
	"github.com/bloodandsand/backend/internal/platform/apperr"
	"github.com/bloodandsand/backend/internal/platform/httpx"
)

func (s *Service) Routes(rt httpx.Router) {
	rt.Public("GET /api/push/public-key", func(w http.ResponseWriter, r *http.Request) error {
		return httpx.OK(w, map[string]any{"enabled": s.Enabled(), "publicKey": s.publicKey})
	})
	rt.Public("POST /api/push/subscriptions", func(w http.ResponseWriter, r *http.Request) error {
		var in SubscriptionInput
		if err := httpx.Decode(w, r, &in); err != nil {
			return err
		}
		var uid *int64
		if a := actor.From(r.Context()); a != nil {
			uid = &a.ID
		}
		if err := s.Subscribe(r.Context(), uid, in, r.UserAgent()); err != nil {
			return err
		}
		return httpx.NoContent(w)
	})
	rt.Public("DELETE /api/push/subscriptions", func(w http.ResponseWriter, r *http.Request) error {
		var in struct {
			Endpoint string `json:"endpoint"`
		}
		if err := httpx.Decode(w, r, &in); err != nil {
			return err
		}
		if err := s.Unsubscribe(r.Context(), in.Endpoint); err != nil {
			return err
		}
		return httpx.NoContent(w)
	})

	rt.Admin("GET /api/admin/announcements", func(w http.ResponseWriter, r *http.Request) error {
		rows, err := s.pool.Query(r.Context(), `SELECT a.id, a.title, a.body, a.url, a.audience, a.sent_count, a.created_at, coalesce(u.full_name, '')
			FROM announcements a LEFT JOIN users u ON u.id = a.created_by ORDER BY a.created_at DESC LIMIT 50`)
		if err != nil {
			return err
		}
		defer rows.Close()
		type row struct {
			ID        int64  `json:"id"`
			Title     string `json:"title"`
			Body      string `json:"body"`
			URL       string `json:"url"`
			Audience  string `json:"audience"`
			SentCount int    `json:"sentCount"`
			CreatedAt any    `json:"createdAt"`
			CreatedBy string `json:"createdBy"`
		}
		out := []row{}
		for rows.Next() {
			var x row
			if err := rows.Scan(&x.ID, &x.Title, &x.Body, &x.URL, &x.Audience, &x.SentCount, &x.CreatedAt, &x.CreatedBy); err != nil {
				return err
			}
			out = append(out, x)
		}
		return httpx.OK(w, out)
	})
	rt.Admin("POST /api/admin/announcements", func(w http.ResponseWriter, r *http.Request) error {
		var in struct {
			Title    string `json:"title"`
			Body     string `json:"body"`
			URL      string `json:"url"`
			Audience string `json:"audience"`
		}
		if err := httpx.Decode(w, r, &in); err != nil {
			return err
		}
		in.Title, in.Body = strings.TrimSpace(in.Title), strings.TrimSpace(in.Body)
		fe := apperr.FieldErrors{}
		fe.Check(in.Title != "" && len(in.Title) <= 60, "title", "Escribe un título de hasta 60 caracteres.")
		fe.Check(in.Body != "" && len(in.Body) <= 200, "body", "Escribe un mensaje de hasta 200 caracteres.")
		fe.Check(in.Audience == "all" || in.Audience == "students" || in.Audience == "coaches", "audience", "Elige a quién enviarlo.")
		fe.Check(in.URL == "" || strings.HasPrefix(in.URL, "/"), "url", "Usa una ruta de la app, por ejemplo /horario.")
		if err := fe.Err(); err != nil {
			return err
		}
		n, err := s.Broadcast(r.Context(), in.Audience, Message{Title: in.Title, Body: in.Body, URL: in.URL, Tag: "anuncio"})
		if err != nil {
			return err
		}
		_, err = s.pool.Exec(r.Context(), `INSERT INTO announcements (title, body, url, audience, sent_count, created_by) VALUES ($1,$2,$3,$4,$5,$6)`,
			in.Title, in.Body, in.URL, in.Audience, n, actor.From(r.Context()).ID)
		if err != nil {
			return err
		}
		return httpx.Created(w, map[string]int{"sentCount": n})
	})
}
