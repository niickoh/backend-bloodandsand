package payment

import (
	"context"
	"crypto/subtle"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/bloodandsand/backend/internal/platform/actor"
	"github.com/bloodandsand/backend/internal/platform/apperr"
	"github.com/bloodandsand/backend/internal/platform/httpx"
)

// CallbackPrefix is exempt from the JSON-only guard: providers post forms and webhooks here.
const CallbackPrefix = "/api/payments/"

func (s *Service) Routes(rt httpx.Router) {
	rt.Public("GET /api/payments/providers", func(w http.ResponseWriter, r *http.Request) error {
		return httpx.OK(w, s.Providers())
	})

	rt.Public("POST /api/checkout/class", func(w http.ResponseWriter, r *http.Request) error {
		var in ClassCheckoutInput
		if err := httpx.Decode(w, r, &in); err != nil {
			return err
		}
		c, err := s.CheckoutClass(r.Context(), actor.From(r.Context()), in)
		if err != nil {
			return err
		}
		return httpx.Created(w, c)
	})
	rt.Auth("POST /api/checkout/plan", func(w http.ResponseWriter, r *http.Request) error {
		var in PlanCheckoutInput
		if err := httpx.Decode(w, r, &in); err != nil {
			return err
		}
		c, err := s.CheckoutPlan(r.Context(), actor.From(r.Context()), in)
		if err != nil {
			return err
		}
		return httpx.Created(w, c)
	})

	// Order status for the result page: the buyer proves ownership with the token or their session.
	rt.Public("GET /api/orders/{id}", func(w http.ResponseWriter, r *http.Request) error {
		o, err := s.authorizedOrder(r)
		if err != nil {
			return err
		}
		o.Detail = nil
		return httpx.OK(w, o)
	})

	rt.Auth("GET /api/me/orders", func(w http.ResponseWriter, r *http.Request) error {
		list, _, err := s.List(r.Context(), ListFilter{UserID: actor.From(r.Context()).ID, Limit: 100})
		if err != nil {
			return err
		}
		for i := range list {
			list[i].Detail = nil
		}
		return httpx.OK(w, list)
	})

	// ---- Fake provider ----
	if _, ok := s.providers["fake"]; ok {
		rt.Public("POST /api/payments/fake/{id}/confirm", func(w http.ResponseWriter, r *http.Request) error {
			var in struct {
				Token      string `json:"token"`
				CardNumber string `json:"cardNumber"`
			}
			if err := httpx.Decode(w, r, &in); err != nil {
				return err
			}
			o, err := s.orderWithToken(r.Context(), r, in.Token)
			if err != nil {
				return err
			}
			if o.Provider != "fake" || o.Status != "pending" {
				return apperr.Conflict("bad_state", "Esta orden ya no se puede pagar.")
			}
			digits := strings.Map(func(c rune) rune {
				if c >= '0' && c <= '9' {
					return c
				}
				return -1
			}, in.CardNumber)
			time.Sleep(700 * time.Millisecond) // feels like a real gateway
			if strings.HasSuffix(digits, "0002") {
				_ = s.Fail(r.Context(), o.ID, "failed", map[string]any{"reason": "declined", "last4": last4(digits)})
				return apperr.Conflict("payment_declined", "Tu banco rechazó el pago. Prueba con otra tarjeta u otro medio de pago.")
			}
			o, err = s.MarkPaid(r.Context(), o.ID, o.Amount, map[string]any{"last4": last4(digits)})
			if err != nil {
				return err
			}
			o.Detail = nil
			return httpx.OK(w, o)
		})
	}

	// ---- Webpay ----
	if p, ok := s.providers["webpay"].(*webpay); ok {
		h := func(w http.ResponseWriter, r *http.Request) error { return s.webpayReturn(w, r, p) }
		rt.Public("GET /api/payments/webpay/return", h)
		rt.Public("POST /api/payments/webpay/return", h)
	}

	// ---- Mercado Pago ----
	if p, ok := s.providers["mercadopago"].(*mercadoPago); ok {
		rt.Public("GET /api/payments/mercadopago/return", func(w http.ResponseWriter, r *http.Request) error {
			id, _ := strconv.ParseInt(r.URL.Query().Get("order"), 10, 64)
			if pid := r.URL.Query().Get("payment_id"); pid != "" && pid != "null" {
				s.syncMercadoPago(r.Context(), p, pid)
			} else if st := r.URL.Query().Get("status"); st == "null" || st == "rejected" {
				_ = s.Fail(r.Context(), id, "cancelled", map[string]any{"reason": "buyer_returned"})
			}
			return s.redirectToResult(w, r, id)
		})
		rt.Public("POST /api/payments/mercadopago/webhook", func(w http.ResponseWriter, r *http.Request) error {
			var body struct {
				Type string `json:"type"`
				Data struct {
					ID any `json:"id"`
				} `json:"data"`
			}
			_ = httpx.Decode(w, r, &body)
			dataID := r.URL.Query().Get("data.id")
			if dataID == "" && body.Data.ID != nil {
				dataID = fmt.Sprint(body.Data.ID)
			}
			kind := body.Type
			if kind == "" {
				kind = r.URL.Query().Get("type")
			}
			if kind != "payment" || dataID == "" {
				return httpx.NoContent(w)
			}
			if !p.VerifySignature(r, dataID) {
				return apperr.Unauthorized("Firma inválida.")
			}
			// Process in the background; Mercado Pago only needs a fast 2xx.
			go s.syncMercadoPago(context.WithoutCancel(r.Context()), p, dataID)
			return httpx.NoContent(w)
		})
	}

	// ---- Admin ----
	rt.Admin("GET /api/admin/orders", func(w http.ResponseWriter, r *http.Request) error {
		pg := httpx.PageFrom(r)
		q := r.URL.Query()
		list, total, err := s.List(r.Context(), ListFilter{Status: q.Get("status"), Kind: q.Get("kind"), Q: q.Get("q"),
			UserID: httpx.QueryInt64(r, "user"), Limit: pg.Limit, Offset: pg.Offset})
		if err != nil {
			return err
		}
		return httpx.OK(w, httpx.List[Order]{Items: list, Total: total})
	})
	rt.Admin("GET /api/admin/orders/{id}", func(w http.ResponseWriter, r *http.Request) error {
		id, err := httpx.PathID(r, "id")
		if err != nil {
			return err
		}
		o, err := s.Get(r.Context(), s.pool, id)
		if err != nil {
			return err
		}
		return httpx.OK(w, o)
	})
	rt.Admin("POST /api/admin/orders/{id}/refund", func(w http.ResponseWriter, r *http.Request) error {
		id, err := httpx.PathID(r, "id")
		if err != nil {
			return err
		}
		var in struct {
			Note string `json:"note"`
		}
		if err := httpx.Decode(w, r, &in); err != nil {
			return err
		}
		o, err := s.Refund(r.Context(), id, strings.TrimSpace(in.Note))
		if err != nil {
			return err
		}
		return httpx.OK(w, o)
	})
}

