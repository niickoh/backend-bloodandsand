// Package booking reserves seats in class sessions: pricing, capacity, cancellations and staff edits.
package booking

import (
	"context"
	"fmt"
	"log/slog"
	"net/mail"
	"strings"
	"time"

	"github.com/bloodandsand/backend/internal/membership"
	"github.com/bloodandsand/backend/internal/notify"
	"github.com/bloodandsand/backend/internal/platform/actor"
	"github.com/bloodandsand/backend/internal/platform/apperr"
	"github.com/bloodandsand/backend/internal/platform/db"
	"github.com/bloodandsand/backend/internal/platform/security"
	"github.com/bloodandsand/backend/internal/schedule"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	StatusPending   = "pending_payment"
	StatusConfirmed = "confirmed"
	StatusCancelled = "cancelled"
	StatusAttended  = "attended"
	StatusNoShow    = "no_show"

	PayFree  = "free"
	PayPlan  = "plan"
	PayPaid  = "paid"
	PayStaff = "staff" // added by a coach/admin without charge (courtesy, paid at the desk…)

	DefaultWindowDays = 14
)

type Booking struct {
	ID           int64      `json:"id"`
	Code         string     `json:"code"`
	SessionID    int64      `json:"sessionId"`
	Discipline   string     `json:"discipline"`
	Slug         string     `json:"slug"`
	CoachName    *string    `json:"coachName"`
	StartsAt     time.Time  `json:"startsAt"`
	EndsAt       time.Time  `json:"endsAt"`
	Date         string     `json:"date"`
	Start        string     `json:"start"`
	End          string     `json:"end"`
	UserID       *int64     `json:"userId"`
	Name         string     `json:"name"`
	Email        string     `json:"email"`
	Phone        string     `json:"phone"`
	Status       string     `json:"status"`
	Payment      string     `json:"payment"`
	Amount       int64      `json:"amount"`
	MembershipID *int64     `json:"membershipId"`
	OrderID      *int64     `json:"orderId"`
	Notes        string     `json:"notes"`
	CancelledAt  *time.Time `json:"cancelledAt"`
	CreatedAt    time.Time  `json:"createdAt"`
	CanCancel    bool       `json:"canCancel"`
	RefundsCredit bool      `json:"refundsCredit"`
}

const selectSQL = `SELECT b.id, b.code, b.session_id, d.name, d.slug, u.full_name, s.starts_at, s.ends_at,
	b.user_id, b.name, b.email, b.phone, b.status, b.payment, b.amount, b.membership_id, b.order_id, b.notes, b.cancelled_at, b.created_at
	FROM bookings b JOIN class_sessions s ON s.id = b.session_id JOIN disciplines d ON d.id = s.discipline_id
	LEFT JOIN users u ON u.id = s.coach_id`

// abandonedHold matches seats that were held for a payment that never went through.
const abandonedHold = `(b.status = 'cancelled' AND b.payment = 'paid' AND b.order_id IS NOT NULL
	AND EXISTS (SELECT 1 FROM orders o WHERE o.id = b.order_id AND o.status IN ('failed', 'expired', 'cancelled')))`

type Service struct {
	pool        *pgxpool.Pool
	log         *slog.Logger
	sched       *schedule.Service
	members     *membership.Service
	push        *notify.Service
	loc         *time.Location
	hold        time.Duration
	cancelLimit time.Duration
	remindAt    time.Duration
}

type Options struct {
	Hold, CancelLimit, ReminderBefore time.Duration
}

func NewService(pool *pgxpool.Pool, log *slog.Logger, sched *schedule.Service, members *membership.Service, push *notify.Service, o Options) *Service {
	return &Service{pool: pool, log: log, sched: sched, members: members, push: push, loc: sched.Location(),
		hold: o.Hold, cancelLimit: o.CancelLimit, remindAt: o.ReminderBefore}
}

