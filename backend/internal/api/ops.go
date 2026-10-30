package api

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"pistkoordinat/internal/auth"
	"pistkoordinat/internal/db"
	"pistkoordinat/internal/domain/feed"
	"pistkoordinat/internal/domain/propagation"
	"pistkoordinat/internal/httpx"
	"pistkoordinat/internal/metrics"
	"pistkoordinat/internal/store"
)

// ---------- F-1 uçuşlar ----------

func (s *Server) listFlights(w http.ResponseWriter, r *http.Request) error {
	q := r.URL.Query()
	date := q.Get("date")
	if date != "" {
		if _, err := time.Parse("2006-01-02", date); err != nil {
			return httpx.Invalid("date", "YYYY-MM-DD olmalı")
		}
	}
	f := store.FlightFilter{Date: date, Direction: q.Get("direction"), Airline: q.Get("airline"), Tail: q.Get("tail"), Status: q.Get("status")}
	if f.Direction != "" && f.Direction != "ARR" && f.Direction != "DEP" {
		return httpx.Invalid("direction", "ARR ya da DEP")
	}
	// havayolu temsilcisi yalnızca kendi havayolunu görür
	if c := claims(r); c.Role == auth.AirlineRep {
		if f.Airline != "" && f.Airline != c.Airline {
			return httpx.ErrForbidden
		}
		f.Airline = c.Airline
	}
	limit, cur, err := httpx.PageParams(r, 50, 500)
	if err != nil {
		return err
	}
	var at *time.Time
	var id int64
	if cur != nil {
		at, id = &cur.At, cur.ID
	}
	items, err := s.st.ListFlights(r.Context(), f, at, id, limit+1)
	if err != nil {
		return err
	}
	page := httpx.Page[store.Flight]{Items: items}
	if len(items) > limit {
		page.Items = items[:limit]
		last := page.Items[limit-1]
		page.NextCursor = httpx.Cursor{At: last.SchedTime, ID: last.ID}.Encode()
	}
	httpx.JSON(w, http.StatusOK, page)
	return nil
}

func (s *Server) getFlight(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r)
	if err != nil {
		return err
	}
	f, err := s.st.GetFlight(r.Context(), id)
	if err != nil {
		return dbErr(err)
	}
	if c := claims(r); c.Role == auth.AirlineRep && f.AirlineCode != c.Airline {
		return httpx.ErrNotFound // başka havayolunun uçuşunun varlığını bile söyleme
	}
	httpx.JSON(w, http.StatusOK, f)
	return nil
}

var (
	reFlightNo = regexp.MustCompile(`^[A-Z0-9]{2}[0-9]{1,4}[A-Z]?$`)
	reIATA     = regexp.MustCompile(`^[A-Z]{3}$`)
)

func (s *Server) createFlight(w http.ResponseWriter, r *http.Request) error {
	var in store.NewFlight
	if err := httpx.Decode(r, &in); err != nil {
		return err
	}
	switch {
	case !reFlightNo.MatchString(in.FlightNo):
		return httpx.Invalid("flight_no", "örn. YK1234")
	case in.Direction != "ARR" && in.Direction != "DEP":
		return httpx.Invalid("direction", "ARR ya da DEP")
	case !reIATA.MatchString(in.OtherAirport):
		return httpx.Invalid("other_airport", "3 harfli IATA kodu")
	case in.TailNumber == "":
		return httpx.Invalid("tail_number", "zorunlu")
	case in.SchedTime.IsZero():
		return httpx.Invalid("sched_time", "zorunlu (RFC3339)")
	}
	opDate := in.SchedTime.In(s.cfg.TimeZone).Format("2006-01-02")
	var id int64
	err := pgx.BeginFunc(r.Context(), s.pool, func(tx pgx.Tx) error {
		var err error
		id, err = s.st.CreateFlight(r.Context(), tx, in, opDate)
		if errors.Is(err, store.ErrNotFound) {
			return httpx.Invalid("tail_number", "bilinmeyen kuyruk numarası")
		}
		if err != nil {
			return err
		}
		return store.Audit(r.Context(), tx, claims(r), "flight.create", "flight", strconv.FormatInt(id, 10), nil, in)
	})
	if err != nil {
		return dbErr(err)
	}
	f, err := s.st.GetFlight(r.Context(), id)
	if err != nil {
		return err
	}
	httpx.JSON(w, http.StatusCreated, f)
	return nil
}

