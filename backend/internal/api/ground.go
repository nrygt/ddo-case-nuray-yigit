package api

import (
	"net/http"
	"strconv"
	"time"

	"pistkoordinat/internal/auth"
	"pistkoordinat/internal/domain/crew"
	"pistkoordinat/internal/domain/tasksync"
	"pistkoordinat/internal/httpx"
	"pistkoordinat/internal/metrics"
	"pistkoordinat/internal/store"
)

func (s *Server) listCrews(w http.ResponseWriter, r *http.Request) error {
	d, err := s.opDate(r)
	if err != nil {
		return err
	}
	day, _ := time.ParseInLocation("2006-01-02", d, s.cfg.TimeZone)
	list, err := s.st.ListCrews(r.Context(), day, day.Add(24*time.Hour))
	if err != nil {
		return err
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"items": list})
	return nil
}

func (s *Server) addShift(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r)
	if err != nil {
		return err
	}
	var in struct {
		Start time.Time `json:"start"`
		End   time.Time `json:"end"`
	}
	if err := httpx.Decode(r, &in); err != nil {
		return err
	}
	if !in.End.After(in.Start) || in.End.Sub(in.Start) > 12*time.Hour {
		return httpx.Invalid("end", "vardiya 0-12 saat olmalı")
	}
	sid, err := s.st.AddShift(r.Context(), int(id), crew.Window{Start: in.Start, End: in.End})
	if err != nil {
		if errorsIs(err, store.ErrRestRule) {
			return httpx.Errf(http.StatusUnprocessableEntity, "rest_rule_violation", "%s", err.Error())
		}
		return dbErr(err)
	}
	_ = store.Audit(r.Context(), s.pool, claims(r), "shift.create", "crew", strconv.FormatInt(id, 10), nil, in)
	httpx.JSON(w, http.StatusCreated, map[string]any{"id": sid, "crew_id": id, "start": in.Start, "end": in.End})
	return nil
}

func (s *Server) listEquipment(w http.ResponseWriter, r *http.Request) error {
	list, err := s.st.ListEquipment(r.Context())
	if err != nil {
		return err
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"items": list})
	return nil
}

func (s *Server) tasksPage(w http.ResponseWriter, r *http.Request, crewID int) error {
	d, err := s.opDate(r)
	if err != nil {
		return err
	}
	limit, cur, err := httpx.PageParams(r, 100, 500)
	if err != nil {
		return err
	}
	var at *time.Time
	var id int64
	if cur != nil {
		at, id = &cur.At, cur.ID
	}
	items, err := s.st.ListTasks(r.Context(), d, crewID, at, id, limit+1)
	if err != nil {
		return err
	}
	page := httpx.Page[store.TaskRow]{Items: items}
	if len(items) > limit {
		page.Items = items[:limit]
		last := page.Items[limit-1]
		page.NextCursor = httpx.Cursor{At: last.PlannedStart, ID: last.ID}.Encode()
	}
	httpx.JSON(w, http.StatusOK, page)
	return nil
}

func (s *Server) listTasks(w http.ResponseWriter, r *http.Request) error {
	crewID := 0
	if c := r.URL.Query().Get("crew_id"); c != "" {
		n, err := strconv.Atoi(c)
		if err != nil {
			return httpx.Invalid("crew_id", "sayı olmalı")
		}
		crewID = n
	}
	return s.tasksPage(w, r, crewID)
}

func (s *Server) myTasks(w http.ResponseWriter, r *http.Request) error {
	c := claims(r)
	if c.CrewID == 0 {
		return httpx.Errf(http.StatusForbidden, "no_crew", "bu kullanıcı bir ekibe lider değil")
	}
	return s.tasksPage(w, r, c.CrewID)
}

