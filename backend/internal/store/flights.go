package store

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

type Flight struct {
	ID           int64      `json:"id"`
	FlightNo     string     `json:"flight_no"`
	AirlineCode  string     `json:"airline_code"`
	Direction    string     `json:"direction"`
	TailNumber   string     `json:"tail_number"`
	TypeCode     string     `json:"aircraft_type"`
	OtherAirport string     `json:"other_airport"`
	OpDate       string     `json:"op_date"`
	SchedTime    time.Time  `json:"sched_time"`
	EstTime      time.Time  `json:"est_time"`
	ActualTime   *time.Time `json:"actual_time,omitempty"`
	Status       string     `json:"status"`
	DelayMin     int        `json:"delay_min"`
	TurnaroundID *int64     `json:"turnaround_id,omitempty"`
	StandCode    *string    `json:"stand_code,omitempty"`
	GateCode     *string    `json:"gate_code,omitempty"`
}

type FlightFilter struct {
	Date      string
	Direction string
	Airline   string
	Tail      string
	Status    string
}

const flightSelect = `
SELECT f.id, f.flight_no, f.airline_code, f.direction, f.tail_number, a.type_code, f.other_airport,
       to_char(f.op_date,'YYYY-MM-DD'), f.sched_time, f.est_time, f.actual_time, f.status,
       (extract(epoch FROM f.est_time - f.sched_time)/60)::int,
       t.id, s.code, g.code
  FROM flights f
  JOIN aircraft a ON a.tail_number = f.tail_number
  LEFT JOIN turnarounds t ON t.arr_flight_id = f.id OR t.dep_flight_id = f.id
  LEFT JOIN stand_allocations sa ON sa.turnaround_id = t.id AND sa.active
  LEFT JOIN stands s ON s.id = sa.stand_id
  LEFT JOIN gates g ON g.id = coalesce(sa.gate_id, s.gate_id)`

func scanFlight(r pgx.Row) (Flight, error) {
	var f Flight
	err := r.Scan(&f.ID, &f.FlightNo, &f.AirlineCode, &f.Direction, &f.TailNumber, &f.TypeCode, &f.OtherAirport,
		&f.OpDate, &f.SchedTime, &f.EstTime, &f.ActualTime, &f.Status, &f.DelayMin, &f.TurnaroundID, &f.StandCode, &f.GateCode)
	return f, err
}

// ListFlights keyset sayfalama: (sched_time, id) > imleç.
func (s *Store) ListFlights(ctx context.Context, f FlightFilter, afterAt *time.Time, afterID int64, limit int) ([]Flight, error) {
	rows, err := s.Pool.Query(ctx, flightSelect+`
	 WHERE ($1 = '' OR f.op_date = $1::date)
	   AND ($2 = '' OR f.direction = $2)
	   AND ($3 = '' OR f.airline_code = $3)
	   AND ($4 = '' OR f.tail_number = $4)
	   AND ($5 = '' OR f.status = $5)
	   AND ($6::timestamptz IS NULL OR (f.sched_time, f.id) > ($6, $7))
	 ORDER BY f.sched_time, f.id
	 LIMIT $8`, f.Date, f.Direction, f.Airline, f.Tail, f.Status, afterAt, afterID, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Flight, error) { return scanFlight(r) })
}

func (s *Store) GetFlight(ctx context.Context, id int64) (Flight, error) {
	f, err := scanFlight(s.Pool.QueryRow(ctx, flightSelect+` WHERE f.id = $1`, id))
	return f, notFound(err)
}

type NewFlight struct {
	FlightNo     string    `json:"flight_no"`
	Direction    string    `json:"direction"`
	TailNumber   string    `json:"tail_number"`
	OtherAirport string    `json:"other_airport"`
	SchedTime    time.Time `json:"sched_time"`
}

func (s *Store) CreateFlight(ctx context.Context, q Q, nf NewFlight, opDate string) (int64, error) {
	var id int64
	err := q.QueryRow(ctx, `
		INSERT INTO flights(flight_no, airline_code, direction, tail_number, other_airport, op_date, sched_time, est_time)
		SELECT $1, a.airline_code, $2, a.tail_number, $4, $5::date, $6, $6
		  FROM aircraft a WHERE a.tail_number = $3
		RETURNING id`, nf.FlightNo, nf.Direction, nf.TailNumber, nf.OtherAirport, opDate, nf.SchedTime).Scan(&id)
	return id, notFound(err)
}

