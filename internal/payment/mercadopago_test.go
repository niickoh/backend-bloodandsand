package payment

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http/httptest"
	"testing"
)

func sign(secret, manifest string) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(manifest))
	return hex.EncodeToString(m.Sum(nil))
}

func TestMercadoPagoSignature(t *testing.T) {
	mp := newMercadoPago("token", "s3cret", true)
	tests := []struct {
		name   string
		header string
		reqID  string
		dataID string
		want   bool
	}{
		{"valid", "ts=1700000000,v1=" + sign("s3cret", "id:123;request-id:abc;ts:1700000000;"), "abc", "123", true},
		{"wrong secret", "ts=1700000000,v1=" + sign("other", "id:123;request-id:abc;ts:1700000000;"), "abc", "123", false},
		{"tampered id", "ts=1700000000,v1=" + sign("s3cret", "id:123;request-id:abc;ts:1700000000;"), "abc", "999", false},
		{"missing header", "", "abc", "123", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest("POST", "/api/payments/mercadopago/webhook", nil)
			if tt.header != "" {
				r.Header.Set("x-signature", tt.header)
			}
			r.Header.Set("x-request-id", tt.reqID)
			if got := mp.VerifySignature(r, tt.dataID); got != tt.want {
				t.Fatalf("VerifySignature = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestWebpayBuyOrderFitsLimit(t *testing.T) {
	if got := buyOrder(Order{ID: 9_999_999_999}); len(got) > 26 {
		t.Fatalf("buy_order %q supera 26 caracteres", got)
	}
}
