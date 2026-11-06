package httpx

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net"
	"net/http"
	"runtime/debug"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/bloodandsand/backend/internal/platform/apperr"
	"golang.org/x/time/rate"
)

type Middleware func(http.Handler) http.Handler

func Chain(h http.Handler, m ...Middleware) http.Handler {
	for i := len(m) - 1; i >= 0; i-- {
		h = m[i](h)
	}
	return h
}

type reqIDKey struct{}

func RequestID(ctx context.Context) string {
	id, _ := ctx.Value(reqIDKey{}).(string)
	return id
}

type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	n, err := s.ResponseWriter.Write(b)
	s.bytes += n
	return n, err
}

// Logger assigns a request id and logs one line per request.
func Logger(log *slog.Logger) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := r.Header.Get("X-Request-Id")
			if id == "" || len(id) > 64 {
				b := make([]byte, 8)
				_, _ = rand.Read(b)
				id = hex.EncodeToString(b)
			}
			w.Header().Set("X-Request-Id", id)
			ctx := context.WithValue(r.Context(), reqIDKey{}, id)

			start := time.Now()
			rec := &statusRecorder{ResponseWriter: w}
			next.ServeHTTP(rec, r.WithContext(ctx))

			level := slog.LevelInfo
			if rec.status >= 500 {
				level = slog.LevelError
			}
			log.LogAttrs(ctx, level, "http",
				slog.String("id", id),
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.Int("status", rec.status),
				slog.Int("bytes", rec.bytes),
				slog.Duration("took", time.Since(start)),
			)
		})
	}
}

func Recover(log *slog.Logger) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if p := recover(); p != nil {
					if p == http.ErrAbortHandler {
						panic(p)
					}
					log.ErrorContext(r.Context(), "panic", "value", p, "stack", string(debug.Stack()))
					_ = JSON(w, http.StatusInternalServerError, errorBody{Error: "internal", Message: "Algo falló de nuestro lado."})
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}

func SecureHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
		h.Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

// CORS allows credentialed requests from the configured origins only.
func CORS(origins []string) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")
			if origin != "" && slices.Contains(origins, origin) {
				h := w.Header()
				h.Set("Access-Control-Allow-Origin", origin)
				h.Set("Access-Control-Allow-Credentials", "true")
				h.Add("Vary", "Origin")
				if r.Method == http.MethodOptions {
					h.Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
					h.Set("Access-Control-Allow-Headers", "Content-Type, X-Requested-With")
					h.Set("Access-Control-Max-Age", "600")
					w.WriteHeader(http.StatusNoContent)
					return
				}
			}
			next.ServeHTTP(w, r)
		})
	}
}

// RequireJSONForWrites is a CSRF guard: browsers can't send application/json cross-site without a
// CORS preflight, which we only grant to our own origins. Form-encoded payment callbacks opt out by path.
func RequireJSONForWrites(exempt ...string) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.Method {
			case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
				for _, p := range exempt {
					if strings.HasPrefix(r.URL.Path, p) {
						next.ServeHTTP(w, r)
						return
					}
				}
				if r.ContentLength != 0 && !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
					_ = JSON(w, http.StatusUnsupportedMediaType, errorBody{Error: "bad_content_type", Message: "El cuerpo debe ser JSON."})
					return
				}
				if r.ContentLength == 0 && r.Header.Get("X-Requested-With") == "" && r.Header.Get("Content-Type") == "" {
					_ = JSON(w, http.StatusUnsupportedMediaType, errorBody{Error: "bad_content_type", Message: "Falta la cabecera Content-Type: application/json."})
					return
				}
			}
			next.ServeHTTP(w, r)
		})
	}
}

// RateLimiter is a per-IP token bucket, used on sensitive endpoints like login.
type RateLimiter struct {
	mu      sync.Mutex
	clients map[string]*client
	every   rate.Limit
	burst   int
}

type client struct {
	lim  *rate.Limiter
	seen time.Time
}

func NewRateLimiter(perMinute, burst int) *RateLimiter {
	rl := &RateLimiter{clients: map[string]*client{}, every: rate.Limit(float64(perMinute) / 60), burst: burst}
	go func() {
		for range time.Tick(5 * time.Minute) {
			rl.mu.Lock()
			for ip, c := range rl.clients {
				if time.Since(c.seen) > 10*time.Minute {
					delete(rl.clients, ip)
				}
			}
			rl.mu.Unlock()
		}
	}()
	return rl
}

func (rl *RateLimiter) Allow(ip string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	c, ok := rl.clients[ip]
	if !ok {
		c = &client{lim: rate.NewLimiter(rl.every, rl.burst)}
		rl.clients[ip] = c
	}
	c.seen = time.Now()
	return c.lim.Allow()
}

func (rl *RateLimiter) Wrap(log *slog.Logger, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !rl.Allow(ClientIP(r)) {
			WriteError(w, r, log, apperr.TooMany())
			return
		}
		h(w, r)
	}
}

// ClientIP prefers X-Real-IP set by nginx; falls back to the socket address.
func ClientIP(r *http.Request) string {
	if ip := r.Header.Get("X-Real-IP"); ip != "" {
		return ip
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
