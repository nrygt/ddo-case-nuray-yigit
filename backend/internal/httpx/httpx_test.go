package httpx

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestCursorRoundTrip(t *testing.T) {
	c := Cursor{At: time.Date(2026, 10, 20, 9, 30, 0, 123000, time.UTC), ID: 4242}
	got, err := DecodeCursor(c.Encode())
	if err != nil || !got.At.Equal(c.At) || got.ID != c.ID {
		t.Fatalf("%+v %v", got, err)
	}
	for _, bad := range []string{"!!", "bm90LWEtY3Vyc29y", "MTox"} {
		if _, err := DecodeCursor(bad); err == nil && bad != "MTox" {
			t.Errorf("%q reddedilmeli", bad)
		}
	}
}

func TestPageParams(t *testing.T) {
	r := httptest.NewRequest("GET", "/?limit=0", nil)
	if _, _, err := PageParams(r, 50, 500); err == nil {
		t.Fatal("limit=0 reddedilmeli")
	}
	r = httptest.NewRequest("GET", "/?limit=501", nil)
	if _, _, err := PageParams(r, 50, 500); err == nil {
		t.Fatal("üst sınır aşılmamalı")
	}
	r = httptest.NewRequest("GET", "/", nil)
	if n, c, err := PageParams(r, 50, 500); n != 50 || c != nil || err != nil {
		t.Fatal("varsayılan limit")
	}
}

func TestErrorFormat(t *testing.T) {
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/", nil)
	WriteError(w, r, Invalid("date", "YYYY-MM-DD olmalı"))
	var body struct {
		Error struct{ Code, Message string } `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || w.Code != 422 || body.Error.Code != "validation_failed" {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	w = httptest.NewRecorder()
	WriteError(w, r, errors.New("gizli iç ayrıntı"))
	if w.Code != 500 || strings.Contains(w.Body.String(), "gizli") {
		t.Fatalf("iç hata ayrıntısı sızmamalı: %s", w.Body)
	}
	w = httptest.NewRecorder()
	WriteError(w, r, context.DeadlineExceeded)
	if w.Code != 504 {
		t.Fatalf("zaman aşımı 504 olmalı: %d", w.Code)
	}
}

func TestDecodeRejectsUnknownFields(t *testing.T) {
	r := httptest.NewRequest("POST", "/", strings.NewReader(`{"a":1,"rol":"admin"}`))
	var v struct {
		A int `json:"a"`
	}
	if err := Decode(r, &v); err == nil {
		t.Fatal("bilinmeyen alan reddedilmeli")
	}
}

func TestObserveRecoversPanicAndSetsRequestID(t *testing.T) {
	h := Observe(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if RequestID(r.Context()) == "" {
			t.Error("istek kimliği yok")
		}
		SetRoute(r.Context(), "GET /x")
		panic("boom")
	}))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/x", nil))
	if w.Code != 500 || w.Header().Get("X-Request-ID") == "" {
		t.Fatalf("panik 500'e dönmeli: %d", w.Code)
	}
}

func TestTimeoutSetsDeadlineExceptSSE(t *testing.T) {
	var had bool
	h := Timeout(time.Second, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, had = r.Context().Deadline()
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))
	if !had {
		t.Fatal("normal istek süre sınırı almalı")
	}
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("Accept", "text/event-stream")
	h.ServeHTTP(httptest.NewRecorder(), r)
	if had {
		t.Fatal("SSE süre sınırı almamalı")
	}
}
