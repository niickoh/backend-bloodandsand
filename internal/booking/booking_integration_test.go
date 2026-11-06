package booking_test

import (
	"context"
	"io"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bloodandsand/backend/internal/app"
	"github.com/bloodandsand/backend/internal/booking"
	"github.com/bloodandsand/backend/internal/config"
	"github.com/bloodandsand/backend/internal/platform/apperr"
)

// newApp builds the whole app against TEST_DATABASE_URL (a throwaway database; it gets truncated).
func newApp(t *testing.T) *app.App {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("define TEST_DATABASE_URL para correr los tests de integración")
	}
	loc, _ := time.LoadLocation("America/Santiago")
	cfg := config.Config{
		DatabaseURL: url, JWTSecret: "test-secret-test-secret-test-secret!!", AccessTTL: time.Minute, RefreshTTL: time.Hour,
		Timezone: loc, PaymentProviders: []string{"fake"}, BookingHold: 15 * time.Minute, CancelLimit: 3 * time.Hour,
		ReminderBefore: 2 * time.Hour, GenerateDaysAhead: 7,
	}
	ctx := context.Background()
	a, err := app.New(ctx, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Close)
	if _, err := a.Pool.Exec(ctx, `TRUNCATE bookings, orders, memberships, class_sessions, class_templates, refresh_tokens, push_subscriptions, users RESTART IDENTITY CASCADE`); err != nil {
		t.Fatal(err)
	}
	return a
}

func sessionWithCapacity(t *testing.T, a *app.App, slug string, capacity int) int64 {
	t.Helper()
	var id int64
	err := a.Pool.QueryRow(context.Background(), `INSERT INTO class_sessions (discipline_id, starts_at, ends_at, capacity)
		SELECT id, now() + interval '2 days', now() + interval '2 days 1 hour', $2 FROM disciplines WHERE slug = $1 RETURNING id`, slug, capacity).Scan(&id)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// Many people booking the last seats at once must never overbook.
func TestConcurrentBookingsRespectCapacity(t *testing.T) {
	a := newApp(t)
	id := sessionWithCapacity(t, a, "evaluacion", 3) // free class: no payment involved

	var ok, full atomic.Int32
	var wg sync.WaitGroup
	for i := range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := a.Bookings.Book(context.Background(), nil, booking.BookInput{SessionID: id, Contact: booking.Contact{
				Name: "Persona Prueba", Email: "p" + string(rune('a'+i)) + "@correo.cl", Phone: "+56 9 1234 5678"}})
			if err == nil {
				ok.Add(1)
				return
			}
			if e, isApp := apperr.As(err); isApp && e.Code == "session_full" {
				full.Add(1)
				return
			}
			t.Errorf("error inesperado: %v", err)
		}()
	}
	wg.Wait()
	if ok.Load() != 3 || full.Load() != 9 {
		t.Fatalf("reservas OK=%d llenas=%d; se esperaba 3 y 9", ok.Load(), full.Load())
	}
}

func TestFirstClassFreeThenPaid(t *testing.T) {
	a := newApp(t)
	ctx := context.Background()
	s1 := sessionWithCapacity(t, a, "box", 10)
	s2 := sessionWithCapacity(t, a, "fuerza", 10)
	c := booking.Contact{Name: "Ana Pérez", Email: "ana@correo.cl", Phone: "+56 9 1111 1111"}

	q, err := a.Bookings.Quote(ctx, nil, s1, c.Email)
	if err != nil || q.Payment != booking.PayFree {
		t.Fatalf("primera clase: %+v %v", q, err)
	}
	if _, err := a.Bookings.Book(ctx, nil, booking.BookInput{SessionID: s1, Contact: c}); err != nil {
		t.Fatal(err)
	}
	q, err = a.Bookings.Quote(ctx, nil, s2, "ANA@correo.cl") // email match is case-insensitive
	if err != nil || q.Payment != booking.PayPaid || q.Amount != 8000 {
		t.Fatalf("segunda clase debía pagarse: %+v %v", q, err)
	}
	if _, err := a.Bookings.Book(ctx, nil, booking.BookInput{SessionID: s2, Contact: c}); err == nil {
		t.Fatal("Book debía rechazar una clase pagada")
	}
}

func TestPaymentHoldIsReleasedOnFailure(t *testing.T) {
	a := newApp(t)
	ctx := context.Background()
	id := sessionWithCapacity(t, a, "box", 1)
	// Use up the free first class so the next one requires payment.
	other := sessionWithCapacity(t, a, "fuerza", 5)
	c := booking.Contact{Name: "Luis Soto", Email: "luis@correo.cl", Phone: "+56 9 2222 2222"}
	if _, err := a.Bookings.Book(ctx, nil, booking.BookInput{SessionID: other, Contact: c}); err != nil {
		t.Fatal(err)
	}

	co, err := a.Payments.CheckoutClass(ctx, nil, paymentInput(id, c))
	if err != nil {
		t.Fatal(err)
	}
	// The only seat is held: a second buyer can't take it.
	_, err = a.Payments.CheckoutClass(ctx, nil, paymentInput(id, booking.Contact{Name: "Otra Persona", Email: "otra@correo.cl", Phone: "+56 9 3333 3333"}))
	if e, ok := apperr.As(err); !ok || e.Code != "session_full" {
		t.Fatalf("se esperaba session_full, llegó %v", err)
	}
	if err := a.Payments.Fail(ctx, co.Order.ID, "failed", nil); err != nil {
		t.Fatal(err)
	}
	se, err := a.Schedule.Session(ctx, a.Pool, id, 0, false)
	if err != nil || se.Taken != 0 {
		t.Fatalf("el cupo debía liberarse: taken=%d err=%v", se.Taken, err)
	}
}