func (s *Service) scan(r interface{ Scan(...any) error }) (Booking, error) {
	var b Booking
	err := r.Scan(&b.ID, &b.Code, &b.SessionID, &b.Discipline, &b.Slug, &b.CoachName, &b.StartsAt, &b.EndsAt,
		&b.UserID, &b.Name, &b.Email, &b.Phone, &b.Status, &b.Payment, &b.Amount, &b.MembershipID, &b.OrderID, &b.Notes, &b.CancelledAt, &b.CreatedAt)
	if err != nil {
		return b, err
	}
	st, en := b.StartsAt.In(s.loc), b.EndsAt.In(s.loc)
	b.Date, b.Start, b.End = st.Format(time.DateOnly), st.Format("15:04"), en.Format("15:04")
	active := b.Status == StatusConfirmed || b.Status == StatusPending
	b.CanCancel = active && b.StartsAt.After(time.Now())
	b.RefundsCredit = b.CanCancel && b.Payment == PayPlan && time.Until(b.StartsAt) > s.cancelLimit
	return b, nil
}

func (s *Service) list(ctx context.Context, q db.Querier, where string, args ...any) ([]Booking, error) {
	rows, err := q.Query(ctx, selectSQL+" "+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Booking{}
	for rows.Next() {
		b, err := s.scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

func (s *Service) Get(ctx context.Context, q db.Querier, id int64) (Booking, error) {
	b, err := s.scan(q.QueryRow(ctx, selectSQL+` WHERE b.id = $1`, id))
	if db.NoRows(err) {
		return b, apperr.NotFound("La reserva")
	}
	return b, err
}

// ---- Pricing ----

type Contact struct {
	Name  string `json:"name"`
	Email string `json:"email"`
	Phone string `json:"phone"`
}

func (c *Contact) Normalize() {
	c.Name = strings.Join(strings.Fields(c.Name), " ")
	c.Email = strings.ToLower(strings.TrimSpace(c.Email))
	c.Phone = strings.TrimSpace(c.Phone)
}

func (c Contact) Validate() error {
	fe := apperr.FieldErrors{}
	fe.Check(len(strings.Fields(c.Name)) >= 2 && len(c.Name) <= 120, "name", "Escribe tu nombre y apellido.")
	if _, err := mail.ParseAddress(c.Email); err != nil || !strings.Contains(c.Email, ".") {
		fe.Add("email", "Revisa el correo, por ejemplo nombre@correo.cl.")
	}
	digits := 0
	for _, r := range c.Phone {
		if r >= '0' && r <= '9' {
			digits++
		}
	}
	fe.Check(digits >= 8 && digits <= 15, "phone", "Necesitamos un teléfono de 8 a 15 dígitos para avisarte de cambios.")
	return fe.Err()
}

type Quote struct {
	Payment      string `json:"payment"` // free | plan | paid
	Amount       int64  `json:"amount"`
	Reason       string `json:"reason"`
	MembershipID *int64 `json:"membershipId,omitempty"`
}

// Price decides how a session would be paid by this person. q can be a transaction.
func (s *Service) Price(ctx context.Context, q db.Querier, who *actor.Actor, se schedule.Session, email string) (Quote, error) {
	if se.Price == 0 {
		return Quote{Payment: PayFree, Reason: "La " + strings.ToLower(se.Discipline) + " no tiene costo."}, nil
	}
	if who != nil {
		m, err := s.members.Current(ctx, q, who.ID, false)
		if err != nil {
			return Quote{}, err
		}
		if se.UsesCredit && m.HasCredit(se.StartsAt) {
			reason := "Incluida en tu plan " + m.PlanName + "."
			if m.CreditsLeft != nil {
				reason = fmt.Sprintf("Usa 1 de tus %d clases del plan %s.", *m.CreditsLeft, m.PlanName)
			}
			return Quote{Payment: PayPlan, Reason: reason, MembershipID: &m.ID}, nil
		}
	}
	first, err := s.isFirstClass(ctx, q, who, email)
	if err != nil {
		return Quote{}, err
	}
	if first {
		return Quote{Payment: PayFree, Reason: "Tu primera clase es gratis."}, nil
	}
	return Quote{Payment: PayPaid, Amount: se.Price, Reason: "Clase suelta."}, nil
}

// isFirstClass: nobody with this account or email has a paid-discipline booking that wasn't cancelled.
func (s *Service) isFirstClass(ctx context.Context, q db.Querier, who *actor.Actor, email string) (bool, error) {
	var uid int64
	if who != nil {
		uid = who.ID
		if email == "" {
			email = who.Email
		}
	}
	var exists bool
	err := q.QueryRow(ctx, `SELECT EXISTS (
		SELECT 1 FROM bookings b JOIN class_sessions s ON s.id = b.session_id JOIN disciplines d ON d.id = s.discipline_id
		WHERE d.price > 0 AND b.status <> 'cancelled' AND (b.user_id = $1 OR ($2 <> '' AND lower(b.email) = lower($2))))`, uid, email).Scan(&exists)
	return !exists, err
}

func (s *Service) Quote(ctx context.Context, who *actor.Actor, sessionID int64, email string) (Quote, error) {
	se, err := s.sched.Session(ctx, s.pool, sessionID, 0, false)
	if err != nil {
		return Quote{}, err
	}
	if err := s.checkBookable(se, DefaultWindowDays); err != nil {
		return Quote{}, err
	}
	return s.Price(ctx, s.pool, who, se, strings.ToLower(strings.TrimSpace(email)))
}

func (s *Service) checkBookable(se schedule.Session, windowDays int) error {
	switch {
	case se.Status != "scheduled":
		return apperr.Conflict("session_cancelled", "Esta clase fue cancelada. Elige otro horario.")
	case !se.StartsAt.After(time.Now()):
		return apperr.Conflict("session_started", "Esta clase ya comenzó. Elige otro horario.")
	case se.StartsAt.After(time.Now().AddDate(0, 0, windowDays+1)):
		return apperr.Conflict("outside_window", fmt.Sprintf("Puedes reservar hasta %d días antes.", windowDays))
	case se.Taken >= se.Capacity:
		return apperr.Conflict("session_full", "La clase se llenó. Elige otro horario.")
	}
	return nil
}

// ---- Creating bookings ----

type newBooking struct {
	SessionID    int64
	UserID       *int64
	Contact      Contact
	Status       string
	Payment      string
	Amount       int64
	MembershipID *int64
	CreatedBy    *int64
	Notes        string
	SkipWindow   bool // staff can book beyond the public window
	Force        bool // staff can overbook
}

// insert locks the session row, re-checks capacity and writes the booking. Must run in a transaction.
func (s *Service) insert(ctx context.Context, tx pgx.Tx, nb newBooking) (Booking, error) {
	se, err := s.sched.Session(ctx, tx, nb.SessionID, 0, true)
	if err != nil {
		return Booking{}, err
	}
	window := DefaultWindowDays
	if nb.SkipWindow {
		window = 3650
	}
	if nb.Force && se.Taken >= se.Capacity {
		se.Taken = se.Capacity - 1
	}
	if err := s.checkBookable(se, window); err != nil {
		return Booking{}, err
	}
	if nb.Payment == PayPlan && nb.MembershipID != nil {
		if err := s.members.UseCredit(ctx, tx, *nb.MembershipID); err != nil {
			return Booking{}, err
		}
	}
	var hold *time.Time
	if nb.Status == StatusPending {
		t := time.Now().Add(s.hold)
		hold = &t
	}
	for range 5 {
		code := "BS-" + security.ShortCode(5)
		var id int64
		err = tx.QueryRow(ctx, `INSERT INTO bookings (code, session_id, user_id, name, email, phone, status, payment, amount, membership_id, created_by, notes, hold_expires_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13) RETURNING id`,
			code, nb.SessionID, nb.UserID, nb.Contact.Name, nb.Contact.Email, nb.Contact.Phone, nb.Status, nb.Payment, nb.Amount,
			nb.MembershipID, nb.CreatedBy, nb.Notes, hold).Scan(&id)
		switch {
		case err == nil:
			return s.Get(ctx, tx, id)
		case db.IsUniqueViolation(err, "bookings_code_key"):
			continue
		case db.IsUniqueViolation(err, ""):
			return Booking{}, apperr.Conflict("already_booked", "Ya hay una reserva para esta persona en esta clase.")
		default:
			return Booking{}, err
		}
	}
	return Booking{}, fmt.Errorf("no se pudo generar un código único: %w", err)
}

type BookInput struct {
	SessionID int64 `json:"sessionId"`
	Contact
}

// Book confirms a free or plan-covered booking. Paid bookings go through the payment module (Hold).
func (s *Service) Book(ctx context.Context, who *actor.Actor, in BookInput) (Booking, error) {
	in.Normalize()
	if err := in.Validate(); err != nil {
		return Booking{}, err
	}
	var b Booking
	err := db.WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		se, err := s.sched.Session(ctx, tx, in.SessionID, 0, true)
		if err != nil {
			return err
		}
		q, err := s.Price(ctx, tx, who, se, in.Email)
		if err != nil {
			return err
		}
		if q.Payment == PayPaid {
			return apperr.Conflict("payment_required", "Esta clase requiere pago.")
		}
		nb := newBooking{SessionID: in.SessionID, Contact: in.Contact, Status: StatusConfirmed, Payment: q.Payment, MembershipID: q.MembershipID}
		if who != nil {
			nb.UserID = &who.ID
		}
		if q.Payment == PayPlan {
			if err := s.checkPlanWindow(ctx, tx, *q.MembershipID, se); err != nil {
				return err
			}
		}
		b, err = s.insert(ctx, tx, nb)
		return err
	})
	if err == nil {
		s.notifyBooked(b)
	}
	return b, err
}

func (s *Service) checkPlanWindow(ctx context.Context, q db.Querier, membershipID int64, se schedule.Session) error {
	var days *int
	if err := q.QueryRow(ctx, `SELECT p.booking_window_days FROM memberships m LEFT JOIN plans p ON p.id = m.plan_id WHERE m.id = $1`, membershipID).Scan(&days); err != nil {
		return err
	}
	if days != nil && se.StartsAt.After(time.Now().AddDate(0, 0, *days+1)) {
		return apperr.Conflict("outside_window", fmt.Sprintf("Con tu plan puedes reservar hasta %d días antes.", *days))
	}
	return nil
}

// Hold creates a pending booking that occupies a seat while the payment is processed. Runs inside the caller's tx.
func (s *Service) Hold(ctx context.Context, tx pgx.Tx, who *actor.Actor, in BookInput) (Booking, Quote, error) {
	in.Normalize()
	if err := in.Validate(); err != nil {
		return Booking{}, Quote{}, err
	}
	se, err := s.sched.Session(ctx, tx, in.SessionID, 0, true)
	if err != nil {
		return Booking{}, Quote{}, err
	}
	if err := s.checkBookable(se, DefaultWindowDays); err != nil {
		return Booking{}, Quote{}, err
	}
	q, err := s.Price(ctx, tx, who, se, in.Email)
	if err != nil {
		return Booking{}, q, err
	}
	if q.Payment != PayPaid {
		return Booking{}, q, apperr.Conflict("no_payment_needed", "Esta clase no requiere pago; confírmala directamente.")
	}
	nb := newBooking{SessionID: in.SessionID, Contact: in.Contact, Status: StatusPending, Payment: PayPaid, Amount: q.Amount}
	if who != nil {
		nb.UserID = &who.ID
	}
	b, err := s.insert(ctx, tx, nb)
	return b, q, err
}

// ConfirmPaid finalizes a held booking after payment. Idempotent.
func (s *Service) ConfirmPaid(ctx context.Context, q db.Querier, bookingID, orderID int64) (Booking, error) {
	if _, err := q.Exec(ctx, `UPDATE bookings SET status = 'confirmed', order_id = $2, hold_expires_at = NULL, cancelled_at = NULL, updated_at = now()
		WHERE id = $1 AND status IN ('pending_payment', 'cancelled')`, bookingID, orderID); err != nil {
		return Booking{}, err
	}
	return s.Get(ctx, q, bookingID)
}

// ReleaseHold frees the seat when a payment fails or is abandoned.
func (s *Service) ReleaseHold(ctx context.Context, q db.Querier, bookingID int64) error {
	_, err := q.Exec(ctx, `UPDATE bookings SET status = 'cancelled', cancelled_at = now(), hold_expires_at = NULL, updated_at = now()
		WHERE id = $1 AND status = 'pending_payment'`, bookingID)
	return err
}

// ---- Member actions ----

func (s *Service) ForUser(ctx context.Context, userID int64) ([]Booking, error) {
	return s.list(ctx, s.pool, `WHERE b.user_id = $1 AND b.status <> 'pending_payment' AND NOT `+abandonedHold+` ORDER BY s.starts_at DESC LIMIT 200`, userID)
}

// CancelOwn cancels the caller's booking. The plan credit comes back if it's early enough.
func (s *Service) CancelOwn(ctx context.Context, who *actor.Actor, id int64) (Booking, error) {
	var b Booking
	err := db.WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		var err error
		if b, err = s.Get(ctx, tx, id); err != nil {
			return err
		}
		if b.UserID == nil || *b.UserID != who.ID {
			return apperr.NotFound("La reserva")
		}
		if !b.CanCancel {
			return apperr.Conflict("cannot_cancel", "Esta reserva ya no se puede cancelar.")
		}
		return s.cancel(ctx, tx, &b, b.RefundsCredit)
	})
	return b, err
}

