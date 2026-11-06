// Package auth handles login sessions: a short-lived JWT access cookie and a rotating refresh cookie.
package auth

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/bloodandsand/backend/internal/platform/actor"
	"github.com/bloodandsand/backend/internal/platform/apperr"
	"github.com/bloodandsand/backend/internal/platform/db"
	"github.com/bloodandsand/backend/internal/platform/security"
	"github.com/bloodandsand/backend/internal/user"
	"github.com/golang-jwt/jwt/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const issuer = "bloodandsand"

type Service struct {
	pool       *pgxpool.Pool
	users      *user.Service
	secret     []byte
	accessTTL  time.Duration
	refreshTTL time.Duration
}

func NewService(pool *pgxpool.Pool, users *user.Service, secret string, accessTTL, refreshTTL time.Duration) *Service {
	return &Service{pool: pool, users: users, secret: []byte(secret), accessTTL: accessTTL, refreshTTL: refreshTTL}
}

// Session is what a successful login or refresh hands to the HTTP layer.
type Session struct {
	User         *user.User
	AccessToken  string
	RefreshToken string
}

type claims struct {
	Role  actor.Role `json:"role"`
	Name  string     `json:"name"`
	Email string     `json:"email"`
	jwt.RegisteredClaims
}

func (s *Service) issueAccess(u *user.User) (string, error) {
	now := time.Now()
	c := claims{
		Role: u.Role, Name: u.FullName, Email: u.Email,
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    issuer,
			Subject:   strconv.FormatInt(u.ID, 10),
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(s.accessTTL)),
		},
	}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, c).SignedString(s.secret)
}

// ParseAccess validates an access token and returns the caller.
func (s *Service) ParseAccess(token string) (*actor.Actor, error) {
	var c claims
	_, err := jwt.ParseWithClaims(token, &c, func(t *jwt.Token) (any, error) { return s.secret, nil },
		jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}), jwt.WithIssuer(issuer), jwt.WithExpirationRequired())
	if err != nil {
		return nil, err
	}
	id, err := strconv.ParseInt(c.Subject, 10, 64)
	if err != nil || !c.Role.Valid() {
		return nil, errors.New("token sin sujeto válido")
	}
	return &actor.Actor{ID: id, Role: c.Role, Email: c.Email, Name: c.Name}, nil
}

func (s *Service) newSession(ctx context.Context, q db.Querier, u *user.User, family, userAgent string) (*Session, error) {
	access, err := s.issueAccess(u)
	if err != nil {
		return nil, err
	}
	refresh := security.RandomToken(32)
	if family == "" {
		family = security.RandomToken(12)
	}
	if len(userAgent) > 255 {
		userAgent = userAgent[:255]
	}
	_, err = q.Exec(ctx, `INSERT INTO refresh_tokens (user_id, token_hash, family, expires_at, user_agent) VALUES ($1, $2, $3, $4, $5)`,
		u.ID, security.HashToken(refresh), family, time.Now().Add(s.refreshTTL), userAgent)
	if err != nil {
		return nil, err
	}
	return &Session{User: u, AccessToken: access, RefreshToken: refresh}, nil
}

var errBadCredentials = apperr.Unauthorized("Correo o contraseña incorrectos.")

func (s *Service) Login(ctx context.Context, email, password, userAgent string) (*Session, error) {
	u, err := s.users.ByEmail(ctx, email)
	if err != nil {
		if _, ok := apperr.As(err); ok {
			security.BurnPasswordCheck(password)
			return nil, errBadCredentials
		}
		return nil, err
	}
	if !security.CheckPassword(u.PasswordHash, password) {
		return nil, errBadCredentials
	}
	if !u.Active {
		return nil, apperr.Unauthorized("Tu cuenta está desactivada. Escríbenos para reactivarla.")
	}
	sess, err := s.newSession(ctx, s.pool, u, "", userAgent)
	if err == nil {
		s.users.TouchLogin(ctx, u.ID)
	}
	return sess, err
}

