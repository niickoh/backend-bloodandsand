package payment

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// mercadoPago implements Checkout Pro (preferences + payments API).
// Docs: https://www.mercadopago.cl/developers/es/docs/checkout-pro
type mercadoPago struct {
	token, webhookSecret string
	sandbox              bool
	client               *http.Client
}

func newMercadoPago(token, webhookSecret string, sandbox bool) *mercadoPago {
	return &mercadoPago{token: token, webhookSecret: webhookSecret, sandbox: sandbox, client: &http.Client{Timeout: 20 * time.Second}}
}

func (m *mercadoPago) Name() string  { return "mercadopago" }
func (m *mercadoPago) Label() string { return "Mercado Pago" }

func (m *mercadoPago) do(ctx context.Context, method, path string, body, out any, idem string) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, "https://api.mercadopago.com"+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+m.token)
	req.Header.Set("Content-Type", "application/json")
	if idem != "" {
		req.Header.Set("X-Idempotency-Key", idem)
	}
	resp, err := m.client.Do(req)
	if err != nil {
		return fmt.Errorf("mercadopago: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		return fmt.Errorf("mercadopago %s %s: %d %s", method, path, resp.StatusCode, raw)
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(raw, out)
}

func externalRef(o Order) string { return fmt.Sprintf("order-%d", o.ID) }

func (m *mercadoPago) Start(ctx context.Context, o Order, cb Callbacks) (Action, error) {
	body := map[string]any{
		"items": []map[string]any{{
			"id": fmt.Sprintf("%s-%d", o.Kind, o.ID), "title": o.Description, "quantity": 1, "currency_id": "CLP", "unit_price": o.Amount,
		}},
		"payer":              map[string]any{"email": o.BuyerEmail, "name": o.BuyerName},
		"external_reference": externalRef(o),
		"back_urls":          map[string]string{"success": cb.ReturnURL, "failure": cb.ReturnURL, "pending": cb.ReturnURL},
		"statement_descriptor": "BLOODANDSAND",
		"expires":            true,
		"expiration_date_to": time.Now().Add(30 * time.Minute).Format("2006-01-02T15:04:05.000-07:00"),
	}
	// Mercado Pago only accepts auto_return and webhooks on public HTTPS URLs.
	if strings.HasPrefix(cb.ReturnURL, "https://") {
		body["auto_return"] = "approved"
		body["notification_url"] = cb.WebhookURL
	}
	var res struct {
		ID               string `json:"id"`
		InitPoint        string `json:"init_point"`
		SandboxInitPoint string `json:"sandbox_init_point"`
	}
	if err := m.do(ctx, http.MethodPost, "/checkout/preferences", body, &res, ""); err != nil {
		return Action{}, err
	}
	url := res.InitPoint
	if m.sandbox && res.SandboxInitPoint != "" {
		url = res.SandboxInitPoint
	}
	return Action{Kind: "redirect", URL: url, Ref: res.ID}, nil
}

type mpPayment struct {
	ID                int64   `json:"id"`
	Status            string  `json:"status"` // approved | pending | in_process | rejected | cancelled | refunded
	StatusDetail      string  `json:"status_detail"`
	ExternalReference string  `json:"external_reference"`
	TransactionAmount float64 `json:"transaction_amount"`
	CurrencyID        string  `json:"currency_id"`
	PaymentMethodID   string  `json:"payment_method_id"`
}

func (m *mercadoPago) Payment(ctx context.Context, id string) (mpPayment, error) {
	var p mpPayment
	err := m.do(ctx, http.MethodGet, "/v1/payments/"+id, nil, &p, "")
	return p, err
}

func (m *mercadoPago) Refund(ctx context.Context, o Order) error {
	paymentID, _ := o.Detail["paymentId"].(string)
	if paymentID == "" {
		return ErrRefundUnsupported
	}
	return m.do(ctx, http.MethodPost, "/v1/payments/"+paymentID+"/refunds", map[string]any{}, nil, fmt.Sprintf("refund-order-%d", o.ID))
}

// VerifySignature checks the x-signature header of a webhook.
// Manifest: "id:<data.id>;request-id:<x-request-id>;ts:<ts>;" signed with HMAC-SHA256.
// Without a configured secret the check is skipped (payments are always re-fetched from the API anyway).
func (m *mercadoPago) VerifySignature(r *http.Request, dataID string) bool {
	if m.webhookSecret == "" {
		return true
	}
	var ts, v1 string
	for _, part := range strings.Split(r.Header.Get("x-signature"), ",") {
		k, v, _ := strings.Cut(strings.TrimSpace(part), "=")
		switch k {
		case "ts":
			ts = v
		case "v1":
			v1 = v
		}
	}
	if ts == "" || v1 == "" {
		return false
	}
	manifest := "id:" + strings.ToLower(dataID) + ";"
	if rid := r.Header.Get("x-request-id"); rid != "" {
		manifest += "request-id:" + rid + ";"
	}
	manifest += "ts:" + ts + ";"
	mac := hmac.New(sha256.New, []byte(m.webhookSecret))
	mac.Write([]byte(manifest))
	want := hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(want), []byte(v1))
}
