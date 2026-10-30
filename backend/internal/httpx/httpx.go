// Package httpx ortak HTTP yardımcıları: hata biçimi, JSON, ara katmanlar.
package httpx

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strconv"
	"time"

	"pistkoordinat/internal/metrics"
)

// Error API'nin döndürdüğü tek hata tipi.
type Error struct {
	Status  int            `json:"-"`
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details,omitempty"`
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

func Errf(status int, code, format string, args ...any) *Error {
	return &Error{Status: status, Code: code, Message: fmt.Sprintf(format, args...)}
}

var (
	ErrUnauthorized = &Error{Status: http.StatusUnauthorized, Code: "unauthorized", Message: "kimlik doğrulama gerekli"}
	ErrForbidden    = &Error{Status: http.StatusForbidden, Code: "forbidden", Message: "bu işlem için yetkiniz yok"}
	ErrNotFound     = &Error{Status: http.StatusNotFound, Code: "not_found", Message: "kayıt bulunamadı"}
)

func Invalid(field, msg string) *Error {
	return &Error{Status: http.StatusUnprocessableEntity, Code: "validation_failed", Message: field + ": " + msg,
		Details: map[string]any{"field": field}}
}

func JSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if v != nil {
		_ = json.NewEncoder(w).Encode(v)
	}
}

func WriteError(w http.ResponseWriter, r *http.Request, err error) {
	var e *Error
	if !errors.As(err, &e) {
		if errors.Is(err, context.DeadlineExceeded) {
			e = Errf(http.StatusGatewayTimeout, "timeout", "istek zaman aşımına uğradı")
		} else {
			slog.ErrorContext(r.Context(), "beklenmeyen hata", "err", err, "path", r.URL.Path)
			e = Errf(http.StatusInternalServerError, "internal", "beklenmeyen bir hata oluştu")
		}
	}
	JSON(w, e.Status, map[string]any{"error": e})
}

// Decode gövdeyi katı biçimde çözer: bilinmeyen alan ve 1 MB üstü reddedilir.
func Decode(r *http.Request, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return &Error{Status: http.StatusBadRequest, Code: "bad_json", Message: "geçersiz JSON: " + err.Error()}
	}
	return nil
}

// Handler hata döndürebilen handler.
type Handler func(w http.ResponseWriter, r *http.Request) error

func (h Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if err := h(w, r); err != nil {
		WriteError(w, r, err)
	}
}

type ctxKey int

const (
	requestIDKey ctxKey = 1
	routeKey     ctxKey = 2
)

// SetRoute router'ın eşleştirdiği deseni loglama/metrik için kaydeder.
func SetRoute(ctx context.Context, pattern string) {
	if p, ok := ctx.Value(routeKey).(*string); ok {
		*p = pattern
	}
}

func RequestID(ctx context.Context) string {
	s, _ := ctx.Value(requestIDKey).(string)
	return s
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

func (s *statusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }

// Observe istek kimliği, panik yakalama, JSON log ve metrikleri tek ara katmanda toplar.
func Observe(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rid := r.Header.Get("X-Request-ID")
		if rid == "" || len(rid) > 64 {
			var b [8]byte
			_, _ = rand.Read(b[:])
			rid = hex.EncodeToString(b[:])
		}
		w.Header().Set("X-Request-ID", rid)
		route := new(string)
		ctx := context.WithValue(context.WithValue(r.Context(), requestIDKey, rid), routeKey, route)
		rec := &statusRecorder{ResponseWriter: w}

		defer func() {
			if p := recover(); p != nil {
				slog.Error("panik", "panic", p, "stack", string(debug.Stack()), "request_id", rid)
				if rec.status == 0 {
					WriteError(rec, r, errors.New("panic"))
				}
			}
			if *route == "" {
				*route = "unmatched"
			}
			if rec.status == 0 {
				rec.status = http.StatusOK
			}
			d := time.Since(start)
			metrics.HTTPRequests.Inc(r.Method, *route, strconv.Itoa(rec.status))
			metrics.HTTPDuration.Observe(d.Seconds(), r.Method, *route)
			// gövde/başlık loglanmaz: parola ve token sızmasın
			slog.Info("http",
				"request_id", rid, "method", r.Method, "route", *route,
				"status", rec.status, "duration_ms", d.Milliseconds(), "bytes", rec.bytes)
		}()
		next.ServeHTTP(rec, r.WithContext(ctx))
	})
}

// Timeout istek bağlamına üst süre koyar (SSE hariç).
func Timeout(d time.Duration, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Accept") == "text/event-stream" {
			next.ServeHTTP(w, r)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), d)
		defer cancel()
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}
