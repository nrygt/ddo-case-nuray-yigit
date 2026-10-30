package auth

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

var secret = []byte("test-secret-test-secret-test-secret!")

func TestSignVerifyRoundTrip(t *testing.T) {
	s := NewSigner(secret, time.Minute)
	tok, _, err := s.Sign(Claims{Sub: 7, Role: GroundChief, Name: "Murat"})
	if err != nil {
		t.Fatal(err)
	}
	c, err := s.Verify(tok)
	if err != nil || c.Sub != 7 || c.Role != GroundChief {
		t.Fatalf("%+v %v", c, err)
	}
}

func TestTamperedTokenRejected(t *testing.T) {
	s := NewSigner(secret, time.Minute)
	tok, _, _ := s.Sign(Claims{Sub: 7, Role: CrewLead})
	parts := strings.Split(tok, ".")
	// rolü yönetim olarak değiştirip imzayı aynı bırak
	other, _, _ := s.Sign(Claims{Sub: 7, Role: Management})
	forged := parts[0] + "." + strings.Split(other, ".")[1] + "." + parts[2]
	if _, err := s.Verify(forged); err == nil {
		t.Fatal("değiştirilmiş yük kabul edilmemeli")
	}
	if _, err := NewSigner([]byte("baska-bir-anahtar-baska-bir-anahtar"), time.Minute).Verify(tok); err == nil {
		t.Fatal("başka anahtarla imzalı token kabul edilmemeli")
	}
	if _, err := s.Verify("abc.def"); err == nil {
		t.Fatal("bozuk biçim reddedilmeli")
	}
}

func TestExpiredTokenRejected(t *testing.T) {
	s := NewSigner(secret, time.Minute)
	s.now = func() time.Time { return time.Now().Add(-2 * time.Minute) }
	tok, _, _ := s.Sign(Claims{Sub: 1, Role: OpsController})
	s.now = time.Now
	if _, err := s.Verify(tok); err == nil {
		t.Fatal("süresi dolmuş token reddedilmeli")
	}
}

func TestPasswordHash(t *testing.T) {
	h, err := HashPassword("Demo.2026!")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(h, "$2a$11$") {
		t.Fatalf("bcrypt maliyeti 11 bekleniyordu: %s", h[:7])
	}
	if !CheckPassword(h, "Demo.2026!") || CheckPassword(h, "demo.2026!") {
		t.Fatal("parola doğrulama hatalı")
	}
	if _, err := HashPassword("kisa"); err == nil {
		t.Fatal("kısa parola reddedilmeli")
	}
}

func TestRequireMiddleware(t *testing.T) {
	s := NewSigner(secret, time.Minute)
	h := s.Require(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, _ := FromContext(r.Context())
		w.Write([]byte(c.Role))
	}), OpsController)

	cases := []struct {
		name   string
		header string
		want   int
	}{
		{"yok", "", 401},
		{"bozuk", "Bearer xyz", 401},
		{"yanlış rol", "Bearer " + must(s.Sign(Claims{Sub: 1, Role: CrewLead})), 403},
		{"doğru rol", "Bearer " + must(s.Sign(Claims{Sub: 1, Role: OpsController})), 200},
	}
	for _, c := range cases {
		r := httptest.NewRequest("GET", "/", nil)
		if c.header != "" {
			r.Header.Set("Authorization", c.header)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != c.want {
			t.Errorf("%s: %d bekleniyordu, %d", c.name, c.want, w.Code)
		}
		if c.want >= 400 && !strings.Contains(w.Body.String(), `"error"`) {
			t.Errorf("%s: hata gövdesi standart biçimde değil: %s", c.name, w.Body)
		}
	}
}

func TestQueryTokenOnlyForEventStream(t *testing.T) {
	s := NewSigner(secret, time.Minute)
	tok := must(s.Sign(Claims{Sub: 1, Role: OpsController}))
	h := s.Require(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}), OpsController)
	r := httptest.NewRequest("GET", "/?access_token="+tok, nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 401 {
		t.Fatal("normal istekte sorgu parametresi token kabul edilmemeli")
	}
	r.Header.Set("Accept", "text/event-stream")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatal("SSE isteğinde sorgu parametresi kabul edilmeli")
	}
}

func TestRefreshTokenHash(t *testing.T) {
	a, ha := NewRefreshToken()
	b, hb := NewRefreshToken()
	if a == b || ha == hb || HashRefresh(a) != ha || len(ha) != 64 {
		t.Fatal("refresh token benzersiz ve özeti tutarlı olmalı")
	}
}

func must(s string, _ time.Time, err error) string {
	if err != nil {
		panic(err)
	}
	return s
}
