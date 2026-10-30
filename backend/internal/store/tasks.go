package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"pistkoordinat/internal/domain/crew"
	"pistkoordinat/internal/domain/tasksync"
)

type CrewInfo struct {
	ID       int               `json:"id"`
	Name     string            `json:"name"`
	TaskType string            `json:"task_type"`
	LeadName *string           `json:"lead_name,omitempty"`
	Certs    map[string]string `json:"certifications"`
	Shifts   []crew.Window     `json:"shifts"`
}

func (s *Store) ListCrews(ctx context.Context, from, to time.Time) ([]CrewInfo, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT c.id, c.name, c.task_type, u.full_name,
		       coalesce((SELECT jsonb_object_agg(cert_type, to_char(valid_until,'YYYY-MM-DD')) FROM crew_certifications cc WHERE cc.crew_id=c.id), '{}'),
		       coalesce((SELECT jsonb_agg(jsonb_build_object('Start', lower(period), 'End', upper(period)) ORDER BY lower(period))
		                   FROM crew_shifts sh WHERE sh.crew_id=c.id AND sh.period && tstzrange($1,$2)), '[]')
		  FROM crews c LEFT JOIN users u ON u.id = c.lead_user_id
		 WHERE c.active ORDER BY c.task_type, c.name`, from, to)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (CrewInfo, error) {
		var c CrewInfo
		err := r.Scan(&c.ID, &c.Name, &c.TaskType, &c.LeadName, &c.Certs, &c.Shifts)
		return c, err
	})
}

var ErrRestRule = errors.New("dinlenme kuralı")

// AddShift dinlenme kuralını kontrol ederek vardiya ekler. Aynı ekip için eşzamanlı
// iki ekleme ekip satırı kilitlenerek sıraya sokulur.
func (s *Store) AddShift(ctx context.Context, crewID int, w crew.Window) (int64, error) {
	var id int64
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT id FROM crews WHERE id=$1 FOR UPDATE`, crewID).Scan(&id); err != nil {
			return notFound(err)
		}
		rows, err := tx.Query(ctx, `SELECT lower(period), upper(period) FROM crew_shifts WHERE crew_id=$1
			AND period && tstzrange($2::timestamptz - interval '1 day', $3::timestamptz + interval '1 day')`, crewID, w.Start, w.End)
		if err != nil {
			return err
		}
		existing, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (crew.Window, error) {
			var x crew.Window
			return x, r.Scan(&x.Start, &x.End)
		})
		if err != nil {
			return err
		}
		if err := crew.ValidateRest(existing, w); err != nil {
			return fmt.Errorf("%w: %v", ErrRestRule, err)
		}
		return tx.QueryRow(ctx, `INSERT INTO crew_shifts(crew_id, period) VALUES ($1, tstzrange($2,$3)) RETURNING id`,
			crewID, w.Start, w.End).Scan(&id)
	})
	return id, err
}

type EquipmentRow struct {
	ID     int    `json:"id"`
	Code   string `json:"code"`
	Kind   string `json:"kind"`
	Active bool   `json:"active"`
}

func (s *Store) ListEquipment(ctx context.Context) ([]EquipmentRow, error) {
	rows, err := s.Pool.Query(ctx, `SELECT id, code, kind, active FROM equipment ORDER BY kind, code`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[EquipmentRow])
}