func (s *Service) cancel(ctx context.Context, tx pgx.Tx, b *Booking, refund bool) error {
	if _, err := tx.Exec(ctx, `UPDATE bookings SET status = 'cancelled', cancelled_at = now(), hold_expires_at = NULL, updated_at = now() WHERE id = $1`, b.ID); err != nil {
		return err
	}
	if refund && b.Payment == PayPlan && b.MembershipID != nil {
		if err := s.members.RefundCredit(ctx, tx, *b.MembershipID); err != nil {
			return err
		}
	}
	b.Status = StatusCancelled
	b.CanCancel, b.RefundsCredit = false, false
	return nil
}

// ---- Staff (admin + coach) ----

// authorizeSession: admins can act on any class; coaches only on the ones assigned to them.
func authorizeSession(who *actor.Actor, se schedule.Session) error {
	if who.IsAdmin() || (who.IsCoach() && se.CoachID != nil && *se.CoachID == who.ID) {
		return nil
	}
	return apperr.Forbidden()
}

type Roster struct {
	Session  schedule.Session `json:"session"`
	Bookings []Booking        `json:"bookings"`
}

func (s *Service) Roster(ctx context.Context, who *actor.Actor, sessionID int64) (Roster, error) {
	se, err := s.sched.Session(ctx, s.pool, sessionID, 0, false)
	if err != nil {
		return Roster{}, err
	}
	if err := authorizeSession(who, se); err != nil {
		return Roster{}, err
	}
	list, err := s.list(ctx, s.pool, `WHERE b.session_id = $1 AND NOT `+abandonedHold+` AND (b.status <> 'pending_payment' OR b.hold_expires_at > now())
		ORDER BY b.status = 'cancelled', b.created_at`, sessionID)
	return Roster{Session: se, Bookings: list}, err
}

