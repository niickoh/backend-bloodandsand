package auth

import (
	"testing"
	"time"

	"github.com/bloodandsand/backend/internal/platform/actor"
	"github.com/bloodandsand/backend/internal/user"
	"github.com/golang-jwt/jwt/v5"
)

func TestAccessTokenRoundTrip(t *testing.T) {
	s := &Service{secret: []byte("0123456789abcdef0123456789abcdef"), accessTTL: time.Minute}
	tok, err := s.issueAccess(&user.User{ID: 42, Role: actor.RoleCoach, FullName: "Diego Muñoz", Email: "d@x.cl"})
	if err != nil {
		t.Fatal(err)
	}
	a, err := s.ParseAccess(tok)
	if err != nil {
		t.Fatal(err)
	}
	if a.ID != 42 || a.Role != actor.RoleCoach || a.Name != "Diego Muñoz" {
		t.Fatalf("actor inesperado: %+v", a)
	}
}

func TestAccessTokenRejections(t *testing.T) {
	s := &Service{secret: []byte("0123456789abcdef0123456789abcdef"), accessTTL: time.Minute}

	expired := &Service{secret: s.secret, accessTTL: -time.Minute}
	tok, _ := expired.issueAccess(&user.User{ID: 1, Role: actor.RoleAdmin})
	if _, err := s.ParseAccess(tok); err == nil {
		t.Error("aceptó un token vencido")
	}

	other := &Service{secret: []byte("another-secret-another-secret-!!"), accessTTL: time.Minute}
	tok, _ = other.issueAccess(&user.User{ID: 1, Role: actor.RoleAdmin})
	if _, err := s.ParseAccess(tok); err == nil {
		t.Error("aceptó un token firmado con otro secreto")
	}

	none := jwt.NewWithClaims(jwt.SigningMethodNone, jwt.MapClaims{"sub": "1", "role": "admin", "iss": issuer, "exp": time.Now().Add(time.Hour).Unix()})
	tok, _ = none.SignedString(jwt.UnsafeAllowNoneSignatureType)
	if _, err := s.ParseAccess(tok); err == nil {
		t.Error("aceptó alg=none")
	}
}