// LoadCrewProblem atama için ekipleri, vardiyaları, ekipmanı ve günün görevlerini okur.
// reassign=true ise başlamamış görevler serbest bırakılır (mevcut ekibi tercih olarak kalır).
func LoadCrewProblem(ctx context.Context, q Q, opDate string, reassign bool) (crew.Problem, error) {
	var p crew.Problem
	day, err := time.Parse("2006-01-02", opDate)
	if err != nil {
		return p, err
	}
	from, to := day.Add(-24*time.Hour), day.Add(48*time.Hour)

	rows, err := q.Query(ctx, `
		SELECT c.id, c.name, c.task_type,
		       coalesce((SELECT jsonb_object_agg(cert_type, valid_until) FROM crew_certifications cc WHERE cc.crew_id=c.id), '{}'),
		       coalesce((SELECT jsonb_agg(jsonb_build_object('Start', lower(period), 'End', upper(period)))
		                   FROM crew_shifts sh WHERE sh.crew_id=c.id AND sh.period && tstzrange($1,$2)), '[]')
		  FROM crews c WHERE c.active`, from, to)
	if err != nil {
		return p, err
	}
	p.Crews, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (crew.Crew, error) {
		var c crew.Crew
		var certs map[string]string
		var tt string
		if err := r.Scan(&c.ID, &c.Name, &tt, &certs, &c.Shifts); err != nil {
			return c, err
		}
		c.Type = crew.TaskType(tt)
		c.Certs = map[string]time.Time{}
		for k, v := range certs {
			d, _ := time.Parse("2006-01-02", v)
			c.Certs[k] = d
		}
		return c, nil
	})
	if err != nil {
		return p, err
	}

	rows, err = q.Query(ctx, `SELECT id, kind FROM equipment WHERE active`)
	if err != nil {
		return p, err
	}
	p.Equipment, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (crew.Equipment, error) {
		var e crew.Equipment
		return e, r.Scan(&e.ID, &e.Kind)
	})
	if err != nil {
		return p, err
	}

	rows, err = q.Query(ctx, `
		SELECT k.id, k.turnaround_id, k.task_type, k.planned_start, k.planned_end, t.off_block,
		       t.on_block + make_interval(mins => at.target_turnaround_min),
		       k.status IN ('STARTED','DONE'), coalesce(k.crew_id,0), coalesce(k.equipment_id,0)
		  FROM tasks k
		  JOIN turnarounds t ON t.id = k.turnaround_id
		  JOIN aircraft a ON a.tail_number = t.tail_number
		  JOIN aircraft_types at ON at.code = a.type_code
		 WHERE t.op_date = $1::date AND k.status <> 'CANCELLED'`, opDate)
	if err != nil {
		return p, err
	}
	p.Tasks, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (crew.Task, error) {
		var k crew.Task
		var tt string
		err := r.Scan(&k.ID, &k.TurnaroundID, &tt, &k.Start, &k.End, &k.Deadline, &k.TargetEnd, &k.Pinned, &k.CrewID, &k.EquipmentID)
		k.Type = crew.TaskType(tt)
		if reassign && !k.Pinned {
			k.PreferCrewID = k.CrewID
			k.CrewID, k.EquipmentID = 0, 0
		}
		return k, err
	})
	return p, err
}

// GenerateTasks standı belli olan ve görevi olmayan turnaroundlar için görev üretir.
func (s *Store) GenerateTasks(ctx context.Context, q Q, opDate string) (int, error) {
	rows, err := q.Query(ctx, `
		SELECT t.id, t.on_block, t.off_block, at.size_class, t.needs_bridge AND s.has_bridge, t.dep_flight_id IS NOT NULL
		  FROM turnarounds t
		  JOIN aircraft a ON a.tail_number = t.tail_number
		  JOIN aircraft_types at ON at.code = a.type_code
		  JOIN stand_allocations sa ON sa.turnaround_id = t.id AND sa.active
		  JOIN stands s ON s.id = sa.stand_id
		 WHERE t.op_date = $1::date AND NOT EXISTS (SELECT 1 FROM tasks k WHERE k.turnaround_id = t.id)`, opDate)
	if err != nil {
		return 0, err
	}
	type ta struct {
		id      int64
		on, off time.Time
		size    string
		bridge  bool
		departs bool
	}
	list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (ta, error) {
		var x ta
		return x, r.Scan(&x.id, &x.on, &x.off, &x.size, &x.bridge, &x.departs)
	})
	if err != nil {
		return 0, err
	}
	batch := &pgx.Batch{}
	n := 0
	for _, t := range list {
		for _, spec := range crew.Templates(t.on, t.off, t.size[0], t.bridge) {
			if spec.Type == crew.Fuel && !t.departs {
				continue // gece kalan uçağa yakıt sabah kalkış turnaround'unda verilir
			}
			batch.Queue(`INSERT INTO tasks(turnaround_id, task_type, planned_start, planned_end) VALUES ($1,$2,$3,$4)
				ON CONFLICT (turnaround_id, task_type) DO NOTHING`, t.id, string(spec.Type), spec.Start, spec.End)
			n++
		}
	}
	if n == 0 {
		return 0, nil
	}
	br := q.(interface {
		SendBatch(context.Context, *pgx.Batch) pgx.BatchResults
	}).SendBatch(ctx, batch)
	return n, br.Close()
}

