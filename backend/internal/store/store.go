// Package store veritabanı erişimini toplar. SQL burada, iş kuralları domain paketlerinde.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"pistkoordinat/internal/auth"
)

type Q interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

type Store struct {
	Pool *pgxpool.Pool
}

func New(pool *pgxpool.Pool) *Store { return &Store{Pool: pool} }

var ErrNotFound = errors.New("bulunamadı")

func notFound(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

// Audit eklemeli-yalnız denetim kaydı yazar. Aynı transaction içinde çağrılmalı.
func Audit(ctx context.Context, q Q, actor auth.Claims, action, entity, id string, oldV, newV any) error {
	var o, n []byte
	if oldV != nil {
		o, _ = json.Marshal(oldV)
	}
	if newV != nil {
		n, _ = json.Marshal(newV)
	}
	var actorID *int64
	if actor.Sub != 0 {
		actorID = &actor.Sub
	}
	_, err := q.Exec(ctx, `INSERT INTO audit_log(actor_id, actor_role, action, entity, entity_id, old_value, new_value)
		VALUES ($1,$2,$3,$4,$5,$6,$7)`, actorID, string(actor.Role), action, entity, id, o, n)
	return err
}

type AuditEntry struct {
	ID        int64           `json:"id"`
	At        time.Time       `json:"at"`
	ActorID   *int64          `json:"actor_id"`
	ActorRole *string         `json:"actor_role"`
	Action    string          `json:"action"`
	Entity    string          `json:"entity"`
	EntityID  string          `json:"entity_id"`
	OldValue  json.RawMessage `json:"old_value,omitempty"`
	NewValue  json.RawMessage `json:"new_value,omitempty"`
}

func (s *Store) ListAudit(ctx context.Context, entity string, before *int64, limit int) ([]AuditEntry, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT id, at, actor_id, actor_role, action, entity, entity_id, old_value, new_value
		  FROM audit_log
		 WHERE ($1 = '' OR entity = $1) AND ($2::bigint IS NULL OR id < $2)
		 ORDER BY id DESC LIMIT $3`, entity, before, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (AuditEntry, error) {
		var a AuditEntry
		err := r.Scan(&a.ID, &a.At, &a.ActorID, &a.ActorRole, &a.Action, &a.Entity, &a.EntityID, &a.OldValue, &a.NewValue)
		return a, err
	})
}

// ---------- kullanıcılar ve oturum ----------

type User struct {
	ID           int64     `json:"id"`
	Email        string    `json:"email"`
	FullName     string    `json:"full_name"`
	Role         auth.Role `json:"role"`
	AirlineCode  *string   `json:"airline_code,omitempty"`
	CrewID       *int      `json:"crew_id,omitempty"`
	PasswordHash string    `json:"-"`
	Active       bool      `json:"-"`
}

const userCols = `u.id, u.email, u.full_name, u.role::text, u.airline_code, c.id, u.password_hash, u.active`

func scanUser(row pgx.Row) (User, error) {
	var u User
	var role string
	err := row.Scan(&u.ID, &u.Email, &u.FullName, &role, &u.AirlineCode, &u.CrewID, &u.PasswordHash, &u.Active)
	u.Role = auth.Role(role)
	return u, notFound(err)
}

func (s *Store) UserByEmail(ctx context.Context, email string) (User, error) {
	return scanUser(s.Pool.QueryRow(ctx, `SELECT `+userCols+` FROM users u LEFT JOIN crews c ON c.lead_user_id = u.id
		WHERE lower(u.email) = lower($1)`, email))
}

func (s *Store) UserByID(ctx context.Context, id int64) (User, error) {
	return scanUser(s.Pool.QueryRow(ctx, `SELECT `+userCols+` FROM users u LEFT JOIN crews c ON c.lead_user_id = u.id
		WHERE u.id = $1`, id))
}

func (s *Store) SaveRefresh(ctx context.Context, userID int64, hash string, exp time.Time) error {
	_, err := s.Pool.Exec(ctx, `INSERT INTO refresh_tokens(token_hash, user_id, expires_at) VALUES ($1,$2,$3)`, hash, userID, exp)
	return err
}

// RotateRefresh eski token'ı tek kullanımlık olarak tüketir. Aynı token ikinci kez
// gelirse (çalınma şüphesi) kullanıcının tüm refresh token'ları iptal edilir.
func (s *Store) RotateRefresh(ctx context.Context, oldHash, newHash string, exp time.Time) (int64, error) {
	var userID int64
	reused := false
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		var revoked *time.Time
		var expires time.Time
		err := tx.QueryRow(ctx, `SELECT user_id, revoked_at, expires_at FROM refresh_tokens WHERE token_hash=$1 FOR UPDATE`, oldHash).
			Scan(&userID, &revoked, &expires)
		if err != nil {
			return notFound(err)
		}
		if revoked != nil {
			reused = true
			return nil
		}
		if time.Now().After(expires) {
			return ErrNotFound
		}
		if _, err := tx.Exec(ctx, `UPDATE refresh_tokens SET revoked_at = now() WHERE token_hash=$1`, oldHash); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO refresh_tokens(token_hash, user_id, expires_at) VALUES ($1,$2,$3)`, newHash, userID, exp)
		return err
	})
	if err == nil && reused {
		_, _ = s.Pool.Exec(ctx, `UPDATE refresh_tokens SET revoked_at = now() WHERE user_id=$1 AND revoked_at IS NULL`, userID)
		return 0, ErrTokenReused
	}
	return userID, err
}

var ErrTokenReused = errors.New("refresh token tekrar kullanıldı")

func (s *Store) RevokeRefresh(ctx context.Context, hash string) error {
	_, err := s.Pool.Exec(ctx, `UPDATE refresh_tokens SET revoked_at = now() WHERE token_hash=$1 AND revoked_at IS NULL`, hash)
	return err
}

// ---------- idempotans ----------

type IdemRecord struct {
	RequestHash string
	Status      *int
	Body        []byte
}

// ClaimIdempotency anahtarı "işleniyor" olarak ayırır. Daha önce varsa mevcut kaydı döner.
func (s *Store) ClaimIdempotency(ctx context.Context, userID int64, key, method, path, hash string) (*IdemRecord, error) {
	tag, err := s.Pool.Exec(ctx, `INSERT INTO idempotency_keys(user_id, key, method, path, request_hash)
		VALUES ($1,$2,$3,$4,$5) ON CONFLICT DO NOTHING`, userID, key, method, path, hash)
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() == 1 {
		return nil, nil
	}
	var r IdemRecord
	err = s.Pool.QueryRow(ctx, `SELECT request_hash, status_code, response_body FROM idempotency_keys WHERE user_id=$1 AND key=$2`,
		userID, key).Scan(&r.RequestHash, &r.Status, &r.Body)
	return &r, err
}

func (s *Store) CompleteIdempotency(ctx context.Context, userID int64, key string, status int, body []byte) error {
	_, err := s.Pool.Exec(ctx, `UPDATE idempotency_keys SET status_code=$3, response_body=$4 WHERE user_id=$1 AND key=$2`,
		userID, key, status, body)
	return err
}

// ReleaseIdempotency 5xx gibi tekrar denenebilir hatalarda anahtarı serbest bırakır.
func (s *Store) ReleaseIdempotency(ctx context.Context, userID int64, key string) error {
	_, err := s.Pool.Exec(ctx, `DELETE FROM idempotency_keys WHERE user_id=$1 AND key=$2 AND status_code IS NULL`, userID, key)
	return err
}
