package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"pistkoordinat/internal/domain/allocation"
	"pistkoordinat/internal/domain/propagation"
)

type TurnaroundRow struct {
	ID          int64
	Tail        string
	ArrFlightID int64
	DepFlightID int64
	On, Off     time.Time
	Size        byte
	NeedsBridge bool
	MinTurn     time.Duration
	TargetTurn  time.Duration
	Locked      bool
	StandID     int
	Airline     string
}

// LoadTurnarounds o günün turnaroundlarını (yanında bir önceki günden sarkanları da) okur.
func LoadTurnarounds(ctx context.Context, q Q, opDate string) ([]TurnaroundRow, error) {
	rows, err := q.Query(ctx, `
		SELECT t.id, t.tail_number, coalesce(t.arr_flight_id,0), coalesce(t.dep_flight_id,0),
		       t.on_block, t.off_block, at.size_class, t.needs_bridge,
		       at.min_turnaround_min, at.target_turnaround_min,
		       (t.actual_on_block IS NOT NULL OR coalesce(fa.status,'') = 'LANDED' OR coalesce(fd.status,'') = 'DEPARTED'),
		       coalesce(sa.stand_id, 0), a.airline_code
		  FROM turnarounds t
		  JOIN aircraft a ON a.tail_number = t.tail_number
		  JOIN aircraft_types at ON at.code = a.type_code
		  LEFT JOIN flights fa ON fa.id = t.arr_flight_id
		  LEFT JOIN flights fd ON fd.id = t.dep_flight_id
		  LEFT JOIN stand_allocations sa ON sa.turnaround_id = t.id AND sa.active
		 WHERE t.op_date = $1::date AND NOT t.synthetic
		   AND coalesce(fa.status,'') <> 'CANCELLED' AND coalesce(fd.status,'') <> 'CANCELLED'
		 ORDER BY t.on_block, t.id`, opDate)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (TurnaroundRow, error) {
		var t TurnaroundRow
		var size string
		var minT, tgtT int
		err := r.Scan(&t.ID, &t.Tail, &t.ArrFlightID, &t.DepFlightID, &t.On, &t.Off, &size, &t.NeedsBridge,
			&minT, &tgtT, &t.Locked, &t.StandID, &t.Airline)
		t.Size = size[0]
		t.MinTurn = time.Duration(minT) * time.Minute
		t.TargetTurn = time.Duration(tgtT) * time.Minute
		return t, err
	})
}

func LoadStands(ctx context.Context, q Q) ([]allocation.Stand, error) {
	rows, err := q.Query(ctx, `SELECT id, code, max_size_class, has_bridge, coalesce(gate_id,0), active FROM stands ORDER BY id`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (allocation.Stand, error) {
		var s allocation.Stand
		var size string
		err := r.Scan(&s.ID, &s.Code, &size, &s.HasBridge, &s.GateID, &s.Active)
		s.MaxSize = size[0]
		return s, err
	})
}

type RunSummary struct {
	RunID       int64                    `json:"run_id"`
	OpDate      string                   `json:"op_date"`
	DurationMS  int64                    `json:"duration_ms"`
	SolveMS     float64                  `json:"solve_ms"`
	Total       int                      `json:"total"`
	Allocated   int                      `json:"allocated"`
	Unallocated []allocation.Unallocated `json:"unallocated"`
	Changed     int                      `json:"changed"`
}

