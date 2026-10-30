package store

import (
	"context"
	"time"
)

type Dashboard struct {
	OpDate        string         `json:"op_date"`
	Flights       int            `json:"flights"`
	Arrivals      int            `json:"arrivals"`
	Departures    int            `json:"departures"`
	Delayed       int            `json:"delayed"`
	Cancelled     int            `json:"cancelled"`
	AvgDelayMin   float64        `json:"avg_delay_min"`
	Turnarounds   int            `json:"turnarounds"`
	Allocated     int            `json:"allocated"`
	OnStandNow    int            `json:"on_stand_now"`
	StandsActive  int            `json:"stands_active"`
	Tasks         map[string]int `json:"tasks"`
	Feed          map[string]int `json:"feed"`
	PendingReplan int            `json:"pending_replans"`
	GeneratedAt   time.Time      `json:"generated_at"`
}

func (s *Store) Dashboard(ctx context.Context, opDate string) (Dashboard, error) {
	d := Dashboard{OpDate: opDate, Tasks: map[string]int{}, GeneratedAt: time.Now().UTC()}
	err := s.Pool.QueryRow(ctx, `
		SELECT count(*), count(*) FILTER (WHERE direction='ARR'), count(*) FILTER (WHERE direction='DEP'),
		       count(*) FILTER (WHERE status='DELAYED'), count(*) FILTER (WHERE status='CANCELLED'),
		       coalesce(round(avg(greatest(0, extract(epoch FROM est_time - sched_time)/60)) FILTER (WHERE status <> 'CANCELLED'), 1), 0)::float8
		  FROM flights WHERE op_date=$1::date`, opDate).
		Scan(&d.Flights, &d.Arrivals, &d.Departures, &d.Delayed, &d.Cancelled, &d.AvgDelayMin)
	if err != nil {
		return d, err
	}
	err = s.Pool.QueryRow(ctx, `
		SELECT count(*), count(sa.id), count(sa.id) FILTER (WHERE now() <@ tstzrange(t.on_block, t.off_block)),
		       (SELECT count(*) FROM stands WHERE active)
		  FROM turnarounds t LEFT JOIN stand_allocations sa ON sa.turnaround_id=t.id AND sa.active
		 WHERE t.op_date=$1::date AND NOT t.synthetic`, opDate).Scan(&d.Turnarounds, &d.Allocated, &d.OnStandNow, &d.StandsActive)
	if err != nil {
		return d, err
	}
	rows, err := s.Pool.Query(ctx, `SELECT k.status, count(*) FROM tasks k JOIN turnarounds t ON t.id=k.turnaround_id
		WHERE t.op_date=$1::date GROUP BY k.status`, opDate)
	if err != nil {
		return d, err
	}
	for rows.Next() {
		var st string
		var n int
		if err := rows.Scan(&st, &n); err != nil {
			return d, err
		}
		d.Tasks[st] = n
	}
	if d.Feed, err = s.FeedStats(ctx, opDate); err != nil {
		return d, err
	}
	pend, err := s.PendingReplans(ctx, opDate)
	d.PendingReplan = len(pend)
	return d, err
}

type StandSlot struct {
	StandCode string    `json:"stand_code"`
	Tail      string    `json:"tail_number"`
	Flights   string    `json:"flights"`
	Airline   string    `json:"airline_code"`
	OnBlock   time.Time `json:"on_block"`
	OffBlock  time.Time `json:"off_block"`
}

// Gantt stand doluluk zaman çizelgesi için satırlar.
func (s *Store) Gantt(ctx context.Context, opDate, airline string) ([]StandSlot, error) {
	allocs, err := s.ListAllocations(ctx, opDate, airline)
	if err != nil {
		return nil, err
	}
	out := make([]StandSlot, 0, len(allocs))
	for _, a := range allocs {
		fl := ""
		if a.ArrFlight != nil {
			fl = *a.ArrFlight
		}
		if a.DepFlight != nil {
			if fl != "" {
				fl += " / "
			}
			fl += *a.DepFlight
		}
		out = append(out, StandSlot{StandCode: a.StandCode, Tail: a.TailNumber, Flights: fl, Airline: a.AirlineCode,
			OnBlock: a.OnBlock, OffBlock: a.OffBlock})
	}
	return out, nil
}
