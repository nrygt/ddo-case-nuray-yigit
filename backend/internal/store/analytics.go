package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

type ReportRow struct {
	Group         string  `json:"group"`
	Count         int64   `json:"count"`
	P50           float64 `json:"p50_min"`
	P90           float64 `json:"p90_min"`
	Avg           float64 `json:"avg_min"`
	Target        float64 `json:"target_min"`
	ViolationRate float64 `json:"violation_rate"`
	RankByP90     int     `json:"rank_by_p90"`
}

type Report struct {
	GroupBy     string      `json:"group_by"`
	From        string      `json:"from"`
	To          string      `json:"to"`
	Rows        []ReportRow `json:"rows"`
	Total       int64       `json:"total_turnarounds"`
	Overall     ReportRow   `json:"overall"`
	QueryMS     float64     `json:"query_ms"`
	RefreshedAt *time.Time  `json:"refreshed_at,omitempty"`
}

var groupCols = map[string]string{"type": "type_code", "airline": "airline_code", "stand": "stand_code"}

// reportSQL yüzdelikleri pencere fonksiyonlarıyla en-yakın-sıra yöntemiyle hesaplar.
//
// İlk sürüm her satıra row_number() OVER (PARTITION BY grup ORDER BY süre) veriyordu;
// 415 bin satırda ~950 ms. Süreler saniye çözünürlüğünde ama pratikte dakika katları,
// bu yüzden önce (grup, süre) histogramına indirip kümülatif toplamı pencereyle alıyoruz:
// "kümülatif adet >= ceil(p*n) olan en küçük süre", row_number = ceil(p*n) olan satırla
// aynıdır (store testi percentile_disc ile karşılaştırıyor). Sıralanan satır sayısı
// yüz binlerden birkaç bine iniyor.
const reportSQL = `
WITH base AS (
    SELECT %[1]s AS grp, duration_s, target_turnaround_min * 60 AS target_s
      FROM mv_turnaround_durations
     WHERE op_date BETWEEN $1::date AND $2::date
), hist AS (
    SELECT grp, duration_s, count(*) AS c,
           count(*) FILTER (WHERE duration_s > target_s) AS viol,
           sum(target_s) AS target_sum
      FROM base GROUP BY grp, duration_s
), cum AS (
    SELECT grp, duration_s, c, viol, target_sum,
           sum(c) OVER (PARTITION BY grp ORDER BY duration_s) AS cum_c,
           sum(c) OVER (PARTITION BY grp)                     AS n
      FROM hist
), agg AS (
    SELECT grp, n,
           min(duration_s) FILTER (WHERE cum_c >= ceil(0.5 * n)) AS p50_s,
           min(duration_s) FILTER (WHERE cum_c >= ceil(0.9 * n)) AS p90_s,
           sum(duration_s::bigint * c) / n                       AS avg_s,
           sum(target_sum) / n                                   AS target_s,
           sum(viol) / n                                         AS violation_rate
      FROM cum GROUP BY grp, n
)
SELECT grp, n::bigint, round(p50_s / 60.0, 1)::float8, round(p90_s / 60.0, 1)::float8,
       round(avg_s / 60.0, 2)::float8, round(target_s / 60.0, 1)::float8,
       round(violation_rate, 4)::float8,
       rank() OVER (ORDER BY p90_s DESC)::int
  FROM agg ORDER BY grp`

const overallSQL = `
WITH hist AS (
    SELECT duration_s, count(*) AS c,
           count(*) FILTER (WHERE duration_s > target_turnaround_min * 60) AS viol,
           sum(target_turnaround_min * 60) AS target_sum
      FROM mv_turnaround_durations WHERE op_date BETWEEN $1::date AND $2::date
     GROUP BY duration_s
), cum AS (
    SELECT *, sum(c) OVER (ORDER BY duration_s) AS cum_c, sum(c) OVER () AS n FROM hist
)
SELECT coalesce(max(n), 0)::bigint,
       coalesce(round(min(duration_s) FILTER (WHERE cum_c >= ceil(0.5 * n)) / 60.0, 1), 0)::float8,
       coalesce(round(min(duration_s) FILTER (WHERE cum_c >= ceil(0.9 * n)) / 60.0, 1), 0)::float8,
       coalesce(round(sum(duration_s::bigint * c) / max(n) / 60.0, 2), 0)::float8,
       coalesce(round(sum(target_sum) / max(n) / 60.0, 1), 0)::float8,
       coalesce(round(sum(viol) / max(n), 4), 0)::float8
  FROM cum`

