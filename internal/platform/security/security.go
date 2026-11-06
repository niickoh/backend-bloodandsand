// Package security has password hashing and random tokens.
package security

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strings"

	"golang.org/x/crypto/bcrypt"
)

const bcryptCost = 12

func HashPassword(plain string) (string, error) {
	b, err := bcrypt.GenerateFromPassword([]byte(plain), bcryptCost)
	return string(b), err
}

// CheckPassword compares in constant time. A malformed hash counts as a mismatch.
func CheckPassword(hash, plain string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(plain)) == nil
}

// dummyHash lets login spend the same time whether or not the email exists.
var dummyHash, _ = HashPassword("timing-equalizer-not-a-real-password")

func BurnPasswordCheck(plain string) { _ = CheckPassword(dummyHash, plain) }

// ValidatePassword returns a user-facing message, or "" if the password is acceptable.
func ValidatePassword(p string) string {
	switch {
	case len(p) < 8:
		return "Usa al menos 8 caracteres."
	case len(p) > 72:
		return "Usa como máximo 72 caracteres."
	case strings.TrimSpace(p) == "":
		return "La contraseña no puede estar en blanco."
	}
	return ""
}

// RandomToken returns n random bytes encoded as URL-safe base64 (no padding).
func RandomToken(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(errors.New("crypto/rand no disponible: " + err.Error()))
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// HashToken stores refresh tokens as SHA-256 so a database leak doesn't leak sessions.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

const codeAlphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789" // no 0/O/1/I

// ShortCode returns a human-friendly code like "K7QH2".
func ShortCode(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	for i := range b {
		b[i] = codeAlphabet[int(b[i])%len(codeAlphabet)]
	}
	return string(b)
}