type StaffCreateInput struct {
	UserID  *int64  `json:"userId"` // existing member, or…
	Contact         // …a guest
	Payment string  `json:"payment"` // staff | plan | free
	Notes   string  `json:"notes"`
	Force   bool    `json:"force"` // allow over capacity
}

func (s *Service) StaffCreate(ctx context.Context, who *actor.Actor, sessionID int64, in StaffCreateInput) (Booking, error) {
	var b Booking
	err := db.WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		se, err := s.sched.Session(ctx, tx, sessionID, 0, false)
		if err != nil {
			return err
		}
		if err := authorizeSession(who, se); err != nil {
			return err
		}
		nb := newBooking{SessionID: sessionID, Status: StatusConfirmed, CreatedBy: &who.ID, Notes: strings.TrimSpace(in.Notes), SkipWindow: true, Force: in.Force && who.IsAdmin()}
		if in.UserID != nil {
			if err := tx.QueryRow(ctx, `SELECT full_name, email, phone FROM users WHERE id = $1 AND active`, *in.UserID).
				Scan(&nb.Contact.Name, &nb.Contact.Email, &nb.Contact.Phone); err != nil {
				if db.NoRows(err) {
					return apperr.Fields(map[string]string{"userId": "El alumno no existe o está desactivado."})
				}
				return err
			}
			nb.UserID = in.UserID
		} else {
			in.Contact.Normalize()
			if err := in.Contact.Validate(); err != nil {
				return err
			}
			nb.Contact = in.Contact
		}
		switch in.Payment {
		case "", PayStaff:
			nb.Payment = PayStaff
		case PayFree:
			nb.Payment = PayFree
		case PayPlan:
			if nb.UserID == nil {
				return apperr.Fields(map[string]string{"payment": "Solo un alumno con cuenta puede usar su plan."})
			}
			m, err := s.members.Current(ctx, tx, *nb.UserID, true)
			if err != nil {
				return err
			}
			if !m.HasCredit(se.StartsAt) {
				return apperr.Fields(map[string]string{"payment": "El alumno no tiene un plan con clases disponibles."})
			}
			nb.Payment, nb.MembershipID = PayPlan, &m.ID
		default:
			return apperr.Fields(map[string]string{"payment": "Elige cómo se cubre la clase."})
		}
		b, err = s.insert(ctx, tx, nb)
		return err
	})
	if err == nil {
		s.notifyBooked(b)
	}
	return b, err
}

