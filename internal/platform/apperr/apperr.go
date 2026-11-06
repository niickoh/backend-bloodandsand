// Package apperr defines domain errors that map to HTTP responses with a user-facing message.
package apperr

import (
	"errors"
	"fmt"
)

type Kind int

const (
	KindInternal Kind = iota
	KindInvalid
	KindNotFound
	KindConflict
	KindUnauthorized
	KindForbidden
	KindTooMany
)

// Error carries a message safe to show to the user, plus optional per-field messages.
type Error struct {
	Kind    Kind
	Code    string // stable machine code, e.g. "session_full"
	Message string
	Fields  map[string]string
	Err     error // wrapped cause, never shown to the user
}

func (e *Error) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("%s: %v", e.Message, e.Err)
	}
	return e.Message
}

func (e *Error) Unwrap() error { return e.Err }

func Invalid(code, msg string) *Error { return &Error{Kind: KindInvalid, Code: code, Message: msg} }

func Fields(fields map[string]string) *Error {
	return &Error{Kind: KindInvalid, Code: "invalid_fields", Message: "Revisa los datos marcados.", Fields: fields}
}

func NotFound(what string) *Error {
	return &Error{Kind: KindNotFound, Code: "not_found", Message: what + " no existe."}
}

func Conflict(code, msg string) *Error { return &Error{Kind: KindConflict, Code: code, Message: msg} }

func Unauthorized(msg string) *Error {
	return &Error{Kind: KindUnauthorized, Code: "unauthorized", Message: msg}
}

func Forbidden() *Error {
	return &Error{Kind: KindForbidden, Code: "forbidden", Message: "No tienes permiso para esta acción."}
}

func TooMany() *Error {
	return &Error{Kind: KindTooMany, Code: "rate_limited", Message: "Demasiados intentos. Espera un minuto y vuelve a intentar."}
}

// As extracts an *Error, if any.
func As(err error) (*Error, bool) {
	var e *Error
	ok := errors.As(err, &e)
	return e, ok
}

// FieldErrors collects validation messages.
type FieldErrors map[string]string

func (f FieldErrors) Add(field, msg string) {
	if _, exists := f[field]; !exists {
		f[field] = msg
	}
}

func (f FieldErrors) Check(ok bool, field, msg string) {
	if !ok {
		f.Add(field, msg)
	}
}

// Err returns nil when there are no messages.
func (f FieldErrors) Err() error {
	if len(f) == 0 {
		return nil
	}
	return Fields(f)
}
