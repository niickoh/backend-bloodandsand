// Package httpx has small helpers for JSON handlers on top of net/http.
package httpx

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/bloodandsand/backend/internal/platform/apperr"
)

const maxBody = 1 << 20 // 1 MiB

// HandlerFunc is an http handler that returns an error; Handle turns errors into JSON responses.
type HandlerFunc func(w http.ResponseWriter, r *http.Request) error

func Handle(log *slog.Logger, h HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := h(w, r); err != nil {
			WriteError(w, r, log, err)
		}
	}
}

func JSON(w http.ResponseWriter, status int, v any) error {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if v == nil {
		return nil
	}
	return json.NewEncoder(w).Encode(v)
}

func OK(w http.ResponseWriter, v any) error      { return JSON(w, http.StatusOK, v) }
func Created(w http.ResponseWriter, v any) error { return JSON(w, http.StatusCreated, v) }
func NoContent(w http.ResponseWriter) error {
	w.WriteHeader(http.StatusNoContent)
	return nil
}

type errorBody struct {
	Error   string            `json:"error"`
	Message string            `json:"message"`
	Fields  map[string]string `json:"fields,omitempty"`
}

func WriteError(w http.ResponseWriter, r *http.Request, log *slog.Logger, err error) {
	e, ok := apperr.As(err)
	if !ok {
		log.ErrorContext(r.Context(), "error interno", "err", err, "method", r.Method, "path", r.URL.Path)
		_ = JSON(w, http.StatusInternalServerError, errorBody{Error: "internal", Message: "Algo falló de nuestro lado. Intenta de nuevo en unos minutos."})
		return
	}
	status := map[apperr.Kind]int{
		apperr.KindInvalid:      http.StatusUnprocessableEntity,
		apperr.KindNotFound:     http.StatusNotFound,
		apperr.KindConflict:     http.StatusConflict,
		apperr.KindUnauthorized: http.StatusUnauthorized,
		apperr.KindForbidden:    http.StatusForbidden,
		apperr.KindTooMany:      http.StatusTooManyRequests,
	}[e.Kind]
	if status == 0 {
		log.ErrorContext(r.Context(), "error interno", "err", err)
		status = http.StatusInternalServerError
	}
	_ = JSON(w, status, errorBody{Error: e.Code, Message: e.Message, Fields: e.Fields})
}

// Decode reads a JSON body into dst, rejecting unknown fields, bodies over 1 MiB and non-JSON content types.
func Decode(w http.ResponseWriter, r *http.Request, dst any) error {
	if ct := r.Header.Get("Content-Type"); ct != "" {
		mt, _, _ := mime.ParseMediaType(ct)
		if mt != "application/json" {
			return apperr.Invalid("bad_content_type", "El cuerpo debe ser JSON.")
		}
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		var syntax *json.SyntaxError
		var typeErr *json.UnmarshalTypeError
		var tooBig *http.MaxBytesError
		switch {
		case errors.Is(err, io.EOF):
			return apperr.Invalid("empty_body", "Faltan los datos de la solicitud.")
		case errors.As(err, &syntax):
			return apperr.Invalid("bad_json", "El JSON no es válido.")
		case errors.As(err, &typeErr):
			return apperr.Fields(map[string]string{typeErr.Field: "Tipo de dato incorrecto."})
		case errors.As(err, &tooBig):
			return apperr.Invalid("body_too_large", "La solicitud es demasiado grande.")
		case strings.HasPrefix(err.Error(), "json: unknown field "):
			return apperr.Invalid("unknown_field", "Campo desconocido: "+strings.TrimPrefix(err.Error(), "json: unknown field "))
		default:
			return apperr.Invalid("bad_json", "El JSON no es válido.")
		}
	}
	if dec.More() {
		return apperr.Invalid("bad_json", "El cuerpo debe tener un solo objeto JSON.")
	}
	return nil
}

// PathID parses a numeric path value such as /users/{id}.
func PathID(r *http.Request, name string) (int64, error) {
	id, err := strconv.ParseInt(r.PathValue(name), 10, 64)
	if err != nil || id <= 0 {
		return 0, apperr.NotFound("El recurso")
	}
	return id, nil
}

func QueryInt(r *http.Request, name string, def int) int {
	v, err := strconv.Atoi(r.URL.Query().Get(name))
	if err != nil {
		return def
	}
	return v
}

func QueryInt64(r *http.Request, name string) int64 {
	v, _ := strconv.ParseInt(r.URL.Query().Get(name), 10, 64)
	return v
}

// QueryDate parses yyyy-mm-dd in loc. Missing values return def.
func QueryDate(r *http.Request, name string, loc *time.Location, def time.Time) (time.Time, error) {
	v := r.URL.Query().Get(name)
	if v == "" {
		return def, nil
	}
	t, err := time.ParseInLocation(time.DateOnly, v, loc)
	if err != nil {
		return time.Time{}, apperr.Invalid("bad_date", fmt.Sprintf("La fecha %q debe tener formato AAAA-MM-DD.", name))
	}
	return t, nil
}

// Page holds limit/offset pagination read from ?limit=&offset=.
type Page struct {
	Limit, Offset int
}

func PageFrom(r *http.Request) Page {
	p := Page{Limit: QueryInt(r, "limit", 50), Offset: QueryInt(r, "offset", 0)}
	if p.Limit <= 0 || p.Limit > 200 {
		p.Limit = 50
	}
	if p.Offset < 0 {
		p.Offset = 0
	}
	return p
}

type List[T any] struct {
	Items []T `json:"items"`
	Total int `json:"total"`
}
