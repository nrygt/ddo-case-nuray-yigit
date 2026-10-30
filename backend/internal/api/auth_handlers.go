package api

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"pistkoordinat/internal/auth"
	"pistkoordinat/internal/httpx"
	"pistkoordinat/internal/store"
)

type tokenResponse struct {
	AccessToken  string     `json:"access_token"`
	RefreshToken string     `json:"refresh_token"`
	ExpiresAt    time.Time  `json:"expires_at"`
	User         store.User `json:"user"`
}

func (s *Server) issue(w http.ResponseWriter, r *http.Request, u store.User) error {
	c := auth.Claims{Sub: u.ID, Role: u.Role, Name: u.FullName}
	if u.AirlineCode != nil {
		c.Airline = *u.AirlineCode
	}
	if u.CrewID != nil {
		c.CrewID = *u.CrewID
	}
	access, exp, err := s.signer.Sign(c)
	if err != nil {
		return err
	}
	refresh, hash := auth.NewRefreshToken()
	if err := s.st.SaveRefresh(r.Context(), u.ID, hash, time.Now().Add(s.cfg.RefreshTTL)); err != nil {
		return err
	}
	httpx.JSON(w, http.StatusOK, tokenResponse{AccessToken: access, RefreshToken: refresh, ExpiresAt: exp, User: u})
	return nil
}

var errBadLogin = httpx.Errf(http.StatusUnauthorized, "invalid_credentials", "e-posta ya da parola hatalı")

func (s *Server) login(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if err := httpx.Decode(r, &in); err != nil {
		return err
	}
	in.Email = strings.TrimSpace(in.Email)
	if in.Email == "" || in.Password == "" {
		return httpx.Invalid("email", "e-posta ve parola zorunlu")
	}
	u, err := s.st.UserByEmail(r.Context(), in.Email)
	if errors.Is(err, store.ErrNotFound) {
		auth.BurnTime(in.Password)
		return errBadLogin
	}
	if err != nil {
		return err
	}
	if !auth.CheckPassword(u.PasswordHash, in.Password) || !u.Active {
		return errBadLogin
	}
	return s.issue(w, r, u)
}

func (s *Server) refresh(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		RefreshToken string `json:"refresh_token"`
	}
	if err := httpx.Decode(r, &in); err != nil {
		return err
	}
	newTok, newHash := auth.NewRefreshToken()
	uid, err := s.st.RotateRefresh(r.Context(), auth.HashRefresh(in.RefreshToken), newHash, time.Now().Add(s.cfg.RefreshTTL))
	if errors.Is(err, store.ErrTokenReused) {
		return httpx.Errf(http.StatusUnauthorized, "token_reused", "refresh token tekrar kullanıldı; tüm oturumlar kapatıldı")
	}
	if err != nil {
		return httpx.Errf(http.StatusUnauthorized, "invalid_token", "refresh token geçersiz")
	}
	u, err := s.st.UserByID(r.Context(), uid)
	if err != nil {
		return err
	}
	c := auth.Claims{Sub: u.ID, Role: u.Role, Name: u.FullName}
	if u.AirlineCode != nil {
		c.Airline = *u.AirlineCode
	}
	if u.CrewID != nil {
		c.CrewID = *u.CrewID
	}
	access, exp, err := s.signer.Sign(c)
	if err != nil {
		return err
	}
	httpx.JSON(w, http.StatusOK, tokenResponse{AccessToken: access, RefreshToken: newTok, ExpiresAt: exp, User: u})
	return nil
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		RefreshToken string `json:"refresh_token"`
	}
	if err := httpx.Decode(r, &in); err != nil {
		return err
	}
	if err := s.st.RevokeRefresh(r.Context(), auth.HashRefresh(in.RefreshToken)); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (s *Server) me(w http.ResponseWriter, r *http.Request) error {
	u, err := s.st.UserByID(r.Context(), claims(r).Sub)
	if err != nil {
		return dbErr(err)
	}
	httpx.JSON(w, http.StatusOK, u)
	return nil
}
