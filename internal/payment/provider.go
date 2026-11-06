package payment

import (
	"context"
	"errors"
)

// Provider is a payment gateway. Each one starts a payment and, optionally, refunds it.
// Confirmation arrives through the provider-specific HTTP callbacks in http.go.
type Provider interface {
	Name() string
	Label() string
	Start(ctx context.Context, o Order, cb Callbacks) (Action, error)
	Refund(ctx context.Context, o Order) error
}

// Callbacks are the absolute URLs a provider sends the buyer (or its server) back to.
type Callbacks struct {
	ReturnURL  string // browser comes back here
	WebhookURL string // server-to-server notification, if the provider supports it
}

// Action tells the frontend what to do next.
type Action struct {
	Kind   string            `json:"kind"` // inline | redirect | form_post
	URL    string            `json:"url,omitempty"`
	Fields map[string]string `json:"fields,omitempty"` // for form_post
	Ref    string            `json:"-"` // provider reference to store on the order (token, preference id)
}

var ErrRefundUnsupported = errors.New("el proveedor no permite reembolsos automáticos")

// fakeProvider approves or declines in-app; for development and demos only.
type fakeProvider struct{}

func (fakeProvider) Name() string  { return "fake" }
func (fakeProvider) Label() string { return "Tarjeta de prueba" }
func (fakeProvider) Start(context.Context, Order, Callbacks) (Action, error) {
	return Action{Kind: "inline"}, nil
}
func (fakeProvider) Refund(context.Context, Order) error { return nil }