func (s *Server) buildSchedule(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		Date string `json:"date"`
	}
	if err := httpx.Decode(r, &in); err != nil {
		return err
	}
	if _, err := time.Parse("2006-01-02", in.Date); err != nil {
		return httpx.Invalid("date", "YYYY-MM-DD olmalı")
	}
	var n int
	err := pgx.BeginFunc(r.Context(), s.pool, func(tx pgx.Tx) error {
		var err error
		if n, err = s.st.BuildTurnarounds(r.Context(), tx, in.Date); err != nil {
			return err
		}
		return store.Audit(r.Context(), tx, claims(r), "schedule.build", "op_date", in.Date, nil, map[string]int{"turnarounds": n})
	})
	if err != nil {
		return dbErr(err)
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"op_date": in.Date, "turnarounds_created": n})
	return nil
}

// ---------- F-2 envanter ----------

func (s *Server) listStands(w http.ResponseWriter, r *http.Request) error {
	list, err := s.st.ListStands(r.Context(), r.URL.Query().Get("type"))
	if err != nil {
		return err
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"items": list})
	return nil
}

func (s *Server) patchStand(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r)
	if err != nil {
		return err
	}
	var in struct {
		Active *bool `json:"active"`
	}
	if err := httpx.Decode(r, &in); err != nil {
		return err
	}
	if in.Active == nil {
		return httpx.Invalid("active", "zorunlu")
	}
	err = pgx.BeginFunc(r.Context(), s.pool, func(tx pgx.Tx) error {
		old, err := s.st.SetStandActive(r.Context(), tx, int(id), *in.Active)
		if err != nil {
			return err
		}
		return store.Audit(r.Context(), tx, claims(r), "stand.update", "stand", strconv.FormatInt(id, 10),
			map[string]bool{"active": old}, map[string]bool{"active": *in.Active})
	})
	if err != nil {
		return dbErr(err)
	}
	s.hub.Publish("stand.updated", map[string]any{"stand_id": id, "active": *in.Active})
	httpx.JSON(w, http.StatusOK, map[string]any{"id": id, "active": *in.Active})
	return nil
}

func (s *Server) listGates(w http.ResponseWriter, r *http.Request) error {
	list, err := s.st.ListGates(r.Context())
	if err != nil {
		return err
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"items": list})
	return nil
}

func (s *Server) listTypes(w http.ResponseWriter, r *http.Request) error {
	list, err := s.st.ListAircraftTypes(r.Context())
	if err != nil {
		return err
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"items": list})
	return nil
}

// ---------- F-3 / T-1 tahsis ----------

func (s *Server) airlineScope(r *http.Request) string {
	if c := claims(r); c.Role == auth.AirlineRep {
		return c.Airline
	}
	return r.URL.Query().Get("airline")
}

func (s *Server) listAllocations(w http.ResponseWriter, r *http.Request) error {
	d, err := s.opDate(r)
	if err != nil {
		return err
	}
	list, err := s.st.ListAllocations(r.Context(), d, s.airlineScope(r))
	if err != nil {
		return err
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"op_date": d, "items": list})
	return nil
}

func (s *Server) gantt(w http.ResponseWriter, r *http.Request) error {
	d, err := s.opDate(r)
	if err != nil {
		return err
	}
	slots, err := s.st.Gantt(r.Context(), d, s.airlineScope(r))
	if err != nil {
		return err
	}
	stands, err := s.st.ListStands(r.Context(), "")
	if err != nil {
		return err
	}
	codes := make([]string, 0, len(stands))
	for _, st := range stands {
		codes = append(codes, st.Code)
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"op_date": d, "stands": codes, "slots": slots})
	return nil
}