type AssignSummary struct {
	OpDate          string                `json:"op_date"`
	Tasks           int                   `json:"tasks"`
	Assigned        int                   `json:"assigned"`
	Changed         int                   `json:"changed"`
	Unassigned      []crew.Unassigned     `json:"unassigned"`
	Risks           []crew.Risk           `json:"risks"`
	ShiftViolations []crew.ShiftViolation `json:"shift_violations"`
	DurationMS      int64                 `json:"duration_ms"`
}

func (s *Store) RunAssignment(ctx context.Context, opDate string, actorID int64) (AssignSummary, error) {
	started := time.Now()
	var sum AssignSummary
	err := pgx.BeginTxFunc(ctx, s.Pool, pgx.TxOptions{IsoLevel: pgx.RepeatableRead}, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('crew:' || $1))`, opDate); err != nil {
			return err
		}
		if _, err := s.GenerateTasks(ctx, tx, opDate); err != nil {
			return err
		}
		p, err := LoadCrewProblem(ctx, tx, opDate, true)
		if err != nil {
			return err
		}
		res := crew.Assign(p)
		// önce boşalt: EXCLUDE kısıtları geçici çakışma görmesin
		if _, err := tx.Exec(ctx, `UPDATE tasks k SET crew_id=NULL, equipment_id=NULL
			  FROM turnarounds t WHERE t.id=k.turnaround_id AND t.op_date=$1::date AND k.status IN ('PENDING','ASSIGNED')`, opDate); err != nil {
			return err
		}
		for _, k := range p.Tasks {
			if k.Pinned {
				continue
			}
			a, ok := res.Assign[k.ID]
			if !ok {
				if _, err := tx.Exec(ctx, `UPDATE tasks SET status='PENDING', updated_at=now() WHERE id=$1 AND status IN ('PENDING','ASSIGNED')`, k.ID); err != nil {
					return err
				}
				continue
			}
			if a.CrewID != k.PreferCrewID {
				sum.Changed++
				if _, err := tx.Exec(ctx, `INSERT INTO task_events(task_id, turnaround_id, task_type, event_type, occurred_at, client_event_id, actor_id)
					VALUES ($1,$2,$3,'ASSIGNED',now(),gen_random_uuid(),$4)`, k.ID, k.TurnaroundID, string(k.Type), nullID(actorID)); err != nil {
					return err
				}
			}
			if _, err := tx.Exec(ctx, `UPDATE tasks SET crew_id=$2, equipment_id=NULLIF($3,0), planned_start=$4, planned_end=$5,
				status='ASSIGNED', version=version+1, updated_at=now() WHERE id=$1 AND status IN ('PENDING','ASSIGNED')`, k.ID, a.CrewID, a.EquipmentID, a.Start, a.End); err != nil {
				return fmt.Errorf("görev %d: %w", k.ID, err)
			}
			sum.Assigned++
		}
		sum.OpDate, sum.Tasks = opDate, len(p.Tasks)
		sum.Unassigned, sum.Risks, sum.ShiftViolations = res.Unassigned, res.Risks, res.ShiftViolations
		return nil
	})
	if sum.Unassigned == nil {
		sum.Unassigned = []crew.Unassigned{}
	}
	if sum.Risks == nil {
		sum.Risks = []crew.Risk{}
	}
	if sum.ShiftViolations == nil {
		sum.ShiftViolations = []crew.ShiftViolation{}
	}
	sum.DurationMS = time.Since(started).Milliseconds()
	return sum, err
}

// Risks atama yapmadan güncel riskleri hesaplar (salt okunur önizleme).
func (s *Store) Risks(ctx context.Context, opDate string) (AssignSummary, error) {
	p, err := LoadCrewProblem(ctx, s.Pool, opDate, true)
	if err != nil {
		return AssignSummary{}, err
	}
	res := crew.Assign(p)
	return AssignSummary{OpDate: opDate, Tasks: len(p.Tasks), Assigned: len(res.Assign),
		Unassigned: res.Unassigned, Risks: res.Risks, ShiftViolations: res.ShiftViolations}, nil
}

type TaskRow struct {
	ID            int64      `json:"id"`
	TurnaroundID  int64      `json:"turnaround_id"`
	TaskType      string     `json:"task_type"`
	TailNumber    string     `json:"tail_number"`
	StandCode     *string    `json:"stand_code,omitempty"`
	FlightNos     string     `json:"flights"`
	PlannedStart  time.Time  `json:"planned_start"`
	PlannedEnd    time.Time  `json:"planned_end"`
	CrewID        *int       `json:"crew_id,omitempty"`
	CrewName      *string    `json:"crew_name,omitempty"`
	EquipmentCode *string    `json:"equipment_code,omitempty"`
	Status        string     `json:"status"`
	StartedAt     *time.Time `json:"started_at,omitempty"`
	FinishedAt    *time.Time `json:"finished_at,omitempty"`
	Version       int        `json:"version"`
}

func (s *Store) ListTasks(ctx context.Context, opDate string, crewID int, afterAt *time.Time, afterID int64, limit int) ([]TaskRow, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT k.id, k.turnaround_id, k.task_type, t.tail_number, st.code,
		       concat_ws(' / ', fa.flight_no, fd.flight_no),
		       k.planned_start, k.planned_end, k.crew_id, c.name, e.code, k.status, k.started_at, k.finished_at, k.version
		  FROM tasks k
		  JOIN turnarounds t ON t.id = k.turnaround_id
		  LEFT JOIN stand_allocations sa ON sa.turnaround_id = t.id AND sa.active
		  LEFT JOIN stands st ON st.id = sa.stand_id
		  LEFT JOIN flights fa ON fa.id = t.arr_flight_id
		  LEFT JOIN flights fd ON fd.id = t.dep_flight_id
		  LEFT JOIN crews c ON c.id = k.crew_id
		  LEFT JOIN equipment e ON e.id = k.equipment_id
		 WHERE t.op_date = $1::date AND ($2 = 0 OR k.crew_id = $2)
		   AND ($3::timestamptz IS NULL OR (k.planned_start, k.id) > ($3, $4))
		 ORDER BY k.planned_start, k.id LIMIT $5`, opDate, crewID, afterAt, afterID, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[TaskRow])
}