// RunAllocation günün tüm turnaroundlarını yeniden çözer. Kilitli (standda olan)
// atamalar sabit kalır; mevcut plan "tercih" olarak verilir ki gereksiz taşıma olmasın.
func (s *Store) RunAllocation(ctx context.Context, opDate string, actorID int64) (RunSummary, error) {
	started := time.Now()
	var sum RunSummary
	err := pgx.BeginTxFunc(ctx, s.Pool, pgx.TxOptions{IsoLevel: pgx.RepeatableRead}, func(tx pgx.Tx) error {
		// aynı gün için iki tahsis aynı anda çalışmasın
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('alloc:' || $1))`, opDate); err != nil {
			return err
		}
		tas, err := LoadTurnarounds(ctx, tx, opDate)
		if err != nil {
			return err
		}
		stands, err := LoadStands(ctx, tx)
		if err != nil {
			return err
		}
		p := allocation.Problem{Stands: stands, Pinned: map[int64]int{}, Current: map[int64]int{}, Changed: map[int64]bool{}}
		lastStand := map[string]int{}
		for _, t := range tas {
			at := allocation.Turnaround{ID: t.ID, Tail: t.Tail, Size: t.Size, NeedsBridge: t.NeedsBridge, On: t.On, Off: t.Off,
				PreferStand: lastStand[t.Tail]}
			p.Turnarounds = append(p.Turnarounds, at)
			if t.StandID != 0 {
				p.Current[t.ID] = t.StandID
				lastStand[t.Tail] = t.StandID
				if t.Locked {
					p.Pinned[t.ID] = t.StandID
				}
			}
		}
		t0 := time.Now()
		res := allocation.Solve(p)
		sum.SolveMS = float64(time.Since(t0).Microseconds()) / 1000

		gateOf := map[int]int{}
		for _, st := range stands {
			gateOf[st.ID] = st.GateID
		}
		for _, t := range tas {
			newStand := res.Assign[t.ID]
			if newStand == t.StandID {
				continue
			}
			sum.Changed++
			if err := applyStand(ctx, tx, t.ID, t.On, t.Off, newStand, gateOf[newStand], actorID); err != nil {
				return err
			}
		}
		sum.OpDate, sum.Total, sum.Allocated, sum.Unallocated = opDate, len(tas), len(res.Assign), res.Unallocated
		if sum.Unallocated == nil {
			sum.Unallocated = []allocation.Unallocated{}
		}
		sum.DurationMS = time.Since(started).Milliseconds()
		un, _ := json.Marshal(sum.Unallocated)
		return tx.QueryRow(ctx, `INSERT INTO allocation_runs(op_date, duration_ms, total, allocated, unallocated, created_by)
			VALUES ($1,$2,$3,$4,$5,$6) RETURNING id`, opDate, sum.DurationMS, sum.Total, sum.Allocated, un, nullID(actorID)).Scan(&sum.RunID)
	})
	return sum, err
}

func nullID(id int64) *int64 {
	if id == 0 {
		return nil
	}
	return &id
}

// applyStand bir turnaround'un aktif atamasını değiştirir. Eski kayıt pasifleşir
// (geçmiş kalsın diye silinmez); yenisini EXCLUDE kısıtı denetler.
func applyStand(ctx context.Context, q Q, taID int64, on, off time.Time, standID, gateID int, actorID int64) error {
	if _, err := q.Exec(ctx, `UPDATE stand_allocations SET active=false WHERE turnaround_id=$1 AND active`, taID); err != nil {
		return err
	}
	if standID == 0 {
		_, err := q.Exec(ctx, `UPDATE turnarounds SET actual_stand_id = NULL WHERE id=$1 AND actual_on_block IS NULL`, taID)
		return err
	}
	var gate *int
	if gateID != 0 {
		gate = &gateID
	}
	if _, err := q.Exec(ctx, `INSERT INTO stand_allocations(turnaround_id, stand_id, gate_id, on_block, off_block, created_by)
		VALUES ($1,$2,$3,$4,$5,$6)`, taID, standID, gate, on, off, nullID(actorID)); err != nil {
		return err
	}
	_, err := q.Exec(ctx, `UPDATE turnarounds SET actual_stand_id=$2 WHERE id=$1`, taID, standID)
	return err
}

type Allocation struct {
	ID           int64     `json:"id"`
	TurnaroundID int64     `json:"turnaround_id"`
	TailNumber   string    `json:"tail_number"`
	AircraftType string    `json:"aircraft_type"`
	AirlineCode  string    `json:"airline_code"`
	ArrFlight    *string   `json:"arr_flight,omitempty"`
	DepFlight    *string   `json:"dep_flight,omitempty"`
	StandID      int       `json:"stand_id"`
	StandCode    string    `json:"stand_code"`
	GateCode     *string   `json:"gate_code,omitempty"`
	OnBlock      time.Time `json:"on_block"`
	OffBlock     time.Time `json:"off_block"`
	Version      int       `json:"version"`
}

func (s *Store) ListAllocations(ctx context.Context, opDate, airline string) ([]Allocation, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT sa.id, t.id, t.tail_number, a.type_code, a.airline_code, fa.flight_no, fd.flight_no,
		       s.id, s.code, g.code, sa.on_block, sa.off_block, sa.version
		  FROM stand_allocations sa
		  JOIN turnarounds t ON t.id = sa.turnaround_id
		  JOIN aircraft a ON a.tail_number = t.tail_number
		  JOIN stands s ON s.id = sa.stand_id
		  LEFT JOIN gates g ON g.id = coalesce(sa.gate_id, s.gate_id)
		  LEFT JOIN flights fa ON fa.id = t.arr_flight_id
		  LEFT JOIN flights fd ON fd.id = t.dep_flight_id
		 WHERE sa.active AND t.op_date = $1::date AND ($2 = '' OR a.airline_code = $2)
		 ORDER BY s.code, sa.on_block`, opDate, airline)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[Allocation])
}