// BuildTurnarounds henüz turnaround'a bağlanmamış uçuşları uçak bazında eşler:
// bir gelişi aynı uçağın sonraki ilk kalkışıyla birleştirir. Eşi olmayan
// gelişler (gece kalan) ve kalkışlar (sabah ilk) tek uçuşlu turnaround olur.
func (s *Store) BuildTurnarounds(ctx context.Context, q Q, opDate string) (int, error) {
	tag, err := q.Exec(ctx, `
	WITH free AS (
		SELECT f.*, at.min_turnaround_min, at.target_turnaround_min, at.size_class
		  FROM flights f
		  JOIN aircraft a ON a.tail_number = f.tail_number
		  JOIN aircraft_types at ON at.code = a.type_code
		 WHERE f.op_date = $1::date AND f.status <> 'CANCELLED'
		   AND NOT EXISTS (SELECT 1 FROM turnarounds t WHERE t.arr_flight_id = f.id OR t.dep_flight_id = f.id)
	), ordered AS (
		SELECT *, lead(id) OVER w AS next_id, lead(direction) OVER w AS next_dir, lead(est_time) OVER w AS next_time,
		          lag(direction) OVER w AS prev_dir
		  FROM free WINDOW w AS (PARTITION BY tail_number ORDER BY est_time, id)
	), pairs AS (
		SELECT tail_number, id AS arr_id, next_id AS dep_id, est_time AS on_block, next_time AS off_block, size_class,
		       target_turnaround_min
		  FROM ordered WHERE direction = 'ARR' AND next_dir = 'DEP'
		UNION ALL
		SELECT tail_number, id, NULL, est_time, est_time + make_interval(mins => target_turnaround_min), size_class,
		       target_turnaround_min
		  FROM ordered WHERE direction = 'ARR' AND (next_dir IS NULL OR next_dir = 'ARR')
		UNION ALL
		-- gece burada kalan uçağın ilk kalkışı: en az 2 saatlik hazırlık penceresi
		SELECT tail_number, NULL, id, est_time - make_interval(mins => greatest(120, target_turnaround_min)), est_time, size_class,
		       target_turnaround_min
		  FROM ordered WHERE direction = 'DEP' AND (prev_dir IS NULL OR prev_dir = 'DEP')
	)
	INSERT INTO turnarounds(tail_number, arr_flight_id, dep_flight_id, op_date, on_block, off_block, needs_bridge)
	SELECT tail_number, arr_id, dep_id, $1::date, on_block, greatest(off_block, on_block + interval '1 minute'),
	       size_class >= 'C' AND (hashtext(tail_number || coalesce(arr_id, dep_id)::text) % 3 = 0)
	  FROM pairs`, opDate)
	if err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}

// ---------- referans veriler ----------

type Stand struct {
	ID         int      `json:"id"`
	Code       string   `json:"code"`
	MaxSize    string   `json:"max_size_class"`
	HasBridge  bool     `json:"has_bridge"`
	GateID     *int     `json:"gate_id,omitempty"`
	GateCode   *string  `json:"gate_code,omitempty"`
	Active     bool     `json:"active"`
	Compatible []string `json:"compatible_types"`
}

func (s *Store) ListStands(ctx context.Context, typeCode string) ([]Stand, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT s.id, s.code, s.max_size_class, s.has_bridge, s.gate_id, g.code, s.active,
		       coalesce(array_agg(at.code ORDER BY at.code) FILTER (WHERE at.code IS NOT NULL), '{}')
		  FROM stands s
		  LEFT JOIN gates g ON g.id = s.gate_id
		  LEFT JOIN aircraft_types at ON at.size_class <= s.max_size_class
		 GROUP BY s.id, g.code
		HAVING $1 = '' OR $1 = ANY(array_agg(at.code))
		 ORDER BY regexp_replace(s.code, '[0-9]', '', 'g'), nullif(regexp_replace(s.code, '[^0-9]', '', 'g'), '')::int`, typeCode)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Stand, error) {
		var st Stand
		err := r.Scan(&st.ID, &st.Code, &st.MaxSize, &st.HasBridge, &st.GateID, &st.GateCode, &st.Active, &st.Compatible)
		return st, err
	})
}

func (s *Store) SetStandActive(ctx context.Context, q Q, id int, active bool) (bool, error) {
	var old bool
	err := q.QueryRow(ctx, `UPDATE stands n SET active=$2 FROM stands o WHERE n.id=o.id AND n.id=$1 RETURNING o.active`, id, active).Scan(&old)
	return old, notFound(err)
}

type Gate struct {
	ID       int    `json:"id"`
	Code     string `json:"code"`
	Terminal string `json:"terminal"`
	IsBus    bool   `json:"is_bus"`
}

func (s *Store) ListGates(ctx context.Context) ([]Gate, error) {
	rows, err := s.Pool.Query(ctx, `SELECT id, code, terminal, is_bus FROM gates ORDER BY code`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[Gate])
}

type AircraftType struct {
	Code         string  `json:"code"`
	Name         string  `json:"name"`
	SizeClass    string  `json:"size_class"`
	WingspanM    float64 `json:"wingspan_m"`
	MinTurnMin   int     `json:"min_turnaround_min"`
	TargetTurnMn int     `json:"target_turnaround_min"`
}

func (s *Store) ListAircraftTypes(ctx context.Context) ([]AircraftType, error) {
	rows, err := s.Pool.Query(ctx, `SELECT code, name, size_class, wingspan_m::float8, min_turnaround_min, target_turnaround_min
		FROM aircraft_types ORDER BY size_class, code`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[AircraftType])
}
