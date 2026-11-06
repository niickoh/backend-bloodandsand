package httpx

import (
	"log/slog"
	"net/http"

	"github.com/bloodandsand/backend/internal/platform/actor"
)

// Guard builds role middlewares; implemented by the auth module.
type Guard func(roles ...actor.Role) func(http.Handler) http.Handler

// Router registers error-returning handlers, optionally behind a role guard.
type Router struct {
	Mux   *http.ServeMux
	Log   *slog.Logger
	Guard Guard
}

// Public registers a handler open to anyone (the caller may still be logged in).
func (rt Router) Public(pattern string, h HandlerFunc) {
	rt.Mux.Handle(pattern, Handle(rt.Log, h))
}

// Auth registers a handler for logged-in users with one of the roles (any role if none).
func (rt Router) Auth(pattern string, h HandlerFunc, roles ...actor.Role) {
	rt.Mux.Handle(pattern, rt.Guard(roles...)(Handle(rt.Log, h)))
}

func (rt Router) Admin(pattern string, h HandlerFunc) { rt.Auth(pattern, h, actor.RoleAdmin) }

func (rt Router) Staff(pattern string, h HandlerFunc) {
	rt.Auth(pattern, h, actor.RoleAdmin, actor.RoleCoach)
}