func (s *Server) taskEvents(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r)
	if err != nil {
		return err
	}
	if c := claims(r); c.Role == auth.CrewLead {
		owner, err := s.st.TaskCrew(r.Context(), id)
		if err != nil {
			return dbErr(err)
		}
		if owner != c.CrewID {
			return httpx.ErrForbidden
		}
	}
	list, err := s.st.TaskHistory(r.Context(), id)
	if err != nil {
		return err
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"task_id": id, "events": list})
	return nil
}

func (s *Server) runAssignment(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		Date string `json:"date"`
	}
	if err := httpx.Decode(r, &in); err != nil {
		return err
	}
	if _, err := time.Parse("2006-01-02", in.Date); err != nil {
		return httpx.Invalid("date", "YYYY-MM-DD olmalı")
	}
	sum, err := s.st.RunAssignment(r.Context(), in.Date, claims(r).Sub)
	if err != nil {
		return dbErr(err)
	}
	_ = store.Audit(r.Context(), s.pool, claims(r), "assignment.run", "op_date", in.Date, nil,
		map[string]any{"assigned": sum.Assigned, "changed": sum.Changed, "unassigned": len(sum.Unassigned), "risks": len(sum.Risks)})
	s.hub.Publish("assignment.run", map[string]any{"op_date": in.Date, "assigned": sum.Assigned, "risks": len(sum.Risks)})
	httpx.JSON(w, http.StatusOK, sum)
	return nil
}

func (s *Server) risks(w http.ResponseWriter, r *http.Request) error {
	d, err := s.opDate(r)
	if err != nil {
		return err
	}
	sum, err := s.st.Risks(r.Context(), d)
	if err != nil {
		return err
	}
	httpx.JSON(w, http.StatusOK, sum)
	return nil
}

// ---------- T-4 senkronizasyon ve akış ----------

func (s *Server) syncTaskEvents(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		DeviceID string           `json:"device_id"`
		Events   []tasksync.Event `json:"events"`
	}
	if err := httpx.Decode(r, &in); err != nil {
		return err
	}
	if in.DeviceID == "" || len(in.DeviceID) > 64 {
		return httpx.Invalid("device_id", "1-64 karakter")
	}
	if len(in.Events) == 0 || len(in.Events) > 200 {
		return httpx.Invalid("events", "1-200 olay")
	}
	c := claims(r)
	crewScope := 0
	if c.Role == auth.CrewLead {
		if c.CrewID == 0 {
			return httpx.Errf(http.StatusForbidden, "no_crew", "bu kullanıcı bir ekibe lider değil")
		}
		crewScope = c.CrewID
	}
	results, err := s.st.SyncTaskEvents(r.Context(), crewScope, c.Sub, in.DeviceID, in.Events)
	if err != nil {
		return dbErr(err)
	}
	for _, res := range results {
		metrics.SyncEvents.Inc(string(res.Result))
		if res.Result == tasksync.Applied || res.Result == tasksync.Merged {
			s.hub.Publish("task.updated", map[string]any{"task_id": res.TaskID, "state": res.State, "result": res.Result, "device_id": in.DeviceID})
		}
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"results": results, "server_time": time.Now().UTC()})
	return nil
}

func (s *Server) streamHandler(w http.ResponseWriter, r *http.Request) error {
	s.hub.ServeHTTP(w, r)
	return nil
}

// postNotice operasyon duyurusu (ör. "Pist 05 kapalı") panoya anında düşer.
func (s *Server) postNotice(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		Message string `json:"message"`
		Level   string `json:"level"`
	}
	if err := httpx.Decode(r, &in); err != nil {
		return err
	}
	if in.Message == "" || len(in.Message) > 500 {
		return httpx.Invalid("message", "1-500 karakter")
	}
	if in.Level == "" {
		in.Level = "info"
	}
	if in.Level != "info" && in.Level != "warning" && in.Level != "critical" {
		return httpx.Invalid("level", "info|warning|critical")
	}
	ev := s.hub.Publish("notice", map[string]any{"message": in.Message, "level": in.Level, "by": claims(r).Name})
	httpx.JSON(w, http.StatusAccepted, map[string]any{"event_id": ev.ID})
	return nil
}
