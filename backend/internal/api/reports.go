package api

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"pistkoordinat/internal/httpx"
)

func errorsIs(err, target error) bool { return errors.Is(err, target) }

func (s *Server) dashboard(w http.ResponseWriter, r *http.Request) error {
	d, err := s.opDate(r)
	if err != nil {
		return err
	}
	dash, err := s.st.Dashboard(r.Context(), d)
	if err != nil {
		return err
	}
	httpx.JSON(w, http.StatusOK, dash)
	return nil
}

func (s *Server) turnaroundReport(w http.ResponseWriter, r *http.Request) error {
	q := r.URL.Query()
	to := q.Get("to")
	from := q.Get("from")
	if to == "" {
		to = time.Now().In(s.cfg.TimeZone).Format("2006-01-02")
	}
	if from == "" {
		from = "2000-01-01"
	}
	for k, v := range map[string]string{"from": from, "to": to} {
		if _, err := time.Parse("2006-01-02", v); err != nil {
			return httpx.Invalid(k, "YYYY-MM-DD olmalı")
		}
	}
	group := q.Get("group_by")
	if group == "" {
		group = "type"
	}
	if group != "type" && group != "airline" && group != "stand" {
		return httpx.Invalid("group_by", "type|airline|stand")
	}
	rep, err := s.st.TurnaroundReport(r.Context(), from, to, group)
	if err != nil {
		return err
	}
	httpx.JSON(w, http.StatusOK, rep)
	return nil
}

func (s *Server) refreshReports(w http.ResponseWriter, r *http.Request) error {
	d, err := s.st.RefreshAnalytics(r.Context())
	if err != nil {
		return err
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"refreshed": true, "duration_ms": d.Milliseconds()})
	return nil
}

func (s *Server) dataQuality(w http.ResponseWriter, r *http.Request) error {
	rows, err := s.st.DataQuality(r.Context())
	if err != nil {
		return err
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"issues": rows})
	return nil
}

func (s *Server) audit(w http.ResponseWriter, r *http.Request) error {
	var before *int64
	if b := r.URL.Query().Get("before"); b != "" {
		n, err := strconv.ParseInt(b, 10, 64)
		if err != nil {
			return httpx.Invalid("before", "sayı olmalı")
		}
		before = &n
	}
	limit := 50
	list, err := s.st.ListAudit(r.Context(), r.URL.Query().Get("entity"), before, limit)
	if err != nil {
		return err
	}
	next := ""
	if len(list) == limit {
		next = strconv.FormatInt(list[len(list)-1].ID, 10)
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"items": list, "next_before": next})
	return nil
}