func (s *Server) runAllocation(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		Date string `json:"date"`
	}
	if err := httpx.Decode(r, &in); err != nil {
		return err
	}
	if _, err := time.Parse("2006-01-02", in.Date); err != nil {
		return httpx.Invalid("date", "YYYY-MM-DD olmalı")
	}
	sum, err := s.st.RunAllocation(r.Context(), in.Date, claims(r).Sub)
	if err != nil {
		return dbErr(err)
	}
	_ = store.Audit(r.Context(), s.pool, claims(r), "allocation.run", "op_date", in.Date, nil,
		map[string]any{"run_id": sum.RunID, "allocated": sum.Allocated, "unallocated": len(sum.Unallocated), "changed": sum.Changed})
	s.hub.Publish("allocation.run", map[string]any{"op_date": in.Date, "allocated": sum.Allocated, "unallocated": len(sum.Unallocated), "changed": sum.Changed})
	httpx.JSON(w, http.StatusOK, sum)
	return nil
}

func (s *Server) latestRun(w http.ResponseWriter, r *http.Request) error {
	d, err := s.opDate(r)
	if err != nil {
		return err
	}
	run, err := s.st.LatestRun(r.Context(), d)
	if err != nil {
		return dbErr(err)
	}
	httpx.JSON(w, http.StatusOK, run)
	return nil
}

func (s *Server) manualAllocate(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		TurnaroundID int64 `json:"turnaround_id"`
		StandID      int   `json:"stand_id"`
	}
	if err := httpx.Decode(r, &in); err != nil {
		return err
	}
	if in.TurnaroundID <= 0 || in.StandID <= 0 {
		return httpx.Invalid("turnaround_id", "turnaround_id ve stand_id zorunlu")
	}
	a, err := s.st.ManualAllocate(r.Context(), in.TurnaroundID, in.StandID, claims(r).Sub)
	if err != nil {
		return dbErr(err)
	}
	_ = store.Audit(r.Context(), s.pool, claims(r), "allocation.manual", "turnaround", strconv.FormatInt(in.TurnaroundID, 10), nil, a)
	s.hub.Publish("allocation.changed", a)
	httpx.JSON(w, http.StatusCreated, a)
	return nil
}

// ---------- F-5 / T-2 besleme ve yeniden planlama ----------

func (s *Server) feedEvents(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		Events []feed.Event `json:"events"`
	}
	if err := httpx.Decode(r, &in); err != nil {
		return err
	}
	if len(in.Events) == 0 || len(in.Events) > 500 {
		return httpx.Invalid("events", "1-500 olay")
	}
	for i, e := range in.Events {
		if msg := e.Valid(); msg != "" {
			return httpx.Invalid("events["+strconv.Itoa(i)+"]", msg)
		}
	}
	results := make([]store.FeedResult, 0, len(in.Events))
	counts := map[feed.Outcome]int{}
	for _, e := range in.Events {
		res, err := s.st.ApplyFeedEvent(r.Context(), e)
		if err != nil {
			return dbErr(err)
		}
		results = append(results, res)
		counts[res.Outcome]++
		metrics.FeedEvents.Inc(string(res.Outcome))
		if res.Outcome == feed.Applied {
			s.hub.Publish("flight.updated", map[string]any{"flight_id": res.FlightID, "flight_no": e.FlightNo,
				"direction": e.Direction, "status": res.Status, "est_time": res.EstTime, "seq": e.Seq})
		}
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"results": results, "summary": counts})
	return nil
}

func (s *Server) pendingReplans(w http.ResponseWriter, r *http.Request) error {
	d, err := s.opDate(r)
	if err != nil {
		return err
	}
	list, err := s.st.PendingReplans(r.Context(), d)
	if err != nil {
		return err
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"op_date": d, "items": list})
	return nil
}

type replanInput struct {
	FlightID    int64     `json:"flight_id"`
	NewTime     time.Time `json:"new_time"`
	Fingerprint string    `json:"fingerprint,omitempty"`
}

