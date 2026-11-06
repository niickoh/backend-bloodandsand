// Package schedule manages the weekly timetable (templates) and the concrete class sessions.
package schedule

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/bloodandsand/backend/internal/platform/apperr"
	"github.com/bloodandsand/backend/internal/platform/db"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Template struct {
	ID           int64   `json:"id"`
	DisciplineID int64   `json:"disciplineId"`
	Discipline   string  `json:"discipline"`
	CoachID      *int64  `json:"coachId"`
	CoachName    *string `json:"coachName"`
	Weekday      int     `json:"weekday"` // 1 = Monday … 7 = Sunday
	StartTime    string  `json:"startTime"`
	Capacity     int     `json:"capacity"`
	Active       bool    `json:"active"`
}

type Session struct {
	ID           int64     `json:"id"`
	TemplateID   *int64    `json:"templateId"`
	DisciplineID int64     `json:"disciplineId"`
	Discipline   string    `json:"discipline"`
	Slug         string    `json:"slug"`
	Price        int64     `json:"price"`
	UsesCredit   bool      `json:"usesCredit"`
	CoachID      *int64    `json:"coachId"`
	CoachName    *string   `json:"coachName"`
	StartsAt     time.Time `json:"startsAt"`
	EndsAt       time.Time `json:"endsAt"`
	Date         string    `json:"date"`  // local yyyy-mm-dd
	Start        string    `json:"start"` // local HH:MM
	End          string    `json:"end"`
	Capacity     int       `json:"capacity"`
	Taken        int       `json:"taken"`
	Status       string    `json:"status"`
	Note         string    `json:"note"`
	BookedByMe   bool      `json:"bookedByMe"`
}

// ActiveBookingSQL is the condition for a booking that occupies a seat.
const ActiveBookingSQL = `(b.status IN ('confirmed', 'attended') OR (b.status = 'pending_payment' AND b.hold_expires_at > now()))`

type Service struct {
	pool *pgxpool.Pool
	loc  *time.Location
}

func NewService(pool *pgxpool.Pool, loc *time.Location) *Service { return &Service{pool: pool, loc: loc} }

func (s *Service) Location() *time.Location { return s.loc }

// Weekday returns 1 = Monday … 7 = Sunday.
func Weekday(t time.Time) int { return (int(t.Weekday())+6)%7 + 1 }

func (s *Service) localize(se *Session) {
	st, en := se.StartsAt.In(s.loc), se.EndsAt.In(s.loc)
	se.Date, se.Start, se.End = st.Format(time.DateOnly), st.Format("15:04"), en.Format("15:04")
}

// ---- Templates ----

const tplSelect = `SELECT t.id, t.discipline_id, d.name, t.coach_id, u.full_name, t.weekday, to_char(t.start_time, 'HH24:MI'), t.capacity, t.active
	FROM class_templates t JOIN disciplines d ON d.id = t.discipline_id LEFT JOIN users u ON u.id = t.coach_id`

func scanTpl(r interface{ Scan(...any) error }) (Template, error) {
	var t Template
	err := r.Scan(&t.ID, &t.DisciplineID, &t.Discipline, &t.CoachID, &t.CoachName, &t.Weekday, &t.StartTime, &t.Capacity, &t.Active)
	return t, err
}