type RegisterInput struct {
	Email    string `json:"email"`
	Password string `json:"password"`
	FullName string `json:"fullName"`
	Phone    string `json:"phone"`
}

// Register creates a student account. Coaches and admins are created by an admin.
func (s *Service) Register(ctx context.Context, in RegisterInput, userAgent string) (*Session, error) {
	u, err := s.users.Create(ctx, user.CreateInput{Email: in.Email, Password: in.Password, FullName: in.FullName, Phone: in.Phone, Role: actor.RoleStudent})
	if err != nil {
		return nil, err
	}
	// Link the bookings this person made as a guest with the same email.
	_, _ = s.pool.Exec(ctx, `UPDATE bookings SET user_id = $1 WHERE user_id IS NULL AND lower(email) = lower($2)`, u.ID, u.Email)
	_, _ = s.pool.Exec(ctx, `UPDATE orders SET user_id = $1 WHERE user_id IS NULL AND lower(buyer_email) = lower($2)`, u.ID, u.Email)
	return s.newSession(ctx, s.pool, u, "", userAgent)
}

var errSessionExpired = apperr.Unauthorized("Tu sesión expiró. Vuelve a ingresar.")

// Refresh rotates the refresh token. Reusing an already-rotated token revokes the whole family,
// which logs out an attacker who stole an old cookie (and the victim, who just logs in again).
func (s *Service) Refresh(ctx context.Context, token, userAgent string) (*Session, error) {
	if token == "" {
		return nil, errSessionExpired
	}
	var sess *Session
	err := db.WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		var (
			id, userID int64
			family     string
			expires    time.Time
			revoked    *time.Time
		)
		err := tx.QueryRow(ctx, `SELECT id, user_id, family, expires_at, revoked_at FROM refresh_tokens WHERE token_hash = $1 FOR UPDATE`,
			security.HashToken(token)).Scan(&id, &userID, &family, &expires, &revoked)
		if db.NoRows(err) {
			return errSessionExpired
		}
		if err != nil {
			return err
		}
		if revoked != nil {
			return errReuse // handled after rollback, see below
		}
		if time.Now().After(expires) {
			return errSessionExpired
		}
		u, err := s.users.ByID(ctx, userID)
		if err != nil {
			return errSessionExpired
		}
		if !u.Active {
			return apperr.Unauthorized("Tu cuenta está desactivada.")
		}
		if _, err := tx.Exec(ctx, `UPDATE refresh_tokens SET revoked_at = now() WHERE id = $1`, id); err != nil {
			return err
		}
		sess, err = s.newSession(ctx, tx, u, family, userAgent)
		return err
	})
	if errors.Is(err, errReuse) {
		// Outside the rolled-back transaction so the revocation persists.
		_, _ = s.pool.Exec(ctx, `UPDATE refresh_tokens SET revoked_at = now()
			WHERE family = (SELECT family FROM refresh_tokens WHERE token_hash = $1) AND revoked_at IS NULL`, security.HashToken(token))
		return nil, errSessionExpired
	}
	return sess, err
}

var errReuse = fmt.Errorf("refresh token reutilizado")

func (s *Service) Logout(ctx context.Context, token string) {
	if token == "" {
		return
	}
	_, _ = s.pool.Exec(ctx, `UPDATE refresh_tokens SET revoked_at = now() WHERE token_hash = $1 AND revoked_at IS NULL`, security.HashToken(token))
}

// ChangePassword checks the current password, then replaces it (closing other sessions).
func (s *Service) ChangePassword(ctx context.Context, userID int64, current, next string) error {
	u, err := s.users.ByID(ctx, userID)
	if err != nil {
		return err
	}
	if !security.CheckPassword(u.PasswordHash, current) {
		return apperr.Fields(map[string]string{"currentPassword": "La contraseña actual no coincide."})
	}
	return s.users.SetPassword(ctx, userID, next)
}

// PurgeExpired deletes old refresh tokens; called by the maintenance worker.
func (s *Service) PurgeExpired(ctx context.Context) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM refresh_tokens WHERE expires_at < now() - interval '7 days'`)
	return err
}
