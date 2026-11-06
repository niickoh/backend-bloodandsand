// Package user manages accounts: admins, coaches and students.
package user

import (
	"context"
	"net/mail"
	"strings"
	"time"

	"github.com/bloodandsand/backend/internal/platform/actor"
	"github.com/bloodandsand/backend/internal/platform/apperr"
	"github.com/bloodandsand/backend/internal/platform/db"
	"github.com/bloodandsand/backend/internal/platform/security"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type User struct {
	ID           int64      `json:"id"`
	Email        string     `json:"email"`
	FullName     string     `json:"fullName"`
	Phone        string     `json:"phone"`
	Role         actor.Role `json:"role"`
	Active       bool       `json:"active"`
	LastLoginAt  *time.Time `json:"lastLoginAt"`
	CreatedAt    time.Time  `json:"createdAt"`
	PasswordHash string     `json:"-"`
}

func (u *User) Actor() *actor.Actor {
	return &actor.Actor{ID: u.ID, Role: u.Role, Email: u.Email, Name: u.FullName}
}

const cols = `id, email, full_name, phone, role, active, last_login_at, created_at, password_hash`

func scan(row interface{ Scan(...any) error }) (*User, error) {
	var u User
	err := row.Scan(&u.ID, &u.Email, &u.FullName, &u.Phone, &u.Role, &u.Active, &u.LastLoginAt, &u.CreatedAt, &u.PasswordHash)
	return &u, err
}

// Service holds user persistence and rules.
type Service struct {
	pool *pgxpool.Pool
}

func NewService(pool *pgxpool.Pool) *Service { return &Service{pool: pool} }

func (s *Service) ByID(ctx context.Context, id int64) (*User, error) {
	u, err := scan(s.pool.QueryRow(ctx, `SELECT `+cols+` FROM users WHERE id = $1`, id))
	if db.NoRows(err) {
		return nil, apperr.NotFound("El usuario")
	}
	return u, err
}

func (s *Service) ByEmail(ctx context.Context, email string) (*User, error) {
	u, err := scan(s.pool.QueryRow(ctx, `SELECT `+cols+` FROM users WHERE lower(email) = lower($1)`, strings.TrimSpace(email)))
	if db.NoRows(err) {
		return nil, apperr.NotFound("El usuario")
	}
	return u, err
}

func (s *Service) TouchLogin(ctx context.Context, id int64) {
	_, _ = s.pool.Exec(ctx, `UPDATE users SET last_login_at = now() WHERE id = $1`, id)
}

type Filter struct {
	Q      string
	Role   actor.Role
	Active *bool
	Limit  int
	Offset int
}

func (s *Service) List(ctx context.Context, f Filter) ([]User, int, error) {
	where := `WHERE ($1 = '' OR full_name ILIKE '%' || $1 || '%' OR email ILIKE '%' || $1 || '%' OR phone ILIKE '%' || $1 || '%')
		AND ($2 = '' OR role = $2) AND ($3::boolean IS NULL OR active = $3)`
	args := []any{strings.TrimSpace(f.Q), string(f.Role), f.Active}

	var total int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM users `+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := s.pool.Query(ctx, `SELECT `+cols+` FROM users `+where+` ORDER BY created_at DESC LIMIT $4 OFFSET $5`,
		append(args, f.Limit, f.Offset)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []User{}
	for rows.Next() {
		u, err := scan(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, *u)
	}
	return out, total, rows.Err()
}

type CreateInput struct {
	Email    string     `json:"email"`
	Password string     `json:"password"`
	FullName string     `json:"fullName"`
	Phone    string     `json:"phone"`
	Role     actor.Role `json:"role"`
}

func (in *CreateInput) normalize() {
	in.Email = strings.ToLower(strings.TrimSpace(in.Email))
	in.FullName = strings.Join(strings.Fields(in.FullName), " ")
	in.Phone = strings.TrimSpace(in.Phone)
}

func validateCommon(fe apperr.FieldErrors, email, name, phone string) {
	if _, err := mail.ParseAddress(email); err != nil || !strings.Contains(email, ".") {
		fe.Add("email", "Escribe un correo válido, por ejemplo nombre@correo.cl.")
	}
	fe.Check(len(strings.Fields(name)) >= 2 && len(name) <= 120, "fullName", "Escribe nombre y apellido.")
	digits := 0
	for _, c := range phone {
		if c >= '0' && c <= '9' {
			digits++
		}
	}
	fe.Check(phone == "" || (digits >= 8 && digits <= 15), "phone", "El teléfono debe tener entre 8 y 15 dígitos.")
}

func (s *Service) Create(ctx context.Context, in CreateInput) (*User, error) {
	in.normalize()
	fe := apperr.FieldErrors{}
	validateCommon(fe, in.Email, in.FullName, in.Phone)
	if msg := security.ValidatePassword(in.Password); msg != "" {
		fe.Add("password", msg)
	}
	fe.Check(in.Role.Valid(), "role", "Elige un perfil: admin, coach o alumno.")
	if err := fe.Err(); err != nil {
		return nil, err
	}
	hash, err := security.HashPassword(in.Password)
	if err != nil {
		return nil, err
	}
	u, err := scan(s.pool.QueryRow(ctx, `INSERT INTO users (email, password_hash, full_name, phone, role)
		VALUES ($1, $2, $3, $4, $5) RETURNING `+cols, in.Email, hash, in.FullName, in.Phone, in.Role))
	if db.IsUniqueViolation(err, "users_email_key") {
		return nil, apperr.Fields(map[string]string{"email": "Ya existe una cuenta con este correo."})
	}
	return u, err
}

type UpdateInput struct {
	Email    *string     `json:"email"`
	FullName *string     `json:"fullName"`
	Phone    *string     `json:"phone"`
	Role     *actor.Role `json:"role"`
	Active   *bool       `json:"active"`
}

// Update applies a partial change. by is the caller; admins can't demote or deactivate themselves.
func (s *Service) Update(ctx context.Context, by *actor.Actor, id int64, in UpdateInput) (*User, error) {
	cur, err := s.ByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if in.Email != nil {
		cur.Email = strings.ToLower(strings.TrimSpace(*in.Email))
	}
	if in.FullName != nil {
		cur.FullName = strings.Join(strings.Fields(*in.FullName), " ")
	}
	if in.Phone != nil {
		cur.Phone = strings.TrimSpace(*in.Phone)
	}
	fe := apperr.FieldErrors{}
	validateCommon(fe, cur.Email, cur.FullName, cur.Phone)
	if in.Role != nil {
		fe.Check(in.Role.Valid(), "role", "Elige un perfil: admin, coach o alumno.")
		if by != nil && by.ID == id && *in.Role != actor.RoleAdmin {
			fe.Add("role", "No puedes quitarte el perfil de admin a ti mismo.")
		}
		cur.Role = *in.Role
	}
	if in.Active != nil {
		if by != nil && by.ID == id && !*in.Active {
			fe.Add("active", "No puedes desactivar tu propia cuenta.")
		}
		cur.Active = *in.Active
	}
	if err := fe.Err(); err != nil {
		return nil, err
	}

	err = db.WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		u, err := scan(tx.QueryRow(ctx, `UPDATE users SET email=$2, full_name=$3, phone=$4, role=$5, active=$6, updated_at=now()
			WHERE id=$1 RETURNING `+cols, id, cur.Email, cur.FullName, cur.Phone, cur.Role, cur.Active))
		if err != nil {
			return err
		}
		cur = u
		if !cur.Active || in.Role != nil {
			// Force a new login so the new role/state applies right away.
			_, err = tx.Exec(ctx, `UPDATE refresh_tokens SET revoked_at = now() WHERE user_id = $1 AND revoked_at IS NULL`, id)
		}
		return err
	})
	if db.IsUniqueViolation(err, "users_email_key") {
		return nil, apperr.Fields(map[string]string{"email": "Ya existe una cuenta con este correo."})
	}
	return cur, err
}

// UpdateProfile lets any user edit their own name and phone.
func (s *Service) UpdateProfile(ctx context.Context, id int64, fullName, phone string) (*User, error) {
	cur, err := s.ByID(ctx, id)
	if err != nil {
		return nil, err
	}
	name := strings.Join(strings.Fields(fullName), " ")
	phone = strings.TrimSpace(phone)
	fe := apperr.FieldErrors{}
	validateCommon(fe, cur.Email, name, phone)
	if err := fe.Err(); err != nil {
		return nil, err
	}
	return scan(s.pool.QueryRow(ctx, `UPDATE users SET full_name=$2, phone=$3, updated_at=now() WHERE id=$1 RETURNING `+cols, id, name, phone))
}

// SetPassword replaces the password and closes every other session.
func (s *Service) SetPassword(ctx context.Context, id int64, password string) error {
	if msg := security.ValidatePassword(password); msg != "" {
		return apperr.Fields(map[string]string{"password": msg})
	}
	hash, err := security.HashPassword(password)
	if err != nil {
		return err
	}
	return db.WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE users SET password_hash=$2, updated_at=now() WHERE id=$1`, id, hash)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return apperr.NotFound("El usuario")
		}
		_, err = tx.Exec(ctx, `UPDATE refresh_tokens SET revoked_at = now() WHERE user_id = $1 AND revoked_at IS NULL`, id)
		return err
	})
}

// Delete removes a user. Accounts with history should be deactivated instead; the FK rules keep bookings and orders.
func (s *Service) Delete(ctx context.Context, by *actor.Actor, id int64) error {
	if by != nil && by.ID == id {
		return apperr.Invalid("self_delete", "No puedes eliminar tu propia cuenta.")
	}
	tag, err := s.pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return apperr.NotFound("El usuario")
	}
	return nil
}

// EnsureAdmin creates the first admin from config when there is none.
func (s *Service) EnsureAdmin(ctx context.Context, email, password string) (bool, error) {
	var n int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM users WHERE role = 'admin'`).Scan(&n); err != nil {
		return false, err
	}
	if n > 0 || email == "" || password == "" {
		return false, nil
	}
	_, err := s.Create(ctx, CreateInput{Email: email, Password: password, FullName: "Admin Blood Sand", Role: actor.RoleAdmin})
	return err == nil, err
}
