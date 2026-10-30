// Package api HTTP uç noktalarını kurar. Her uç noktanın izinli rolleri routes()
// içinde tek yerde, açıkça yazılıdır.
package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"pistkoordinat/internal/auth"
	"pistkoordinat/internal/config"
	"pistkoordinat/internal/db"
	"pistkoordinat/internal/httpx"
	"pistkoordinat/internal/metrics"
	"pistkoordinat/internal/store"
	"pistkoordinat/internal/stream"
)

type Server struct {
	cfg     config.Config
	pool    *pgxpool.Pool
	st      *store.Store
	signer  *auth.Signer
	hub     *stream.Hub
	openapi []byte
}

func New(cfg config.Config, pool *pgxpool.Pool, openapi []byte) *Server {
	return &Server{
		cfg: cfg, pool: pool, st: store.New(pool),
		signer:  auth.NewSigner(cfg.JWTSecret, cfg.AccessTTL),
		hub:     stream.NewHub(4096, 1024),
		openapi: openapi,
	}
}

func (s *Server) Hub() *stream.Hub { return s.hub }

var (
	all     = []auth.Role{auth.OpsController, auth.GroundChief, auth.CrewLead, auth.AirlineRep, auth.Management}
	staff   = []auth.Role{auth.OpsController, auth.GroundChief, auth.Management}
	ops     = []auth.Role{auth.OpsController}
	chief   = []auth.Role{auth.GroundChief}
	mgmt    = []auth.Role{auth.Management}
	opsMgmt = []auth.Role{auth.OpsController, auth.Management}
	opsChf  = []auth.Role{auth.OpsController, auth.GroundChief}
	viewers = []auth.Role{auth.OpsController, auth.GroundChief, auth.Management, auth.AirlineRep}
	field   = []auth.Role{auth.CrewLead, auth.GroundChief}
)

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	h := func(pattern string, fn httpx.Handler, roles ...auth.Role) {
		if len(roles) == 0 {
			mux.Handle(pattern, fn)
			return
		}
		mux.Handle(pattern, s.signer.Require(fn, roles...))
	}
	// idem: durum değiştiren kritik POST'lar Idempotency-Key destekler
	idem := func(fn httpx.Handler) httpx.Handler { return s.idempotent(fn) }

	h("GET /health", s.health)
	h("GET /metrics", s.metricsHandler)
	h("GET /openapi.yaml", s.openAPI)

	h("POST /api/v1/auth/login", s.login)
	h("POST /api/v1/auth/refresh", s.refresh)
	h("POST /api/v1/auth/logout", s.logout)
	h("GET /api/v1/me", s.me, all...)

	// F-1 uçuşlar
	h("GET /api/v1/flights", s.listFlights, viewers...)
	h("GET /api/v1/flights/{id}", s.getFlight, viewers...)
	h("POST /api/v1/flights", idem(s.createFlight), ops...)
	h("POST /api/v1/schedule/build", idem(s.buildSchedule), ops...)

	// F-2 envanter
	h("GET /api/v1/stands", s.listStands, viewers...)
	h("PATCH /api/v1/stands/{id}", s.patchStand, ops...)
	h("GET /api/v1/gates", s.listGates, viewers...)
	h("GET /api/v1/aircraft-types", s.listTypes, viewers...)

	// F-3 / T-1 tahsis
	h("GET /api/v1/allocations", s.listAllocations, viewers...)
	h("POST /api/v1/allocations", idem(s.manualAllocate), ops...)
	h("POST /api/v1/allocations/run", idem(s.runAllocation), ops...)
	h("GET /api/v1/allocations/runs/latest", s.latestRun, staff...)
	h("GET /api/v1/gantt", s.gantt, viewers...)

	// F-5 / T-2 besleme, değişiklikler, yeniden planlama
	h("POST /api/v1/feed/events", s.feedEvents, ops...)
	h("GET /api/v1/replan/pending", s.pendingReplans, opsChf...)
	h("POST /api/v1/replan/preview", s.replanPreview, opsChf...)
	h("POST /api/v1/replan/apply", idem(s.replanApply), ops...)
	h("GET /api/v1/changes", s.changes, staff...)

	// F-4 / T-3 ekipler ve görevler
	h("GET /api/v1/crews", s.listCrews, staff...)
	h("POST /api/v1/crews/{id}/shifts", idem(s.addShift), chief...)
	h("GET /api/v1/equipment", s.listEquipment, staff...)
	h("GET /api/v1/tasks", s.listTasks, staff...)
	h("GET /api/v1/tasks/{id}/events", s.taskEvents, append(staff, auth.CrewLead)...)
	h("POST /api/v1/assignments/run", idem(s.runAssignment), chief...)
	h("GET /api/v1/risks", s.risks, opsChf...)

	// F-6 / T-4 pano, akış, mobil senkronizasyon
	h("GET /api/v1/dashboard", s.dashboard, staff...)
	h("GET /api/v1/stream", s.streamHandler, append(staff, auth.CrewLead)...)
	h("GET /api/v1/my/tasks", s.myTasks, auth.CrewLead)
	h("POST /api/v1/sync/task-events", s.syncTaskEvents, field...)
	h("POST /api/v1/notices", s.postNotice, opsChf...)

	// F-7 / T-5 raporlar
	h("GET /api/v1/reports/turnaround", s.turnaroundReport, opsMgmt...)
	h("POST /api/v1/reports/refresh", s.refreshReports, mgmt...)
	h("GET /api/v1/reports/data-quality", s.dataQuality, mgmt...)
	h("GET /api/v1/audit", s.audit, mgmt...)

	mux.Handle("/", httpx.Handler(func(w http.ResponseWriter, r *http.Request) error {
		return httpx.Errf(http.StatusNotFound, "not_found", "uç nokta yok: %s %s", r.Method, r.URL.Path)
	}))
	routed := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, pat := mux.Handler(r); pat != "" && pat != "/" {
			httpx.SetRoute(r.Context(), pat)
		}
		mux.ServeHTTP(w, r)
	})
	return httpx.Observe(httpx.Timeout(s.cfg.RequestTimeout, routed))
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) error {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	start := time.Now()
	var one int
	err := s.pool.QueryRow(ctx, `SELECT 1`).Scan(&one)
	body := map[string]any{
		"status":      "ok",
		"database":    "ok",
		"db_latency":  time.Since(start).String(),
		"sse_clients": s.hub.Clients(),
		"time":        time.Now().UTC(),
	}
	if err != nil {
		body["status"], body["database"] = "degraded", "unreachable"
		httpx.JSON(w, http.StatusServiceUnavailable, body)
		return nil
	}
	httpx.JSON(w, http.StatusOK, body)
	return nil
}