type StaffPatch struct {
	Status    *string `json:"status"`    // confirmed | attended | no_show | cancelled
	SessionID *int64  `json:"sessionId"` // move to another class
	Notes     *string `json:"notes"`
	Name      *string `json:"name"`
	Phone     *string `json:"phone"`
}

func (s *Service) StaffUpdate(ctx context.Context, who *actor.Actor, id int64, p StaffPatch) (Booking, error) {
	var b, before Booking
	err := db.WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		var err error
		if b, err = s.Get(ctx, tx, id); err != nil {
			return err
		}
		before = b
		cur, err := s.sched.Session(ctx, tx, b.SessionID, 0, false)
		if err != nil {
			return err
		}
		if err := authorizeSession(who, cur); err != nil {
			return err
		}

		if p.SessionID != nil && *p.SessionID != b.SessionID {
			target, err := s.sched.Session(ctx, tx, *p.SessionID, 0, true)
			if err != nil {
				return err
			}
			if err := authorizeSession(who, target); err != nil {
				return err
			}
			if err := s.checkBookable(target, 3650); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE bookings SET session_id = $2, reminder_sent_at = NULL, updated_at = now() WHERE id = $1`, id, target.ID); err != nil {
				if db.IsUniqueViolation(err, "") {
					return apperr.Conflict("already_booked", "Esta persona ya tiene reserva en la clase de destino.")
				}
				return err
			}
		}
		if p.Notes != nil || p.Name != nil || p.Phone != nil {
			name, phone, notes := b.Name, b.Phone, b.Notes
			if p.Name != nil {
				name = strings.Join(strings.Fields(*p.Name), " ")
			}
			if p.Phone != nil {
				phone = strings.TrimSpace(*p.Phone)
			}
			if p.Notes != nil {
				notes = strings.TrimSpace(*p.Notes)
			}
			if name == "" {
				return apperr.Fields(map[string]string{"name": "El nombre no puede quedar vacío."})
			}
			if _, err := tx.Exec(ctx, `UPDATE bookings SET name = $2, phone = $3, notes = $4, updated_at = now() WHERE id = $1`, id, name, phone, notes); err != nil {
				return err
			}
		}
		if p.Status != nil && *p.Status != b.Status {
			if err := s.changeStatus(ctx, tx, &b, *p.Status); err != nil {
				return err
			}
		}
		b, err = s.Get(ctx, tx, id)
		return err
	})
	if err != nil {
		return b, err
	}
	switch {
	case b.SessionID != before.SessionID:
		s.notifyPerson(b, notify.Message{Title: "Tu clase cambió", Body: fmt.Sprintf("%s ahora es el %s a las %s.", b.Discipline, longDate(b.StartsAt, s.loc), b.Start), URL: "/mis-reservas", Tag: b.Code})
	case b.Status == StatusCancelled && before.Status != StatusCancelled:
		s.notifyPerson(b, notify.Message{Title: "Reserva cancelada", Body: fmt.Sprintf("Tu reserva de %s del %s fue cancelada por el gimnasio.", b.Discipline, longDate(b.StartsAt, s.loc)), URL: "/mis-reservas", Tag: b.Code})
	}
	return b, nil
}

func (s *Service) changeStatus(ctx context.Context, tx pgx.Tx, b *Booking, status string) error {
	switch status {
	case StatusCancelled:
		return s.cancel(ctx, tx, b, true) // staff cancellations always return the credit
	case StatusConfirmed, StatusAttended, StatusNoShow:
		if b.Status == StatusCancelled || b.Status == StatusPending {
			// Reactivating takes a seat (and a credit) again.
			se, err := s.sched.Session(ctx, tx, b.SessionID, 0, true)
			if err != nil {
				return err
			}
			if se.Taken >= se.Capacity {
				return apperr.Conflict("session_full", "La clase está llena; libera un cupo antes de reactivar esta reserva.")
			}
			if b.Payment == PayPlan && b.MembershipID != nil {
				if err := s.members.UseCredit(ctx, tx, *b.MembershipID); err != nil {
					return err
				}
			}
		}
		_, err := tx.Exec(ctx, `UPDATE bookings SET status = $2, cancelled_at = NULL, hold_expires_at = NULL, updated_at = now() WHERE id = $1`, b.ID, status)
		return err
	default:
		return apperr.Fields(map[string]string{"status": "Estado inválido."})
	}
}

// StaffDelete removes a booking permanently (e.g. created by mistake), returning the credit.
func (s *Service) StaffDelete(ctx context.Context, who *actor.Actor, id int64) error {
	return db.WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		b, err := s.Get(ctx, tx, id)
		if err != nil {
			return err
		}
		se, err := s.sched.Session(ctx, tx, b.SessionID, 0, false)
		if err != nil {
			return err
		}
		if err := authorizeSession(who, se); err != nil {
			return err
		}
		if b.OrderID != nil && !who.IsAdmin() {
			return apperr.Conflict("has_payment", "Esta reserva tiene un pago asociado; cancélala en vez de eliminarla.")
		}
		if b.Status != StatusCancelled && b.Payment == PayPlan && b.MembershipID != nil {
			if err := s.members.RefundCredit(ctx, tx, *b.MembershipID); err != nil {
				return err
			}
		}
		_, err = tx.Exec(ctx, `DELETE FROM bookings WHERE id = $1`, id)
		return err
	})
}

// ---- Admin ----

type AdminFilter struct {
	From, To  time.Time
	Status    string
	Q         string
	SessionID int64
	UserID    int64
	Limit     int
	Offset    int
}

func (s *Service) AdminList(ctx context.Context, f AdminFilter) ([]Booking, int, error) {
	where := `WHERE s.starts_at >= $1 AND s.starts_at < $2 AND ($3 = '' OR b.status = $3)
		AND ($4 = '' OR b.name ILIKE '%' || $4 || '%' OR b.email ILIKE '%' || $4 || '%' OR b.code ILIKE '%' || $4 || '%')
		AND ($5 = 0 OR b.session_id = $5) AND ($6 = 0 OR b.user_id = $6)`
	args := []any{f.From, f.To, f.Status, strings.TrimSpace(f.Q), f.SessionID, f.UserID}
	var total int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM bookings b JOIN class_sessions s ON s.id = b.session_id `+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	list, err := s.list(ctx, s.pool, where+` ORDER BY s.starts_at, b.created_at LIMIT $7 OFFSET $8`, append(args, f.Limit, f.Offset)...)
	return list, total, err
}

// UpdateSession edits a class and tells booked people if the time changed.
func (s *Service) UpdateSession(ctx context.Context, id int64, p schedule.SessionPatch) (schedule.Session, error) {
	var se schedule.Session
	var moved bool
	err := db.WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		var err error
		se, moved, err = s.sched.UpdateSession(ctx, tx, id, p)
		if err == nil && moved {
			_, err = tx.Exec(ctx, `UPDATE bookings SET reminder_sent_at = NULL WHERE session_id = $1`, id)
		}
		return err
	})
	if err == nil && moved {
		s.notifySession(id, notify.Message{Title: "Tu clase cambió de horario",
			Body: fmt.Sprintf("%s ahora es el %s a las %s.", se.Discipline, longDate(se.StartsAt, s.loc), se.Start), URL: "/mis-reservas"})
	}
	return se, err
}

