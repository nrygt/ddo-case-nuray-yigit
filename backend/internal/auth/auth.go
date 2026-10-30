// Package auth parola özeti, JWT ve rol tabanlı yetki kontrolü.
package auth

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"

	"pistkoordinat/internal/httpx"
)

type Role string

const (
	OpsController Role = "ops_controller"
	GroundChief   Role = "ground_chief"
	CrewLead      Role = "crew_lead"
	AirlineRep    Role = "airline_rep"
	Management    Role = "management"
)

func (r Role) Valid() bool {
	switch r {
	case OpsController, GroundChief, CrewLead, AirlineRep, Management:
		return true
	}
	return false
}

const bcryptCost = 11

func HashPassword(pw string) (string, error) {
	if len(pw) < 8 || len(pw) > 72 {
		return "", errors.New("parola 8-72 karakter olmalı")
	}
	b, err := bcrypt.GenerateFromPassword([]byte(pw), bcryptCost)
	return string(b), err
}

func CheckPassword(hash, pw string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(pw)) == nil
}

// dummyHash: kullanıcı yoksa da bcrypt maliyeti ödensin, zamanlama farkı e-posta sızdırmasın.
var dummyHash, _ = bcrypt.GenerateFromPassword([]byte("zaman-esitleme-parolasi"), bcryptCost)

func BurnTime(pw string) { _ = bcrypt.CompareHashAndPassword(dummyHash, []byte(pw)) }

// Claims access token içeriği.
type Claims struct {
	Sub     int64  `json:"sub"`
	Role    Role   `json:"role"`
	Name    string `json:"name"`
	Airline string `json:"airline,omitempty"`
	CrewID  int    `json:"crew,omitempty"`
	Iat     int64  `json:"iat"`
	Exp     int64  `json:"exp"`
}

type Signer struct {
	secret []byte
	ttl    time.Duration
	now    func() time.Time
}

func NewSigner(secret []byte, ttl time.Duration) *Signer {
	return &Signer{secret: secret, ttl: ttl, now: time.Now}
}

var jwtHeader = base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))

func (s *Signer) Sign(c Claims) (string, time.Time, error) {
	now := s.now()
	exp := now.Add(s.ttl)
	c.Iat, c.Exp = now.Unix(), exp.Unix()
	payload, err := json.Marshal(c)
	if err != nil {
		return "", time.Time{}, err
	}
	unsigned := jwtHeader + "." + base64.RawURLEncoding.EncodeToString(payload)
	return unsigned + "." + s.mac(unsigned), exp, nil
}

func (s *Signer) mac(msg string) string {
	h := hmac.New(sha256.New, s.secret)
	h.Write([]byte(msg))
	return base64.RawURLEncoding.EncodeToString(h.Sum(nil))
}

var ErrBadToken = errors.New("geçersiz token")

func (s *Signer) Verify(tok string) (Claims, error) {
	var c Claims
	parts := strings.Split(tok, ".")
	if len(parts) != 3 || parts[0] != jwtHeader {
		return c, ErrBadToken
	}
	want := s.mac(parts[0] + "." + parts[1])
	if subtle.ConstantTimeCompare([]byte(want), []byte(parts[2])) != 1 {
		return c, ErrBadToken
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return c, ErrBadToken
	}
	if err := json.Unmarshal(raw, &c); err != nil {
		return c, ErrBadToken
	}
	if s.now().Unix() >= c.Exp || !c.Role.Valid() {
		return c, ErrBadToken
	}
	return c, nil
}

// NewRefreshToken rastgele bir refresh token ve veritabanında saklanacak özetini üretir.
func NewRefreshToken() (token, hash string) {
	var b [32]byte
	_, _ = rand.Read(b[:])
	token = base64.RawURLEncoding.EncodeToString(b[:])
	return token, HashRefresh(token)
}

func HashRefresh(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

type ctxKey struct{}

func FromContext(ctx context.Context) (Claims, bool) {
	c, ok := ctx.Value(ctxKey{}).(Claims)
	return c, ok
}

func WithClaims(ctx context.Context, c Claims) context.Context {
	return context.WithValue(ctx, ctxKey{}, c)
}

// Require kimliği doğrular ve rolü izinliler arasında değilse 403 döner.
// Her korumalı uç nokta bu sarmalayıcıdan geçer; rol listesi router'da açıkça yazılır.
func (s *Signer) Require(next http.Handler, roles ...Role) http.Handler {
	allowed := map[Role]bool{}
	for _, r := range roles {
		allowed[r] = true
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok := bearer(r)
		if tok == "" {
			httpx.WriteError(w, r, httpx.ErrUnauthorized)
			return
		}
		c, err := s.Verify(tok)
		if err != nil {
			httpx.WriteError(w, r, httpx.Errf(http.StatusUnauthorized, "invalid_token", "token geçersiz ya da süresi dolmuş"))
			return
		}
		if !allowed[c.Role] {
			httpx.WriteError(w, r, httpx.ErrForbidden)
			return
		}
		next.ServeHTTP(w, r.WithContext(WithClaims(r.Context(), c)))
	})
}

func bearer(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if t, ok := strings.CutPrefix(h, "Bearer "); ok {
		return strings.TrimSpace(t)
	}
	// EventSource başlık gönderemez; yalnızca SSE ucu sorgu parametresini kabul eder.
	if r.Header.Get("Accept") == "text/event-stream" {
		return r.URL.Query().Get("access_token")
	}
	return ""
}
