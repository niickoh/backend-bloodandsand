package booking_test

import (
	"github.com/bloodandsand/backend/internal/booking"
	"github.com/bloodandsand/backend/internal/payment"
)

func paymentInput(sessionID int64, c booking.Contact) payment.ClassCheckoutInput {
	return payment.ClassCheckoutInput{BookInput: booking.BookInput{SessionID: sessionID, Contact: c}, Provider: "fake"}
}