func (s *Server) readReplan(r *http.Request) (replanInput, string, error) {
	var in replanInput
	if err := httpx.Decode(r, &in); err != nil {
		return in, "", err
	}
	if in.FlightID <= 0 || in.NewTime.IsZero() {
		return in, "", httpx.Invalid("flight_id", "flight_id ve new_time zorunlu")
	}
	f, err := s.st.GetFlight(r.Context(), in.FlightID)
	if err != nil {
		return in, "", dbErr(err)
	}
	return in, f.OpDate, nil
}

func (s *Server) computePlan(ctx context.Context, q store.Q, opDate string, in replanInput) (propagation.Plan, propagation.Timeline, error) {
	tl, _, err := store.LoadTimeline(ctx, q, opDate)
	if err != nil {
		return propagation.Plan{}, tl, err
	}
	plan, err := propagation.Replan(tl, propagation.Trigger{FlightID: in.FlightID, NewTime: in.NewTime.UTC()})
	if errors.Is(err, propagation.ErrUnknownFlight) {
		return plan, tl, httpx.Errf(http.StatusUnprocessableEntity, "not_planned", "uçuş bir turnaround'a bağlı değil (iptal ya da plansız)")
	}
	if err != nil {
		return plan, tl, httpx.Errf(http.StatusUnprocessableEntity, "replan_rejected", "%s", err.Error())
	}
	return plan, tl, nil
}

// replanPreview "ne olur" önizlemesi: hiçbir şey yazılmaz.
func (s *Server) replanPreview(w http.ResponseWriter, r *http.Request) error {
	in, opDate, err := s.readReplan(r)
	if err != nil {
		return err
	}
	plan, _, err := s.computePlan(r.Context(), s.pool, opDate, in)
	if err != nil {
		return err
	}
	httpx.JSON(w, http.StatusOK, plan)
	return nil
}

// replanApply planı SERIALIZABLE transaction içinde yeniden hesaplayıp uygular.
// İstemci önizlemedeki parmak izini gönderirse ve durum değişmişse 409 döner.
func (s *Server) replanApply(w http.ResponseWriter, r *http.Request) error {
	in, opDate, err := s.readReplan(r)
	if err != nil {
		return err
	}
	var plan propagation.Plan
	err = db.RetrySerializable(r.Context(), s.pool, 3, func(tx pgx.Tx) error {
		if _, err := tx.Exec(r.Context(), `SELECT pg_advisory_xact_lock(hashtext('alloc:' || $1))`, opDate); err != nil {
			return err
		}
		var tl propagation.Timeline
		var err error
		plan, tl, err = s.computePlan(r.Context(), tx, opDate, in)
		if err != nil {
			return err
		}
		if in.Fingerprint != "" && in.Fingerprint != plan.Fingerprint {
			return httpx.Errf(http.StatusConflict, "plan_changed", "önizlemeden sonra durum değişti; yeniden önizleyin")
		}
		if err := store.ApplyPlan(r.Context(), tx, plan, tl.Stands, claims(r).Sub); err != nil {
			return err
		}
		return store.Audit(r.Context(), tx, claims(r), "replan.apply", "flight", strconv.FormatInt(in.FlightID, 10), nil,
			map[string]any{"new_time": in.NewTime, "fingerprint": plan.Fingerprint, "stands": plan.Stands, "tasks": len(plan.Tasks),
				"turnarounds": len(plan.Turnarounds)})
	})
	if err != nil {
		return dbErr(err)
	}
	s.hub.Publish("replan.applied", map[string]any{"flight_id": in.FlightID, "turnarounds": len(plan.Turnarounds),
		"stand_changes": len(plan.Stands), "task_changes": len(plan.Tasks)})
	httpx.JSON(w, http.StatusOK, plan)
	return nil
}

func (s *Server) changes(w http.ResponseWriter, r *http.Request) error {
	var before *int64
	if b := r.URL.Query().Get("before"); b != "" {
		n, err := strconv.ParseInt(b, 10, 64)
		if err != nil {
			return httpx.Invalid("before", "sayı olmalı")
		}
		before = &n
	}
	list, err := s.st.ListAudit(r.Context(), r.URL.Query().Get("entity"), before, 50)
	if err != nil {
		return err
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"items": list})
	return nil
}