// ManualAllocate operatörün elle atamasıdır; kural ihlalleri veritabanından gelir.
func (s *Store) ManualAllocate(ctx context.Context, taID int64, standID int, actorID int64) (Allocation, error) {
	var out Allocation
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		var on, off time.Time
		var gate *int
		if err := tx.QueryRow(ctx, `SELECT on_block, off_block FROM turnarounds WHERE id=$1 FOR UPDATE`, taID).Scan(&on, &off); err != nil {
			return notFound(err)
		}
		if err := tx.QueryRow(ctx, `SELECT gate_id FROM stands WHERE id=$1`, standID).Scan(&gate); err != nil {
			return notFound(err)
		}
		g := 0
		if gate != nil {
			g = *gate
		}
		if err := applyStand(ctx, tx, taID, on, off, standID, g, actorID); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT sa.id, sa.turnaround_id, sa.stand_id, s.code, sa.on_block, sa.off_block, sa.version
			FROM stand_allocations sa JOIN stands s ON s.id=sa.stand_id WHERE sa.turnaround_id=$1 AND sa.active`, taID).
			Scan(&out.ID, &out.TurnaroundID, &out.StandID, &out.StandCode, &out.OnBlock, &out.OffBlock, &out.Version)
	})
	return out, err
}

func (s *Store) LatestRun(ctx context.Context, opDate string) (RunSummary, error) {
	var r RunSummary
	var un []byte
	var d string
	err := s.Pool.QueryRow(ctx, `SELECT id, to_char(op_date,'YYYY-MM-DD'), duration_ms, total, allocated, unallocated
		FROM allocation_runs WHERE op_date=$1::date ORDER BY id DESC LIMIT 1`, opDate).
		Scan(&r.RunID, &d, &r.DurationMS, &r.Total, &r.Allocated, &un)
	if err != nil {
		return r, notFound(err)
	}
	r.OpDate = d
	_ = json.Unmarshal(un, &r.Unallocated)
	return r, nil
}

// ---------- yeniden planlama (T-2) ----------

// LoadTimeline gecikme yayılımı için günün tam resmini okur.
func LoadTimeline(ctx context.Context, q Q, opDate string) (propagation.Timeline, map[int64]TurnaroundRow, error) {
	var tl propagation.Timeline
	tas, err := LoadTurnarounds(ctx, q, opDate)
	if err != nil {
		return tl, nil, err
	}
	stands, err := LoadStands(ctx, q)
	if err != nil {
		return tl, nil, err
	}
	byID := map[int64]TurnaroundRow{}
	tl.Stands = stands
	tl.StandOf = map[int64]int{}
	for _, t := range tas {
		byID[t.ID] = t
		tl.Turnarounds = append(tl.Turnarounds, propagation.Turnaround{ID: t.ID, Tail: t.Tail, ArrFlightID: t.ArrFlightID,
			DepFlightID: t.DepFlightID, On: t.On, Off: t.Off, MinTurn: t.MinTurn, Size: t.Size, NeedsBridge: t.NeedsBridge, Locked: t.Locked})
		if t.StandID != 0 {
			tl.StandOf[t.ID] = t.StandID
		}
	}
	cp, err := LoadCrewProblem(ctx, q, opDate, false)
	if err != nil {
		return tl, nil, err
	}
	tl.Crews, tl.Equipment, tl.Tasks = cp.Crews, cp.Equipment, cp.Tasks
	return tl, byID, nil
}

// ApplyPlan bir planı tek transaction'da uygular. Çağıran SERIALIZABLE açar.
func ApplyPlan(ctx context.Context, tx pgx.Tx, plan propagation.Plan, stands []allocation.Stand, actorID int64) error {
	gateOf := map[int]int{}
	for _, s := range stands {
		gateOf[s.ID] = s.GateID
	}
	for _, f := range plan.Flights {
		if _, err := tx.Exec(ctx, `UPDATE flights SET est_time=$2,
			status = CASE WHEN status IN ('CANCELLED','LANDED','DEPARTED') THEN status
			              WHEN $2 - sched_time > interval '5 minutes' THEN 'DELAYED' ELSE 'SCHEDULED' END,
			updated_at=now() WHERE id=$1`, f.FlightID, f.New); err != nil {
			return err
		}
	}
	newTimes := map[int64][2]time.Time{}
	for _, t := range plan.Turnarounds {
		newTimes[t.TurnaroundID] = [2]time.Time{t.NewOn, t.NewOff}
	}
	// Önce kayan/taşınan turnaroundların aktif atamalarını kapat ki yeni atamalar
	// birbiriyle geçici olarak çakışmasın.
	touched := map[int64]bool{}
	for id := range newTimes {
		touched[id] = true
	}
	for _, s := range plan.Stands {
		touched[s.TurnaroundID] = true
	}
	finalStand := map[int64]int{}
	for id := range touched {
		var cur int
		_ = tx.QueryRow(ctx, `SELECT stand_id FROM stand_allocations WHERE turnaround_id=$1 AND active`, id).Scan(&cur)
		finalStand[id] = cur
		if _, err := tx.Exec(ctx, `UPDATE stand_allocations SET active=false WHERE turnaround_id=$1 AND active`, id); err != nil {
			return err
		}
	}
	for _, s := range plan.Stands {
		finalStand[s.TurnaroundID] = s.NewStand
	}
	for id, nt := range newTimes {
		if _, err := tx.Exec(ctx, `UPDATE turnarounds SET on_block=$2, off_block=$3 WHERE id=$1`, id, nt[0], nt[1]); err != nil {
			return err
		}
	}
	for id := range touched {
		var on, off time.Time
		if err := tx.QueryRow(ctx, `SELECT on_block, off_block FROM turnarounds WHERE id=$1`, id).Scan(&on, &off); err != nil {
			return err
		}
		sid := finalStand[id]
		if sid == 0 {
			continue
		}
		if _, err := tx.Exec(ctx, `INSERT INTO stand_allocations(turnaround_id, stand_id, gate_id, on_block, off_block, created_by)
			VALUES ($1,$2,NULLIF($3,0),$4,$5,$6)`, id, sid, gateOf[sid], on, off, nullID(actorID)); err != nil {
			return fmt.Errorf("turnaround %d stand %d: %w", id, sid, err)
		}
		if _, err := tx.Exec(ctx, `UPDATE turnarounds SET actual_stand_id=$2 WHERE id=$1`, id, sid); err != nil {
			return err
		}
	}
	// görevler: önce boşalt, sonra yeni zaman/ekiple yaz (EXCLUDE geçici çakışma görmesin)
	for _, k := range plan.Tasks {
		if _, err := tx.Exec(ctx, `UPDATE tasks SET crew_id=NULL, equipment_id=NULL WHERE id=$1`, k.TaskID); err != nil {
			return err
		}
	}
	for _, k := range plan.Tasks {
		status := "ASSIGNED"
		if k.NewCrew == 0 {
			status = "PENDING"
		}
		if _, err := tx.Exec(ctx, `UPDATE tasks SET planned_start=$2, planned_end=$3, crew_id=NULLIF($4,0),
			equipment_id=NULLIF($5,0), status=$6, version=version+1, updated_at=now() WHERE id=$1`,
			k.TaskID, k.NewStart, k.NewEnd, k.NewCrew, k.NewEquip, status); err != nil {
			return fmt.Errorf("görev %d: %w", k.TaskID, err)
		}
	}
	return nil
}