func last4(d string) string {
	if len(d) < 4 {
		return d
	}
	return d[len(d)-4:]
}

func (s *Service) orderWithToken(ctx context.Context, r *http.Request, token string) (Order, error) {
	id, err := httpx.PathID(r, "id")
	if err != nil {
		return Order{}, err
	}
	o, err := s.Get(ctx, s.pool, id)
	if err != nil {
		return o, err
	}
	if token == "" || subtle.ConstantTimeCompare([]byte(token), []byte(o.PublicToken)) != 1 {
		return Order{}, apperr.NotFound("La orden")
	}
	return o, nil
}

func (s *Service) authorizedOrder(r *http.Request) (Order, error) {
	a := actor.From(r.Context())
	if t := r.URL.Query().Get("token"); t != "" {
		return s.orderWithToken(r.Context(), r, t)
	}
	id, err := httpx.PathID(r, "id")
	if err != nil {
		return Order{}, err
	}
	o, err := s.Get(r.Context(), s.pool, id)
	if err != nil {
		return o, err
	}
	if a.IsAdmin() || (a != nil && o.UserID != nil && *o.UserID == a.ID) {
		return o, nil
	}
	return Order{}, apperr.NotFound("La orden")
}

func (s *Service) redirectToResult(w http.ResponseWriter, r *http.Request, orderID int64) error {
	target := s.frontURL + "/pago/resultado"
	if o, err := s.Get(r.Context(), s.pool, orderID); err == nil {
		target += fmt.Sprintf("?order=%d&t=%s", o.ID, o.PublicToken)
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
	return nil
}

// webpayReturn handles every way Webpay brings the buyer back:
// token_ws = finished (commit it); TBK_TOKEN = buyer aborted; only TBK_ORDEN_COMPRA = timeout.
func (s *Service) webpayReturn(w http.ResponseWriter, r *http.Request, p *webpay) error {
	_ = r.ParseForm()
	id, _ := strconv.ParseInt(r.URL.Query().Get("order"), 10, 64)
	ctx := r.Context()
	o, err := s.Get(ctx, s.pool, id)
	if err != nil {
		http.Redirect(w, r, s.frontURL+"/pago/resultado?error=not_found", http.StatusSeeOther)
		return nil
	}
	token := r.Form.Get("token_ws")
	switch {
	case token != "" && r.Form.Get("TBK_TOKEN") == "":
		if token != o.ProviderRef {
			s.log.Warn("webpay: token no coincide con la orden", "order", id)
			break
		}
		c, err := p.Commit(ctx, token)
		if err != nil {
			// The commit may have succeeded before a network error; ask for the status.
			if c, err = p.Status(ctx, token); err != nil {
				s.log.Error("webpay: commit", "order", id, "err", err)
				break
			}
		}
		detail := map[string]any{"authorizationCode": c.AuthorizationCode, "paymentType": c.PaymentTypeCode,
			"last4": c.CardDetail.CardNumber, "responseCode": c.ResponseCode, "transactionDate": c.TransactionDate}
		if c.Status == "AUTHORIZED" && c.ResponseCode == 0 && c.BuyOrder == buyOrder(o) {
			if _, err := s.MarkPaid(ctx, id, c.Amount, detail); err != nil {
				s.log.Error("webpay: marcar pagada", "order", id, "err", err)
			}
		} else {
			_ = s.Fail(ctx, id, "failed", detail)
		}
	case r.Form.Get("TBK_TOKEN") != "":
		_ = s.Fail(ctx, id, "cancelled", map[string]any{"reason": "buyer_aborted"})
	default:
		_ = s.Fail(ctx, id, "expired", map[string]any{"reason": "webpay_timeout"})
	}
	return s.redirectToResult(w, r, id)
}

// syncMercadoPago fetches the payment from the API (never trusting query params) and updates the order.
func (s *Service) syncMercadoPago(ctx context.Context, p *mercadoPago, paymentID string) {
	pay, err := p.Payment(ctx, paymentID)
	if err != nil {
		s.log.Error("mercadopago: consultar pago", "payment", paymentID, "err", err)
		return
	}
	id, err := strconv.ParseInt(strings.TrimPrefix(pay.ExternalReference, "order-"), 10, 64)
	if err != nil {
		s.log.Warn("mercadopago: referencia desconocida", "ref", pay.ExternalReference)
		return
	}
	detail := map[string]any{"paymentId": strconv.FormatInt(pay.ID, 10), "mpStatus": pay.Status, "mpStatusDetail": pay.StatusDetail, "method": pay.PaymentMethodID}
	switch pay.Status {
	case "approved":
		if _, err := s.MarkPaid(ctx, id, int64(pay.TransactionAmount), detail); err != nil {
			s.log.Error("mercadopago: marcar pagada", "order", id, "err", err)
		}
	case "rejected", "cancelled":
		_ = s.Fail(ctx, id, "failed", detail)
	}
}
