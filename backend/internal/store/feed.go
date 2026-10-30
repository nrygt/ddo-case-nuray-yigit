package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"pistkoordinat/internal/domain/feed"
)

type FeedResult struct {
	MessageID string       `json:"message_id"`
	Outcome   feed.Outcome `json:"outcome"`
	FlightID  int64        `json:"flight_id,omitempty"`
	Status    string       `json:"status,omitempty"`
	EstTime   *time.Time   `json:"est_time,omitempty"`
}

// ApplyFeedEvent tek bir besleme olayını uygular.
//   - aynı (source, message_id) ikinci kez gelirse DUPLICATE, hiçbir şey yazılmaz
//   - uçuşun feed_seq'inden küçük/eşit seq STALE olarak kaydedilir
//   - aksi halde uçuş, koşullu UPDATE ile (feed_seq < seq) güncellenir
func (s *Store) ApplyFeedEvent(ctx context.Context, e feed.Event) (FeedResult, error) {
	res := FeedResult{MessageID: e.MessageID}
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		var nt *time.Time
		if !e.NewTime.IsZero() {
			nt = &e.NewTime
		}
		var evID int64
		err := tx.QueryRow(ctx, `INSERT INTO feed_events(source, message_id, flight_no, op_date, direction, seq, kind, new_time, outcome)
			VALUES ($1,$2,$3,$4::date,$5,$6,$7,$8,'STALE') ON CONFLICT (source, message_id) DO NOTHING RETURNING id`,
			e.Source, e.MessageID, e.FlightNo, e.OpDate, e.Direction, e.Seq, string(e.Kind), nt).Scan(&evID)
		if errors.Is(err, pgx.ErrNoRows) {
			res.Outcome = feed.Duplicate
			return nil
		}
		if err != nil {
			return err
		}
		var st feed.FlightState
		err = tx.QueryRow(ctx, `SELECT id, feed_seq, status, est_time, sched_time, actual_time FROM flights
			WHERE flight_no=$1 AND op_date=$2::date AND direction=$3 FOR UPDATE`, e.FlightNo, e.OpDate, e.Direction).
			Scan(&res.FlightID, &st.Seq, &st.Status, &st.EstTime, &st.Scheduled, scanTimePtr(&st.Actual))
		if errors.Is(err, pgx.ErrNoRows) {
			res.Outcome = feed.UnknownFlight
			_, err = tx.Exec(ctx, `UPDATE feed_events SET outcome='UNKNOWN_FLIGHT' WHERE id=$1`, evID)
			return err
		}
		if err != nil {
			return err
		}
		next, outcome := feed.Apply(st, e)
		res.Outcome = outcome
		if outcome != feed.Applied {
			res.Status, res.EstTime = st.Status, &st.EstTime
			return nil
		}
		var actual *time.Time
		if !next.Actual.IsZero() {
			actual = &next.Actual
		}
		tag, err := tx.Exec(ctx, `UPDATE flights SET feed_seq=$2, status=$3, est_time=$4, actual_time=$5, updated_at=now()
			WHERE id=$1 AND feed_seq < $2`, res.FlightID, next.Seq, next.Status, next.EstTime, actual)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			res.Outcome = feed.Stale
			return nil
		}
		res.Status, res.EstTime = next.Status, &next.EstTime
		_, err = tx.Exec(ctx, `UPDATE feed_events SET outcome='APPLIED' WHERE id=$1`, evID)
		return err
	})
	return res, err
}

type nullTime struct{ t *time.Time }

func (n nullTime) Scan(src any) error {
	if src == nil {
		*n.t = time.Time{}
		return nil
	}
	if v, ok := src.(time.Time); ok {
		*n.t = v
		return nil
	}
	return errors.New("zaman bekleniyordu")
}

func scanTimePtr(t *time.Time) nullTime { return nullTime{t} }

type PendingReplan struct {
	FlightID     int64     `json:"flight_id"`
	FlightNo     string    `json:"flight_no"`
	Direction    string    `json:"direction"`
	TurnaroundID int64     `json:"turnaround_id"`
	PlannedTime  time.Time `json:"planned_time"`
	FeedTime     time.Time `json:"feed_time"`
	DiffMin      int       `json:"diff_min"`
}

// PendingReplans beslemeden gelen tahmini saatle planlanan blok saati farklı olan uçuşlar.
func (s *Store) PendingReplans(ctx context.Context, opDate string) ([]PendingReplan, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT f.id, f.flight_no, f.direction, t.id,
		       CASE WHEN f.direction='ARR' THEN t.on_block ELSE t.off_block END AS planned,
		       f.est_time
		  FROM flights f
		  JOIN turnarounds t ON (f.direction='ARR' AND t.arr_flight_id=f.id) OR (f.direction='DEP' AND t.dep_flight_id=f.id)
		 WHERE f.op_date=$1::date AND f.status NOT IN ('CANCELLED','DEPARTED')
		   AND f.est_time <> CASE WHEN f.direction='ARR' THEN t.on_block ELSE t.off_block END
		 ORDER BY abs(extract(epoch FROM f.est_time - CASE WHEN f.direction='ARR' THEN t.on_block ELSE t.off_block END)) DESC, f.id
		 LIMIT 100`, opDate)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (PendingReplan, error) {
		var p PendingReplan
		err := r.Scan(&p.FlightID, &p.FlightNo, &p.Direction, &p.TurnaroundID, &p.PlannedTime, &p.FeedTime)
		p.DiffMin = int(p.FeedTime.Sub(p.PlannedTime).Minutes())
		return p, err
	})
}

func (s *Store) FeedStats(ctx context.Context, opDate string) (map[string]int, error) {
	rows, err := s.Pool.Query(ctx, `SELECT outcome, count(*) FROM feed_events WHERE op_date=$1::date GROUP BY outcome`, opDate)
	if err != nil {
		return nil, err
	}
	out := map[string]int{}
	for rows.Next() {
		var k string
		var n int
		if err := rows.Scan(&k, &n); err != nil {
			return nil, err
		}
		out[k] = n
	}
	return out, rows.Err()
}