// ---------- çevrimdışı senkronizasyon (T-4) ----------

type SyncResult struct {
	ClientEventID string          `json:"client_event_id"`
	TaskID        int64           `json:"task_id"`
	Result        tasksync.Result `json:"result"`
	Reason        string          `json:"reason,omitempty"`
	State         *tasksync.State `json:"state,omitempty"`
	Version       int             `json:"version,omitempty"`
}

// SyncTaskEvents her olayı kendi transaction'ında işler. Görev satırı FOR UPDATE
// kilitlenir; böylece iki cihazın aynı görev için eşzamanlı senkronizasyonu
// sıraya girer ve katlama her seferinde tüm olay geçmişi üzerinden yapılır.
func (s *Store) SyncTaskEvents(ctx context.Context, crewID int, actorID int64, deviceID string, evs []tasksync.Event) ([]SyncResult, error) {
	out := make([]SyncResult, 0, len(evs))
	now := time.Now()
	for _, e := range evs {
		r := SyncResult{ClientEventID: e.ClientEventID, TaskID: e.TaskID}
		if msg := e.Validate(now); msg != "" {
			r.Result, r.Reason = tasksync.Rejected, msg
			out = append(out, r)
			continue
		}
		err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
			var taID int64
			var taskType, status string
			var owner *int
			var started, finished *time.Time
			err := tx.QueryRow(ctx, `SELECT turnaround_id, task_type, status, crew_id, started_at, finished_at
				FROM tasks WHERE id=$1 FOR UPDATE`, e.TaskID).Scan(&taID, &taskType, &status, &owner, &started, &finished)
			if errors.Is(err, pgx.ErrNoRows) {
				r.Result, r.Reason = tasksync.Rejected, "görev yok"
				return nil
			}
			if err != nil {
				return err
			}
			if crewID != 0 && (owner == nil || *owner != crewID) {
				r.Result, r.Reason = tasksync.Rejected, "görev bu ekibe ait değil"
				return nil
			}
			if status == "CANCELLED" || status == "PENDING" {
				r.Result, r.Reason = tasksync.Rejected, "görev "+status+" durumunda"
				return nil
			}
			before := tasksync.State{Status: status, StartedAt: started, FinishedAt: finished}
			tag, err := tx.Exec(ctx, `INSERT INTO task_events(task_id, turnaround_id, task_type, event_type, occurred_at,
				client_event_id, device_id, actor_id) VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
				ON CONFLICT (client_event_id, occurred_at) DO NOTHING`,
				e.TaskID, taID, taskType, string(e.Type), e.OccurredAt, e.ClientEventID, deviceID, nullID(actorID))
			if err != nil {
				return err
			}
			if tag.RowsAffected() == 0 {
				r.Result = tasksync.Duplicate
				r.State = &before
				return nil
			}
			hist, err := loadSyncEvents(ctx, tx, e.TaskID)
			if err != nil {
				return err
			}
			after := tasksync.Fold("ASSIGNED", hist)
			r.Result = tasksync.Classify(before, after)
			r.State = &after
			return tx.QueryRow(ctx, `UPDATE tasks SET status=$2, started_at=$3, finished_at=$4, version=version+1, updated_at=now()
				WHERE id=$1 RETURNING version`, e.TaskID, after.Status, after.StartedAt, after.FinishedAt).Scan(&r.Version)
		})
		if err != nil {
			return out, err
		}
		out = append(out, r)
	}
	return out, nil
}