func ReportQuery(groupBy string) (string, error) {
	col, ok := groupCols[groupBy]
	if !ok {
		return "", fmt.Errorf("group_by type|airline|stand olmalı")
	}
	return fmt.Sprintf(reportSQL, col), nil
}

func (s *Store) TurnaroundReport(ctx context.Context, from, to, groupBy string) (Report, error) {
	rep := Report{GroupBy: groupBy, From: from, To: to}
	q, err := ReportQuery(groupBy)
	if err != nil {
		return rep, err
	}
	start := time.Now()
	// iki sorgu aynı anlık görüntüyü görsün (arada MV yenilenirse toplamlar tutarsız olmasın)
	err = pgx.BeginTxFunc(ctx, s.Pool, pgx.TxOptions{AccessMode: pgx.ReadOnly, IsoLevel: pgx.RepeatableRead}, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, q, from, to)
		if err != nil {
			return err
		}
		rep.Rows, err = pgx.CollectRows(rows, pgx.RowToStructByPos[ReportRow])
		if err != nil {
			return err
		}
		o := &rep.Overall
		o.Group = "TÜMÜ"
		return tx.QueryRow(ctx, overallSQL, from, to).Scan(&o.Count, &o.P50, &o.P90, &o.Avg, &o.Target, &o.ViolationRate)
	})
	if err != nil {
		return rep, err
	}
	rep.Total = rep.Overall.Count
	rep.QueryMS = float64(time.Since(start).Microseconds()) / 1000
	var t *time.Time
	_ = s.Pool.QueryRow(ctx, `SELECT max(applied_at) FROM analytics_refreshes`).Scan(&t)
	rep.RefreshedAt = t
	return rep, nil
}

func (s *Store) RefreshAnalytics(ctx context.Context) (time.Duration, error) {
	start := time.Now()
	if _, err := s.Pool.Exec(ctx, `REFRESH MATERIALIZED VIEW CONCURRENTLY mv_turnaround_durations`); err != nil {
		return 0, err
	}
	// yenileme istatistikleri güncellemez; planlayıcı satır sayısını yanlış tahmin etmesin
	if _, err := s.Pool.Exec(ctx, `ANALYZE mv_turnaround_durations`); err != nil {
		return 0, err
	}
	d := time.Since(start)
	_, err := s.Pool.Exec(ctx, `INSERT INTO analytics_refreshes(duration_ms) VALUES ($1)`, d.Milliseconds())
	return d, err
}

type QualityRow struct {
	Issue string `json:"issue"`
	Tasks int64  `json:"tasks"`
}

func (s *Store) DataQuality(ctx context.Context) ([]QualityRow, error) {
	rows, err := s.Pool.Query(ctx, `SELECT issue, count(*) FROM task_event_quality_issues GROUP BY issue ORDER BY issue`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[QualityRow])
}

type GenerateResult struct {
	Turnarounds int64   `json:"turnarounds"`
	Events      int64   `json:"events"`
	Seconds     float64 `json:"seconds"`
}

func (s *Store) GenerateAnalytics(ctx context.Context, events int64, seed float64) (GenerateResult, error) {
	var r GenerateResult
	start := time.Now()
	err := s.Pool.QueryRow(ctx, `SELECT * FROM analytics_generate($1, $2)`, events, seed).Scan(&r.Turnarounds, &r.Events)
	r.Seconds = time.Since(start).Seconds()
	return r, err
}

func (s *Store) CountEvents(ctx context.Context) (int64, error) {
	var n int64
	err := s.Pool.QueryRow(ctx, `SELECT count(*) FROM task_events`).Scan(&n)
	return n, err
}
