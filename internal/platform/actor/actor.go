// Package actor holds the authenticated identity of a request. It has no dependencies,
// so every module can read the caller without importing the auth module.
package actor

import "context"

type Role string

const (
	RoleAdmin   Role = "admin"
	RoleCoach   Role = "coach"
	RoleStudent Role = "student"
)

func (r Role) Valid() bool { return r == RoleAdmin || r == RoleCoach || r == RoleStudent }

type Actor struct {
	ID    int64
	Role  Role
	Email string
	Name  string
}

func (a *Actor) IsAdmin() bool { return a != nil && a.Role == RoleAdmin }
func (a *Actor) IsCoach() bool { return a != nil && a.Role == RoleCoach }
func (a *Actor) IsStaff() bool { return a.IsAdmin() || a.IsCoach() }

type ctxKey struct{}

func With(ctx context.Context, a *Actor) context.Context { return context.WithValue(ctx, ctxKey{}, a) }

// From returns the caller or nil for anonymous requests.
func From(ctx context.Context) *Actor {
	a, _ := ctx.Value(ctxKey{}).(*Actor)
	return a
}
