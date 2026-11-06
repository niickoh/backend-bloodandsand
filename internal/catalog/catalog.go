// Package catalog holds what the gym sells: disciplines and plans.
package catalog

import (
	"context"
	"regexp"
	"strings"

	"github.com/bloodandsand/backend/internal/platform/apperr"
	"github.com/bloodandsand/backend/internal/platform/db"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Discipline struct {
	ID          int64  `json:"id"`
	Slug        string `json:"slug"`
	Name        string `json:"name"`
	Tagline     string `json:"tagline"`
	Short       string `json:"short"`
	Description string `json:"description"`
	DurationMin int    `json:"durationMin"`
	Price       int64  `json:"price"`
	Image       string `json:"image"`
	UsesCredit  bool   `json:"usesCredit"`
	Active      bool   `json:"active"`
	Sort        int    `json:"sort"`
}

type Plan struct {
	ID                int64    `json:"id"`
	Slug              string   `json:"slug"`
	Name              string   `json:"name"`
	Price             int64    `json:"price"`
	PeriodDays        int      `json:"periodDays"`
	Credits           *int     `json:"credits"` // nil = unlimited
	BookingWindowDays int      `json:"bookingWindowDays"`
	Perks             []string `json:"perks"`
	Featured          bool     `json:"featured"`
	Sellable          bool     `json:"sellable"`
	Active            bool     `json:"active"`
	Sort              int      `json:"sort"`
}

const (
	discCols = `id, slug, name, tagline, short, description, duration_min, price, image, uses_credit, active, sort`
	planCols = `id, slug, name, price, period_days, credits, booking_window_days, perks, featured, sellable, active, sort`
)

func scanDisc(r interface{ Scan(...any) error }) (Discipline, error) {
	var d Discipline
	err := r.Scan(&d.ID, &d.Slug, &d.Name, &d.Tagline, &d.Short, &d.Description, &d.DurationMin, &d.Price, &d.Image, &d.UsesCredit, &d.Active, &d.Sort)
	return d, err
}

func scanPlan(r interface{ Scan(...any) error }) (Plan, error) {
	var p Plan
	err := r.Scan(&p.ID, &p.Slug, &p.Name, &p.Price, &p.PeriodDays, &p.Credits, &p.BookingWindowDays, &p.Perks, &p.Featured, &p.Sellable, &p.Active, &p.Sort)
	return p, err
}

type Service struct{ pool *pgxpool.Pool }

func NewService(pool *pgxpool.Pool) *Service { return &Service{pool: pool} }

func (s *Service) Disciplines(ctx context.Context, onlyActive bool) ([]Discipline, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+discCols+` FROM disciplines WHERE NOT $1 OR active ORDER BY sort, id`, onlyActive)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Discipline{}
	for rows.Next() {
		d, err := scanDisc(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func (s *Service) Discipline(ctx context.Context, id int64) (Discipline, error) {
	d, err := scanDisc(s.pool.QueryRow(ctx, `SELECT `+discCols+` FROM disciplines WHERE id = $1`, id))
	if db.NoRows(err) {
		return d, apperr.NotFound("La disciplina")
	}
	return d, err
}

func (s *Service) Plans(ctx context.Context, onlyActive bool) ([]Plan, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+planCols+` FROM plans WHERE NOT $1 OR active ORDER BY sort, id`, onlyActive)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Plan{}
	for rows.Next() {
		p, err := scanPlan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Service) Plan(ctx context.Context, id int64) (Plan, error) {
	p, err := scanPlan(s.pool.QueryRow(ctx, `SELECT `+planCols+` FROM plans WHERE id = $1`, id))
	if db.NoRows(err) {
		return p, apperr.NotFound("El plan")
	}
	return p, err
}

func (s *Service) PlanBySlug(ctx context.Context, slug string) (Plan, error) {
	p, err := scanPlan(s.pool.QueryRow(ctx, `SELECT `+planCols+` FROM plans WHERE slug = $1`, slug))
	if db.NoRows(err) {
		return p, apperr.NotFound("El plan")
	}
	return p, err
}

var slugRe = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

func (d *Discipline) validate() error {
	d.Slug = strings.ToLower(strings.TrimSpace(d.Slug))
	d.Name = strings.TrimSpace(d.Name)
	fe := apperr.FieldErrors{}
	fe.Check(slugRe.MatchString(d.Slug), "slug", "Usa minúsculas, números y guiones (ej: box-kids).")
	fe.Check(d.Name != "" && len(d.Name) <= 60, "name", "Escribe un nombre de hasta 60 caracteres.")
	fe.Check(d.DurationMin >= 10 && d.DurationMin <= 240, "durationMin", "La duración debe estar entre 10 y 240 minutos.")
	fe.Check(d.Price >= 0, "price", "El precio no puede ser negativo.")
	return fe.Err()
}

func (s *Service) SaveDiscipline(ctx context.Context, d Discipline) (Discipline, error) {
	if err := d.validate(); err != nil {
		return d, err
	}
	var row interface{ Scan(...any) error }
	if d.ID == 0 {
		row = s.pool.QueryRow(ctx, `INSERT INTO disciplines (slug, name, tagline, short, description, duration_min, price, image, uses_credit, active, sort)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11) RETURNING `+discCols,
			d.Slug, d.Name, d.Tagline, d.Short, d.Description, d.DurationMin, d.Price, d.Image, d.UsesCredit, d.Active, d.Sort)
	} else {
		row = s.pool.QueryRow(ctx, `UPDATE disciplines SET slug=$2, name=$3, tagline=$4, short=$5, description=$6, duration_min=$7,
			price=$8, image=$9, uses_credit=$10, active=$11, sort=$12 WHERE id=$1 RETURNING `+discCols,
			d.ID, d.Slug, d.Name, d.Tagline, d.Short, d.Description, d.DurationMin, d.Price, d.Image, d.UsesCredit, d.Active, d.Sort)
	}
	out, err := scanDisc(row)
	switch {
	case db.NoRows(err):
		return out, apperr.NotFound("La disciplina")
	case db.IsUniqueViolation(err, ""):
		return out, apperr.Fields(map[string]string{"slug": "Ya existe una disciplina con este identificador."})
	}
	return out, err
}

func (p *Plan) validate() error {
	p.Slug = strings.ToLower(strings.TrimSpace(p.Slug))
	p.Name = strings.TrimSpace(p.Name)
	fe := apperr.FieldErrors{}
	fe.Check(slugRe.MatchString(p.Slug), "slug", "Usa minúsculas, números y guiones (ej: plan-12).")
	fe.Check(p.Name != "" && len(p.Name) <= 60, "name", "Escribe un nombre de hasta 60 caracteres.")
	fe.Check(p.Price > 0, "price", "El precio debe ser mayor a 0.")
	fe.Check(p.PeriodDays > 0 && p.PeriodDays <= 366, "periodDays", "La duración debe estar entre 1 y 366 días.")
	fe.Check(p.Credits == nil || *p.Credits > 0, "credits", "Las clases deben ser al menos 1, o deja vacío para ilimitado.")
	fe.Check(p.BookingWindowDays > 0 && p.BookingWindowDays <= 60, "bookingWindowDays", "Entre 1 y 60 días.")
	if p.Perks == nil {
		p.Perks = []string{}
	}
	return fe.Err()
}

func (s *Service) SavePlan(ctx context.Context, p Plan) (Plan, error) {
	if err := p.validate(); err != nil {
		return p, err
	}
	var row interface{ Scan(...any) error }
	if p.ID == 0 {
		row = s.pool.QueryRow(ctx, `INSERT INTO plans (slug, name, price, period_days, credits, booking_window_days, perks, featured, sellable, active, sort)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11) RETURNING `+planCols,
			p.Slug, p.Name, p.Price, p.PeriodDays, p.Credits, p.BookingWindowDays, p.Perks, p.Featured, p.Sellable, p.Active, p.Sort)
	} else {
		row = s.pool.QueryRow(ctx, `UPDATE plans SET slug=$2, name=$3, price=$4, period_days=$5, credits=$6, booking_window_days=$7,
			perks=$8, featured=$9, sellable=$10, active=$11, sort=$12 WHERE id=$1 RETURNING `+planCols,
			p.ID, p.Slug, p.Name, p.Price, p.PeriodDays, p.Credits, p.BookingWindowDays, p.Perks, p.Featured, p.Sellable, p.Active, p.Sort)
	}
	out, err := scanPlan(row)
	switch {
	case db.NoRows(err):
		return out, apperr.NotFound("El plan")
	case db.IsUniqueViolation(err, ""):
		return out, apperr.Fields(map[string]string{"slug": "Ya existe un plan con este identificador."})
	}
	return out, err
}

// Delete* only succeed for unused rows; used ones should be deactivated instead.
func (s *Service) DeleteDiscipline(ctx context.Context, id int64) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM disciplines WHERE id = $1`, id)
	if db.IsForeignKeyViolation(err) {
		return apperr.Conflict("in_use", "Esta disciplina tiene clases asociadas. Desactívala en vez de eliminarla.")
	}
	if err == nil && tag.RowsAffected() == 0 {
		return apperr.NotFound("La disciplina")
	}
	return err
}

func (s *Service) DeletePlan(ctx context.Context, id int64) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM plans WHERE id = $1`, id)
	if err == nil && tag.RowsAffected() == 0 {
		return apperr.NotFound("El plan")
	}
	return err
}