func loadSyncEvents(ctx context.Context, q Q, taskID int64) ([]tasksync.Event, error) {
	rows, err := q.Query(ctx, `SELECT client_event_id::text, task_id, event_type, occurred_at, coalesce(device_id,'')
		FROM task_events WHERE task_id=$1 AND event_type IN ('STARTED','FINISHED')`, taskID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (tasksync.Event, error) {
		var e tasksync.Event
		var t string
		err := r.Scan(&e.ClientEventID, &e.TaskID, &t, &e.OccurredAt, &e.DeviceID)
		e.Type = tasksync.EventType(t)
		return e, err
	})
}

type TaskEvent struct {
	ID         int64     `json:"id"`
	EventType  string    `json:"event_type"`
	OccurredAt time.Time `json:"occurred_at"`
	ReceivedAt time.Time `json:"received_at"`
	DeviceID   *string   `json:"device_id,omitempty"`
	ActorID    *int64    `json:"actor_id,omitempty"`
}

func (s *Store) TaskHistory(ctx context.Context, taskID int64) ([]TaskEvent, error) {
	rows, err := s.Pool.Query(ctx, `SELECT id, event_type, occurred_at, received_at, device_id, actor_id
		FROM task_events WHERE task_id=$1 ORDER BY occurred_at, id`, taskID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[TaskEvent])
}

func (s *Store) TaskCrew(ctx context.Context, taskID int64) (int, error) {
	var c *int
	err := s.Pool.QueryRow(ctx, `SELECT crew_id FROM tasks WHERE id=$1`, taskID).Scan(&c)
	if err != nil {
		return 0, notFound(err)
	}
	if c == nil {
		return 0, nil
	}
	return *c, nil
}