func (s *Service) Templates(ctx context.Context, onlyActive bool) ([]Template, error) {
	rows, err := s.pool.Query(ctx, tplSelect+` WHERE NOT $1 OR (t.active AND d.active) ORDER BY t.weekday, t.start_time`, onlyActive)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Template{}
	for rows.Next() {
		t, err := scanTpl(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

type TemplateInput struct {
	DisciplineID int64  `json:"disciplineId"`
	CoachID      *int64 `json:"coachId"`
	Weekday      int    `json:"weekday"`
	StartTime    string `json:"startTime"`
	Capacity     int    `json:"capacity"`
	Active       bool   `json:"active"`
}

func (in *TemplateInput) validate() error {
	fe := apperr.FieldErrors{}
	fe.Check(in.DisciplineID > 0, "disciplineId", "Elige una disciplina.")
	fe.Check(in.Weekday >= 1 && in.Weekday <= 7, "weekday", "Elige un día de la semana.")
	_, err := time.Parse("15:04", in.StartTime)
	fe.Check(err == nil, "startTime", "Usa el formato HH:MM, por ejemplo 07:30.")
	fe.Check(in.Capacity > 0 && in.Capacity <= 200, "capacity", "Los cupos deben estar entre 1 y 200.")
	return fe.Err()
}

func (s *Service) checkCoach(ctx context.Context, q db.Querier, coachID *int64) error {
	if coachID == nil {
		return nil
	}
	var role string
	err := q.QueryRow(ctx, `SELECT role FROM users WHERE id = $1 AND active`, *coachID).Scan(&role)
	if db.NoRows(err) || (err == nil && role != "coach" && role != "admin") {
		return apperr.Fields(map[string]string{"coachId": "Elige un coach activo."})
	}
	return err
}

func (s *Service) CreateTemplate(ctx context.Context, in TemplateInput) (Template, error) {
	if err := in.validate(); err != nil {
		return Template{}, err
	}
	if err := s.checkCoach(ctx, s.pool, in.CoachID); err != nil {
		return Template{}, err
	}
	var id int64
	err := s.pool.QueryRow(ctx, `INSERT INTO class_templates (discipline_id, coach_id, weekday, start_time, capacity, active)
		VALUES ($1, $2, $3, $4::time, $5, $6) RETURNING id`, in.DisciplineID, in.CoachID, in.Weekday, in.StartTime, in.Capacity, in.Active).Scan(&id)
	if db.IsForeignKeyViolation(err) {
		return Template{}, apperr.Fields(map[string]string{"disciplineId": "La disciplina no existe."})
	}
	if err != nil {
		return Template{}, err
	}
	return scanTpl(s.pool.QueryRow(ctx, tplSelect+` WHERE t.id = $1`, id))
}

// UpdateTemplate changes the template and propagates to future sessions:
// coach, capacity and discipline are updated in place; a new day or time regenerates the sessions
// that have no bookings yet (sessions with bookings stay, so nobody loses a seat silently).
func (s *Service) UpdateTemplate(ctx context.Context, id int64, in TemplateInput, daysAhead int) (Template, int, error) {
	if err := in.validate(); err != nil {
		return Template{}, 0, err
	}
	kept := 0
	err := db.WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		if err := s.checkCoach(ctx, tx, in.CoachID); err != nil {
			return err
		}
		var oldWeekday int
		var oldStart string
		err := tx.QueryRow(ctx, `SELECT weekday, to_char(start_time, 'HH24:MI') FROM class_templates WHERE id = $1 FOR UPDATE`, id).Scan(&oldWeekday, &oldStart)
		if db.NoRows(err) {
			return apperr.NotFound("El horario")
		}
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE class_templates SET discipline_id=$2, coach_id=$3, weekday=$4, start_time=$5::time, capacity=$6, active=$7 WHERE id=$1`,
			id, in.DisciplineID, in.CoachID, in.Weekday, in.StartTime, in.Capacity, in.Active); err != nil {
			return err
		}
		future := `template_id = $1 AND starts_at > now() AND status = 'scheduled'`
		if _, err := tx.Exec(ctx, `UPDATE class_sessions s SET coach_id=$2, capacity=GREATEST($3, (SELECT count(*) FROM bookings b WHERE b.session_id = s.id AND `+ActiveBookingSQL+`)),
			discipline_id=$4, ends_at = starts_at + (SELECT duration_min FROM disciplines WHERE id = $4) * interval '1 minute', updated_at=now() WHERE `+future,
			id, in.CoachID, in.Capacity, in.DisciplineID); err != nil {
			return err
		}
		if oldWeekday != in.Weekday || oldStart != in.StartTime || !in.Active {
			if _, err := tx.Exec(ctx, `DELETE FROM class_sessions WHERE `+future+` AND NOT EXISTS (SELECT 1 FROM bookings b WHERE b.session_id = class_sessions.id)`, id); err != nil {
				return err
			}
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM class_sessions WHERE `+future, id).Scan(&kept); err != nil {
				return err
			}
			// Detach the kept ones so the generator can create the new slot.
			if _, err := tx.Exec(ctx, `UPDATE class_sessions SET template_id = NULL WHERE `+future, id); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return Template{}, 0, err
	}
	if _, err := s.Generate(ctx, daysAhead); err != nil {
		return Template{}, kept, err
	}
	t, err := scanTpl(s.pool.QueryRow(ctx, tplSelect+` WHERE t.id = $1`, id))
	return t, kept, err
}

// DeleteTemplate removes it and its future empty sessions. Returns how many booked sessions were kept.
func (s *Service) DeleteTemplate(ctx context.Context, id int64) (int, error) {
	kept := 0
	err := db.WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `DELETE FROM class_sessions WHERE template_id = $1 AND starts_at > now()
			AND NOT EXISTS (SELECT 1 FROM bookings b WHERE b.session_id = class_sessions.id)`, id); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM class_sessions WHERE template_id = $1 AND starts_at > now() AND status = 'scheduled'`, id).Scan(&kept); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `DELETE FROM class_templates WHERE id = $1`, id)
		if err == nil && tag.RowsAffected() == 0 {
			return apperr.NotFound("El horario")
		}
		return err
	})
	return kept, err
}

// Generate materializes sessions from active templates for the next `days` days. Idempotent.
func (s *Service) Generate(ctx context.Context, days int) (int, error) {
	tpls, err := s.Templates(ctx, true)
	if err != nil {
		return 0, err
	}
	durations := map[int64]int{}
	rows, err := s.pool.Query(ctx, `SELECT id, duration_min FROM disciplines`)
	if err != nil {
		return 0, err
	}
	for rows.Next() {
		var id int64
		var d int
		if err := rows.Scan(&id, &d); err != nil {
			rows.Close()
			return 0, err
		}
		durations[id] = d
	}
	rows.Close()

	today := time.Now().In(s.loc)
	today = time.Date(today.Year(), today.Month(), today.Day(), 0, 0, 0, 0, s.loc)
	batch := &pgx.Batch{}
	for i := 0; i <= days; i++ {
		day := today.AddDate(0, 0, i)
		wd := Weekday(day)
		for _, t := range tpls {
			if t.Weekday != wd {
				continue
			}
			hm, _ := time.Parse("15:04", t.StartTime)
			start := time.Date(day.Year(), day.Month(), day.Day(), hm.Hour(), hm.Minute(), 0, 0, s.loc)
			if start.Before(time.Now()) {
				continue
			}
			end := start.Add(time.Duration(durations[t.DisciplineID]) * time.Minute)
			batch.Queue(`INSERT INTO class_sessions (template_id, discipline_id, coach_id, starts_at, ends_at, capacity)
				VALUES ($1, $2, $3, $4, $5, $6) ON CONFLICT (template_id, starts_at) WHERE template_id IS NOT NULL DO NOTHING`,
				t.ID, t.DisciplineID, t.CoachID, start, end, t.Capacity)
		}
	}
	if batch.Len() == 0 {
		return 0, nil
	}
	br := s.pool.SendBatch(ctx, batch)
	defer br.Close()
	created := 0
	for range batch.Len() {
		tag, err := br.Exec()
		if err != nil {
			return created, err
		}
		created += int(tag.RowsAffected())
	}
	return created, nil
}

// ---- Sessions ----

const sessSelect = `SELECT s.id, s.template_id, s.discipline_id, d.name, d.slug, d.price, d.uses_credit, s.coach_id, u.full_name,
	s.starts_at, s.ends_at, s.capacity,
	(SELECT count(*) FROM bookings b WHERE b.session_id = s.id AND ` + ActiveBookingSQL + `),
	s.status, s.note,
	EXISTS (SELECT 1 FROM bookings b WHERE b.session_id = s.id AND b.user_id = $1 AND ` + ActiveBookingSQL + `)
	FROM class_sessions s JOIN disciplines d ON d.id = s.discipline_id LEFT JOIN users u ON u.id = s.coach_id`

func (s *Service) scanSession(r interface{ Scan(...any) error }) (Session, error) {
	var se Session
	err := r.Scan(&se.ID, &se.TemplateID, &se.DisciplineID, &se.Discipline, &se.Slug, &se.Price, &se.UsesCredit, &se.CoachID, &se.CoachName,
		&se.StartsAt, &se.EndsAt, &se.Capacity, &se.Taken, &se.Status, &se.Note, &se.BookedByMe)
	if err == nil {
		s.localize(&se)
	}
	return se, err
}

type SessionFilter struct {
	From, To       time.Time
	DisciplineSlug string
	CoachID        int64
	ViewerID       int64 // marks bookedByMe
	IncludeAll     bool  // include cancelled sessions and inactive disciplines (staff views)
}

func (s *Service) Sessions(ctx context.Context, f SessionFilter) ([]Session, error) {
	q := sessSelect + ` WHERE s.starts_at >= $2 AND s.starts_at < $3
		AND ($4 = '' OR d.slug = $4) AND ($5 = 0 OR s.coach_id = $5)
		AND ($6 OR (s.status = 'scheduled' AND d.active))
		ORDER BY s.starts_at, d.sort`
	rows, err := s.pool.Query(ctx, q, f.ViewerID, f.From, f.To, f.DisciplineSlug, f.CoachID, f.IncludeAll)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Session{}
	for rows.Next() {
		se, err := s.scanSession(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, se)
	}
	return out, rows.Err()
}

// Session loads one session. q may be a transaction; lock adds FOR UPDATE OF s to serialize bookings.
func (s *Service) Session(ctx context.Context, q db.Querier, id, viewerID int64, lock bool) (Session, error) {
	sql := sessSelect + ` WHERE s.id = $2`
	if lock {
		sql += ` FOR UPDATE OF s`
	}
	se, err := s.scanSession(q.QueryRow(ctx, sql, viewerID, id))
	if db.NoRows(err) {
		return se, apperr.NotFound("La clase")
	}
	return se, err
}

type SessionInput struct {
	DisciplineID int64  `json:"disciplineId"`
	CoachID      *int64 `json:"coachId"`
	Date         string `json:"date"`  // yyyy-mm-dd, local
	Start        string `json:"start"` // HH:MM, local
	Capacity     int    `json:"capacity"`
	Note         string `json:"note"`
}

func (s *Service) parseStart(date, start string) (time.Time, error) {
	t, err := time.ParseInLocation("2006-01-02 15:04", date+" "+start, s.loc)
	if err != nil {
		return t, apperr.Fields(map[string]string{"date": "Usa fecha AAAA-MM-DD y hora HH:MM."})
	}
	return t, nil
}

func (s *Service) CreateSession(ctx context.Context, in SessionInput) (Session, error) {
	fe := apperr.FieldErrors{}
	fe.Check(in.DisciplineID > 0, "disciplineId", "Elige una disciplina.")
	fe.Check(in.Capacity > 0 && in.Capacity <= 200, "capacity", "Los cupos deben estar entre 1 y 200.")
	if err := fe.Err(); err != nil {
		return Session{}, err
	}
	start, err := s.parseStart(in.Date, in.Start)
	if err != nil {
		return Session{}, err
	}
	if start.Before(time.Now()) {
		return Session{}, apperr.Fields(map[string]string{"date": "La clase debe ser en el futuro."})
	}
	if err := s.checkCoach(ctx, s.pool, in.CoachID); err != nil {
		return Session{}, err
	}
	var id int64
	err = s.pool.QueryRow(ctx, `INSERT INTO class_sessions (discipline_id, coach_id, starts_at, ends_at, capacity, note)
		SELECT $1, $2, $3, $3 + duration_min * interval '1 minute', $4, $5 FROM disciplines WHERE id = $1 RETURNING id`,
		in.DisciplineID, in.CoachID, start, in.Capacity, strings.TrimSpace(in.Note)).Scan(&id)
	if db.NoRows(err) {
		return Session{}, apperr.Fields(map[string]string{"disciplineId": "La disciplina no existe."})
	}
	if err != nil {
		return Session{}, err
	}
	return s.Session(ctx, s.pool, id, 0, false)
}

type SessionPatch struct {
	CoachID  *int64  `json:"coachId"`
	Date     *string `json:"date"`
	Start    *string `json:"start"`
	Capacity *int    `json:"capacity"`
	Note     *string `json:"note"`
}

// UpdateSession edits one session (not the template). Capacity can't go below current bookings.
func (s *Service) UpdateSession(ctx context.Context, q db.Querier, id int64, p SessionPatch) (Session, bool, error) {
	cur, err := s.Session(ctx, q, id, 0, true)
	if err != nil {
		return cur, false, err
	}
	if cur.Status == "cancelled" {
		return cur, false, apperr.Conflict("session_cancelled", "La clase está cancelada.")
	}
	moved := false
	start := cur.StartsAt
	if p.Date != nil || p.Start != nil {
		d, st := cur.Date, cur.Start
		if p.Date != nil {
			d = *p.Date
		}
		if p.Start != nil {
			st = *p.Start
		}
		if start, err = s.parseStart(d, st); err != nil {
			return cur, false, err
		}
		moved = !start.Equal(cur.StartsAt)
	}
	capacity := cur.Capacity
	if p.Capacity != nil {
		if *p.Capacity < cur.Taken || *p.Capacity <= 0 {
			return cur, false, apperr.Fields(map[string]string{"capacity": fmt.Sprintf("Hay %d personas reservadas; los cupos no pueden ser menos.", cur.Taken)})
		}
		capacity = *p.Capacity
	}
	coach := cur.CoachID
	if p.CoachID != nil {
		if *p.CoachID == 0 {
			coach = nil
		} else {
			if err := s.checkCoach(ctx, q, p.CoachID); err != nil {
				return cur, false, err
			}
			coach = p.CoachID
		}
	}
	note := cur.Note
	if p.Note != nil {
		note = strings.TrimSpace(*p.Note)
	}
	dur := cur.EndsAt.Sub(cur.StartsAt)
	if _, err := q.Exec(ctx, `UPDATE class_sessions SET coach_id=$2, starts_at=$3, ends_at=$4, capacity=$5, note=$6, updated_at=now() WHERE id=$1`,
		id, coach, start, start.Add(dur), capacity, note); err != nil {
		return cur, false, err
	}
	out, err := s.Session(ctx, q, id, 0, false)
	return out, moved, err
}

// MarkCancelled sets the status; booking side effects are handled by the booking module.
func (s *Service) MarkCancelled(ctx context.Context, q db.Querier, id int64, note string) error {
	_, err := q.Exec(ctx, `UPDATE class_sessions SET status='cancelled', note=CASE WHEN $2 = '' THEN note ELSE $2 END, updated_at=now() WHERE id=$1`, id, strings.TrimSpace(note))
	return err
}
