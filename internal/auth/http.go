package auth

import (
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/bloodandsand/backend/internal/platform/actor"
	"github.com/bloodandsand/backend/internal/platform/apperr"
	"github.com/bloodandsand/backend/internal/platform/httpx"
	"github.com/bloodandsand/backend/internal/user"
)

const (
	accessCookie  = "bs_access"
	refreshCookie = "bs_refresh"
	refreshPath   = "/api/auth"
)

type Handler struct {
	svc        *Service
	users      *user.Service
	log        *slog.Logger
	secure     bool
	domain     string
	accessTTL  time.Duration
	refreshTTL time.Duration
	limiter    *httpx.RateLimiter
}

func NewHandler(svc *Service, users *user.Service, log *slog.Logger, secure bool, domain string) *Handler {
	return &Handler{
		svc: svc, users: users, log: log, secure: secure, domain: domain,
		accessTTL: svc.accessTTL, refreshTTL: svc.refreshTTL,
		limiter: httpx.NewRateLimiter(10, 5), // 10 attempts per minute per IP, bursts of 5
	}
}

func (h *Handler) Routes(mux *http.ServeMux) {
	hd := func(f httpx.HandlerFunc) http.HandlerFunc { return httpx.Handle(h.log, f) }
	mux.HandleFunc("POST /api/auth/login", h.limiter.Wrap(h.log, hd(h.login)))
	mux.HandleFunc("POST /api/auth/register", h.limiter.Wrap(h.log, hd(h.register)))
	mux.HandleFunc("POST /api/auth/refresh", hd(h.refresh))
	mux.HandleFunc("POST /api/auth/logout", hd(h.logout))
	mux.Handle("GET /api/auth/me", h.Require()(hd(h.me)))
	mux.Handle("PATCH /api/auth/me", h.Require()(hd(h.updateMe)))
	mux.Handle("POST /api/auth/password", h.Require()(h.limiter.Wrap(h.log, hd(h.changePassword))))
}

func (h *Handler) setCookies(w http.ResponseWriter, s *Session) {
	http.SetCookie(w, &http.Cookie{Name: accessCookie, Value: s.AccessToken, Path: "/", Domain: h.domain,
		MaxAge: int(h.accessTTL.Seconds()), HttpOnly: true, Secure: h.secure, SameSite: http.SameSiteLaxMode})
	http.SetCookie(w, &http.Cookie{Name: refreshCookie, Value: s.RefreshToken, Path: refreshPath, Domain: h.domain,
		MaxAge: int(h.refreshTTL.Seconds()), HttpOnly: true, Secure: h.secure, SameSite: http.SameSiteLaxMode})
}

func (h *Handler) clearCookies(w http.ResponseWriter) {
	for name, path := range map[string]string{accessCookie: "/", refreshCookie: refreshPath} {
		http.SetCookie(w, &http.Cookie{Name: name, Path: path, Domain: h.domain, MaxAge: -1, HttpOnly: true, Secure: h.secure, SameSite: http.SameSiteLaxMode})
	}
}

type credentials struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

func (h *Handler) login(w http.ResponseWriter, r *http.Request) error {
	var in credentials
	if err := httpx.Decode(w, r, &in); err != nil {
		return err
	}
	s, err := h.svc.Login(r.Context(), in.Email, in.Password, r.UserAgent())
	if err != nil {
		return err
	}
	h.setCookies(w, s)
	return httpx.OK(w, s.User)
}

func (h *Handler) register(w http.ResponseWriter, r *http.Request) error {
	var in RegisterInput
	if err := httpx.Decode(w, r, &in); err != nil {
		return err
	}
	s, err := h.svc.Register(r.Context(), in, r.UserAgent())
	if err != nil {
		return err
	}
	h.setCookies(w, s)
	return httpx.Created(w, s.User)
}

func (h *Handler) refresh(w http.ResponseWriter, r *http.Request) error {
	c, _ := r.Cookie(refreshCookie)
	token := ""
	if c != nil {
		token = c.Value
	}
	s, err := h.svc.Refresh(r.Context(), token, r.UserAgent())
	if err != nil {
		h.clearCookies(w)
		return err
	}
	h.setCookies(w, s)
	return httpx.OK(w, s.User)
}

func (h *Handler) logout(w http.ResponseWriter, r *http.Request) error {
	if c, _ := r.Cookie(refreshCookie); c != nil {
		h.svc.Logout(r.Context(), c.Value)
	}
	h.clearCookies(w)
	return httpx.NoContent(w)
}

func (h *Handler) me(w http.ResponseWriter, r *http.Request) error {
	u, err := h.users.ByID(r.Context(), actor.From(r.Context()).ID)
	if err != nil {
		return err
	}
	return httpx.OK(w, u)
}

func (h *Handler) updateMe(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		FullName string `json:"fullName"`
		Phone    string `json:"phone"`
	}
	if err := httpx.Decode(w, r, &in); err != nil {
		return err
	}
	u, err := h.users.UpdateProfile(r.Context(), actor.From(r.Context()).ID, in.FullName, in.Phone)
	if err != nil {
		return err
	}
	return httpx.OK(w, u)
}

func (h *Handler) changePassword(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		CurrentPassword string `json:"currentPassword"`
		NewPassword     string `json:"newPassword"`
	}
	if err := httpx.Decode(w, r, &in); err != nil {
		return err
	}
	a := actor.From(r.Context())
	if err := h.svc.ChangePassword(r.Context(), a.ID, in.CurrentPassword, in.NewPassword); err != nil {
		return err
	}
	// Other sessions were closed; issue a fresh one for this device.
	u, err := h.users.ByID(r.Context(), a.ID)
	if err != nil {
		return err
	}
	s, err := h.svc.newSession(r.Context(), h.svc.pool, u, "", r.UserAgent())
	if err != nil {
		return err
	}
	h.setCookies(w, s)
	return httpx.NoContent(w)
}

// Authenticate reads the access cookie (or a Bearer token, handy for scripts) and attaches the caller.
// Requests without a valid token continue anonymously.
func (h *Handler) Authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := ""
		if c, err := r.Cookie(accessCookie); err == nil {
			token = c.Value
		} else if v := r.Header.Get("Authorization"); strings.HasPrefix(v, "Bearer ") {
			token = strings.TrimPrefix(v, "Bearer ")
		}
		if token != "" {
			if a, err := h.svc.ParseAccess(token); err == nil {
				r = r.WithContext(actor.With(r.Context(), a))
			}
		}
		next.ServeHTTP(w, r)
	})
}

// Require demands a logged-in caller with one of the given roles (any role if none given).
func (h *Handler) Require(roles ...actor.Role) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			a := actor.From(r.Context())
			if a == nil {
				httpx.WriteError(w, r, h.log, apperr.Unauthorized("Ingresa a tu cuenta para continuar."))
				return
			}
			if len(roles) > 0 && !slices.Contains(roles, a.Role) {
				httpx.WriteError(w, r, h.log, apperr.Forbidden())
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
