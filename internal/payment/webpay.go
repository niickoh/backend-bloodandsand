package payment

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// webpay implements Transbank Webpay Plus (REST API v1.2).
// Docs: https://www.transbankdevelopers.cl/referencia/webpay
type webpay struct {
	host, commerce, secret string
	client                 *http.Client
}

func newWebpay(env, commerce, secret string) *webpay {
	host := "https://webpay3gint.transbank.cl"
	if env == "production" {
		host = "https://webpay3g.transbank.cl"
	}
	return &webpay{host: host, commerce: commerce, secret: secret, client: &http.Client{Timeout: 20 * time.Second}}
}

func (w *webpay) Name() string  { return "webpay" }
func (w *webpay) Label() string { return "Webpay (débito y crédito)" }

func (w *webpay) do(ctx context.Context, method, path string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, w.host+"/rswebpaytransaction/api/webpay/v1.2"+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Tbk-Api-Key-Id", w.commerce)
	req.Header.Set("Tbk-Api-Key-Secret", w.secret)
	req.Header.Set("Content-Type", "application/json")
	resp, err := w.client.Do(req)
	if err != nil {
		return fmt.Errorf("webpay: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		return fmt.Errorf("webpay %s %s: %d %s", method, path, resp.StatusCode, raw)
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(raw, out)
}

func buyOrder(o Order) string { return fmt.Sprintf("BS%010d", o.ID) } // max 26 chars

func (w *webpay) Start(ctx context.Context, o Order, cb Callbacks) (Action, error) {
	var res struct {
		Token string `json:"token"`
		URL   string `json:"url"`
	}
	err := w.do(ctx, http.MethodPost, "/transactions", map[string]any{
		"buy_order":  buyOrder(o),
		"session_id": o.PublicToken[:20],
		"amount":     o.Amount,
		"return_url": cb.ReturnURL,
	}, &res)
	if err != nil {
		return Action{}, err
	}
	return Action{Kind: "form_post", URL: res.URL, Fields: map[string]string{"token_ws": res.Token}, Ref: res.Token}, nil
}

type webpayCommit struct {
	Status            string `json:"status"`
	ResponseCode      int    `json:"response_code"`
	Amount            int64  `json:"amount"`
	BuyOrder          string `json:"buy_order"`
	AuthorizationCode string `json:"authorization_code"`
	PaymentTypeCode   string `json:"payment_type_code"`
	CardDetail        struct {
		CardNumber string `json:"card_number"`
	} `json:"card_detail"`
	TransactionDate string `json:"transaction_date"`
}

func (w *webpay) Commit(ctx context.Context, token string) (webpayCommit, error) {
	var c webpayCommit
	err := w.do(ctx, http.MethodPut, "/transactions/"+token, nil, &c)
	return c, err
}

func (w *webpay) Status(ctx context.Context, token string) (webpayCommit, error) {
	var c webpayCommit
	err := w.do(ctx, http.MethodGet, "/transactions/"+token, nil, &c)
	return c, err
}

func (w *webpay) Refund(ctx context.Context, o Order) error {
	if o.ProviderRef == "" {
		return ErrRefundUnsupported
	}
	return w.do(ctx, http.MethodPost, "/transactions/"+o.ProviderRef+"/refunds", map[string]any{"amount": o.Amount}, nil)
}