func (s *Server) metricsHandler(w http.ResponseWriter, r *http.Request) error {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	metrics.Default.Write(w)
	st := s.pool.Stat()
	w.Write([]byte("# HELP pk_db_pool_acquired_conns Kullanımdaki DB bağlantıları\n# TYPE pk_db_pool_acquired_conns gauge\n"))
	w.Write([]byte("pk_db_pool_acquired_conns " + strconv.Itoa(int(st.AcquiredConns())) + "\n"))
	return nil
}

func (s *Server) openAPI(w http.ResponseWriter, r *http.Request) error {
	w.Header().Set("Content-Type", "application/yaml")
	w.Write(s.openapi)
	return nil
}

// ---------- idempotans ----------

type captureWriter struct {
	http.ResponseWriter
	status int
	buf    bytes.Buffer
}

func (c *captureWriter) WriteHeader(code int) {
	c.status = code
	c.ResponseWriter.WriteHeader(code)
}

func (c *captureWriter) Write(b []byte) (int, error) {
	if c.status == 0 {
		c.status = http.StatusOK
	}
	c.buf.Write(b)
	return c.ResponseWriter.Write(b)
}

// idempotent aynı kullanıcı + Idempotency-Key ile gelen tekrar isteğe ilk yanıtı aynen döner.
// Aynı anahtar farklı gövdeyle gelirse 422, ilk istek hâlâ sürüyorsa 409 döner.
func (s *Server) idempotent(next httpx.Handler) httpx.Handler {
	return func(w http.ResponseWriter, r *http.Request) error {
		key := r.Header.Get("Idempotency-Key")
		if key == "" {
			return next(w, r)
		}
		if len(key) < 8 || len(key) > 128 {
			return httpx.Invalid("Idempotency-Key", "8-128 karakter olmalı")
		}
		claims, _ := auth.FromContext(r.Context())
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
		if err != nil {
			return httpx.Errf(http.StatusRequestEntityTooLarge, "too_large", "gövde çok büyük")
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		sum := sha256.Sum256(append([]byte(r.Method+" "+r.URL.RequestURI()+"\n"), body...))
		hash := hex.EncodeToString(sum[:])

		prev, err := s.st.ClaimIdempotency(r.Context(), claims.Sub, key, r.Method, r.URL.Path, hash)
		if err != nil {
			return err
		}
		if prev != nil {
			if prev.RequestHash != hash {
				return httpx.Errf(http.StatusUnprocessableEntity, "idempotency_key_reused", "bu anahtar farklı bir istekle kullanılmış")
			}
			if prev.Status == nil {
				return httpx.Errf(http.StatusConflict, "idempotency_in_progress", "aynı anahtarlı istek hâlâ işleniyor")
			}
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.Header().Set("Idempotent-Replayed", "true")
			w.WriteHeader(*prev.Status)
			w.Write(prev.Body)
			return nil
		}
		cw := &captureWriter{ResponseWriter: w}
		herr := next(cw, r)
		if herr != nil {
			httpx.WriteError(cw, r, herr)
		}
		ctx := context.WithoutCancel(r.Context())
		if cw.status >= 500 || cw.status == 0 {
			_ = s.st.ReleaseIdempotency(ctx, claims.Sub, key)
		} else {
			_ = s.st.CompleteIdempotency(ctx, claims.Sub, key, cw.status, cw.buf.Bytes())
		}
		return nil
	}
}

// ---------- ortak yardımcılar ----------

func (s *Server) opDate(r *http.Request) (string, error) {
	d := r.URL.Query().Get("date")
	if d == "" {
		return time.Now().In(s.cfg.TimeZone).Format("2006-01-02"), nil
	}
	if _, err := time.Parse("2006-01-02", d); err != nil {
		return "", httpx.Invalid("date", "YYYY-MM-DD olmalı")
	}
	return d, nil
}

func pathID(r *http.Request) (int64, error) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		return 0, httpx.Invalid("id", "pozitif tam sayı olmalı")
	}
	return id, nil
}

// dbErr veritabanı kısıt hatalarını anlamlı HTTP hatalarına çevirir.
func dbErr(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, store.ErrNotFound) {
		return httpx.ErrNotFound
	}
	switch db.ErrCode(err) {
	case db.CodeExclusion:
		return httpx.Errf(http.StatusConflict, "conflict", "zaman çakışması: %s", db.ErrMessage(err))
	case db.CodeUnique:
		return httpx.Errf(http.StatusConflict, "duplicate", "kayıt zaten var: %s", db.ErrMessage(err))
	case db.CodeCheck:
		return httpx.Errf(http.StatusUnprocessableEntity, "rule_violation", "%s", db.ErrMessage(err))
	case db.CodeForeignKey:
		return httpx.Errf(http.StatusUnprocessableEntity, "invalid_reference", "%s", db.ErrMessage(err))
	case db.CodeSerialize, db.CodeDeadlock:
		return httpx.Errf(http.StatusConflict, "concurrent_update", "eşzamanlı güncelleme, tekrar deneyin")
	}
	return err
}

func claims(r *http.Request) auth.Claims {
	c, _ := auth.FromContext(r.Context())
	return c
}