// CancelSession cancels a class and all its bookings, returning plan credits. Returns affected bookings.
func (s *Service) CancelSession(ctx context.Context, id int64, reason string) (int, error) {
	var affected []Booking
	var se schedule.Session
	err := db.WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		var err error
		if se, err = s.sched.Session(ctx, tx, id, 0, true); err != nil {
			return err
		}
		if se.Status == "cancelled" {
			return apperr.Conflict("already_cancelled", "La clase ya estaba cancelada.")
		}
		if err := s.sched.MarkCancelled(ctx, tx, id, reason); err != nil {
			return err
		}
		affected, err = s.list(ctx, tx, `WHERE b.session_id = $1 AND b.status IN ('confirmed', 'pending_payment')`, id)
		if err != nil {
			return err
		}
		for i := range affected {
			if err := s.cancel(ctx, tx, &affected[i], true); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	body := fmt.Sprintf("%s del %s a las %s fue cancelada.", se.Discipline, longDate(se.StartsAt, s.loc), se.Start)
	if r := strings.TrimSpace(reason); r != "" {
		body += " " + r
	}
	for _, b := range affected {
		s.notifyPerson(b, notify.Message{Title: "Clase cancelada", Body: body + " Si usaste tu plan, la clase te fue devuelta.", URL: "/reservar", Tag: b.Code})
	}
	return len(affected), nil
}

// ---- Notifications ----

func (s *Service) notifyPerson(b Booking, m notify.Message) {
	s.push.Notify(notify.Recipient{UserID: b.UserID, Email: b.Email}, m)
}

func (s *Service) notifyBooked(b Booking) {
	s.notifyPerson(b, notify.Message{Title: "Reserva confirmada",
		Body: fmt.Sprintf("%s · %s a las %s. Código %s.", b.Discipline, longDate(b.StartsAt, s.loc), b.Start, b.Code), URL: "/mis-reservas", Tag: b.Code})
}

func (s *Service) notifySession(sessionID int64, m notify.Message) {
	list, err := s.list(context.Background(), s.pool, `WHERE b.session_id = $1 AND b.status = 'confirmed'`, sessionID)
	if err != nil {
		s.log.Error("notificar clase", "err", err)
		return
	}
	for _, b := range list {
		s.notifyPerson(b, m)
	}
}

// SendReminders claims bookings starting within the reminder window and notifies them once.
func (s *Service) SendReminders(ctx context.Context) (int, error) {
	rows, err := s.pool.Query(ctx, `UPDATE bookings b SET reminder_sent_at = now()
		FROM class_sessions s WHERE s.id = b.session_id AND b.status = 'confirmed' AND b.reminder_sent_at IS NULL
		AND s.status = 'scheduled' AND s.starts_at > now() AND s.starts_at <= now() + $1 * interval '1 second'
		RETURNING b.id`, int(s.remindAt.Seconds()))
	if err != nil {
		return 0, err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		return 0, err
	}
	for _, id := range ids {
		b, err := s.Get(ctx, s.pool, id)
		if err != nil {
			continue
		}
		coach := ""
		if b.CoachName != nil {
			coach = " con " + *b.CoachName
		}
		s.notifyPerson(b, notify.Message{Title: b.Discipline + " a las " + b.Start,
			Body: fmt.Sprintf("Tu clase%s empieza pronto. Llega 10 minutos antes.", coach), URL: "/mis-reservas", Tag: "reminder-" + b.Code})
	}
	return len(ids), nil
}

var months = [...]string{"enero", "febrero", "marzo", "abril", "mayo", "junio", "julio", "agosto", "septiembre", "octubre", "noviembre", "diciembre"}
var weekdays = [...]string{"domingo", "lunes", "martes", "miércoles", "jueves", "viernes", "sábado"}

func longDate(t time.Time, loc *time.Location) string {
	t = t.In(loc)
	return fmt.Sprintf("%s %d de %s", weekdays[t.Weekday()], t.Day(), months[t.Month()-1])
}
