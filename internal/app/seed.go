package app

import (
	"context"
	"fmt"

	"github.com/bloodandsand/backend/internal/platform/actor"
	"github.com/bloodandsand/backend/internal/schedule"
	"github.com/bloodandsand/backend/internal/user"
)

// SeedDemo creates demo coaches, a student and the weekly timetable. Only for development.
// Idempotent: skips everything if templates already exist.
func (a *App) SeedDemo(ctx context.Context) error {
	var n int
	if err := a.Pool.QueryRow(ctx, `SELECT count(*) FROM class_templates`).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		a.Log.Info("seed: ya hay horario cargado, no se modifica nada")
		return nil
	}

	const pass = "demo1234"
	coaches := map[string]int64{}
	for _, c := range []struct{ key, name, email string }{
		{"camila", "Camila Rojas", "camila@bloodandsand.cl"},
		{"diego", "Diego Muñoz", "diego@bloodandsand.cl"},
		{"tomas", "Tomás Vera", "tomas@bloodandsand.cl"},
		{"javi", "Javiera Pinto", "javiera@bloodandsand.cl"},
	} {
		u, err := a.Users.Create(ctx, user.CreateInput{Email: c.email, Password: pass, FullName: c.name, Phone: "+56 9 1111 2222", Role: actor.RoleCoach})
		if err != nil {
			if existing, e := a.Users.ByEmail(ctx, c.email); e == nil {
				u = existing
			} else {
				return fmt.Errorf("coach %s: %w", c.email, err)
			}
		}
		coaches[c.key] = u.ID
	}
	if _, err := a.Users.Create(ctx, user.CreateInput{Email: "alumno@bloodandsand.cl", Password: pass, FullName: "Alumno Demo", Phone: "+56 9 3333 4444", Role: actor.RoleStudent}); err != nil {
		a.Log.Info("seed: alumno demo ya existía")
	}

	disc := map[string]int64{}
	rows, err := a.Pool.Query(ctx, `SELECT slug, id FROM disciplines`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var slug string
		var id int64
		if err := rows.Scan(&slug, &id); err != nil {
			rows.Close()
			return err
		}
		disc[slug] = id
	}
	rows.Close()

	type t struct {
		d     string
		wd    int
		start string
		coach string
		cap   int
	}
	week := []t{
		{"funcional", 1, "07:00", "camila", 14}, {"box", 1, "08:30", "diego", 12}, {"evaluacion", 1, "11:00", "camila", 1},
		{"fuerza", 1, "18:00", "tomas", 10}, {"box", 1, "19:30", "diego", 12}, {"funcional", 1, "20:45", "javi", 14},
		{"fuerza", 2, "07:00", "tomas", 10}, {"funcional", 2, "09:00", "javi", 14}, {"evaluacion", 2, "17:00", "tomas", 1},
		{"box", 2, "18:30", "diego", 12}, {"funcional", 2, "19:45", "camila", 14},
		{"funcional", 3, "07:00", "camila", 14}, {"box", 3, "08:30", "diego", 12}, {"evaluacion", 3, "12:00", "camila", 1},
		{"fuerza", 3, "18:00", "tomas", 10}, {"box", 3, "19:30", "diego", 12}, {"funcional", 3, "20:45", "javi", 14},
		{"fuerza", 4, "07:00", "tomas", 10}, {"funcional", 4, "09:00", "javi", 14}, {"evaluacion", 4, "17:00", "tomas", 1},
		{"box", 4, "18:30", "diego", 12}, {"fuerza", 4, "19:45", "tomas", 10},
		{"funcional", 5, "07:00", "camila", 14}, {"box", 5, "08:30", "diego", 12}, {"fuerza", 5, "18:00", "tomas", 10}, {"box", 5, "19:30", "diego", 12},
		{"evaluacion", 6, "09:00", "camila", 1}, {"box", 6, "10:00", "diego", 16}, {"funcional", 6, "11:15", "javi", 18},
	}
	for _, x := range week {
		coach := coaches[x.coach]
		if _, err := a.Schedule.CreateTemplate(ctx, schedule.TemplateInput{DisciplineID: disc[x.d], CoachID: &coach, Weekday: x.wd, StartTime: x.start, Capacity: x.cap, Active: true}); err != nil {
			return err
		}
	}
	created, err := a.Schedule.Generate(ctx, a.Cfg.GenerateDaysAhead)
	if err != nil {
		return err
	}
	a.Log.Info("seed demo listo", "coaches", len(coaches), "plantillas", len(week), "clases", created, "contraseña", pass)
	return nil
}
