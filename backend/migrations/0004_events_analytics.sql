-- Olay kaynaklı görev geçmişi (aylık partisyon) ve turnaround analitiği.

ALTER TABLE turnarounds ADD COLUMN actual_stand_id int REFERENCES stands(id);

CREATE TABLE task_events (
    id               bigint GENERATED ALWAYS AS IDENTITY,
    task_id          bigint NOT NULL,
    turnaround_id    bigint NOT NULL,
    task_type        text NOT NULL CHECK (task_type IN ('FUEL','BAGGAGE','CLEANING','BRIDGE')),
    event_type       text NOT NULL CHECK (event_type IN ('ASSIGNED','STARTED','FINISHED','UNASSIGNED')),
    occurred_at      timestamptz NOT NULL,         -- cihazın bildirdiği zaman
    received_at      timestamptz NOT NULL DEFAULT now(),
    client_event_id  uuid NOT NULL,
    device_id        text,
    actor_id         bigint,
    PRIMARY KEY (id, occurred_at),
    -- cihaz aynı olayı tekrar gönderirse ikinci kez yazılmaz
    UNIQUE (client_event_id, occurred_at)
) PARTITION BY RANGE (occurred_at);

CREATE TABLE task_events_default PARTITION OF task_events DEFAULT;

CREATE INDEX task_events_task_idx ON task_events (task_id, occurred_at);
CREATE INDEX task_events_turnaround_idx ON task_events (turnaround_id);

-- [from, to) aralığındaki her ay için partisyon açar (varsa dokunmaz).
CREATE FUNCTION ensure_task_event_partitions(p_from date, p_to date) RETURNS int
LANGUAGE plpgsql AS $$
DECLARE
    m      date := date_trunc('month', p_from)::date;
    name   text;
    made   int := 0;
BEGIN
    WHILE m < p_to LOOP
        name := format('task_events_%s', to_char(m, 'YYYYMM'));
        IF to_regclass(name) IS NULL THEN
            EXECUTE format(
                'CREATE TABLE %I PARTITION OF task_events FOR VALUES FROM (%L) TO (%L)',
                name, m, (m + interval '1 month')::date);
            made := made + 1;
        END IF;
        m := (m + interval '1 month')::date;
    END LOOP;
    RETURN made;
END $$;

SELECT ensure_task_event_partitions((now() - interval '13 months')::date, (now() + interval '3 months')::date);

-- Veri kalitesi: tutarsız zaman damgaları
CREATE VIEW task_event_quality_issues AS
WITH per_task AS (
    SELECT task_id, turnaround_id,
           min(occurred_at) FILTER (WHERE event_type = 'STARTED')  AS started,
           max(occurred_at) FILTER (WHERE event_type = 'FINISHED') AS finished,
           count(*) FILTER (WHERE event_type = 'FINISHED')         AS finished_cnt,
           count(*) FILTER (WHERE event_type = 'STARTED')          AS started_cnt
      FROM task_events
     GROUP BY task_id, turnaround_id
)
SELECT task_id, turnaround_id, 'FINISHED_BEFORE_STARTED' AS issue
  FROM per_task WHERE finished < started
UNION ALL
SELECT task_id, turnaround_id, 'FINISHED_WITHOUT_STARTED'
  FROM per_task WHERE finished_cnt > 0 AND started_cnt = 0
UNION ALL
SELECT task_id, turnaround_id, 'TASK_LONGER_THAN_6H'
  FROM per_task WHERE finished - started > interval '6 hours'
UNION ALL
SELECT DISTINCT task_id, turnaround_id, 'OCCURRED_IN_FUTURE'
  FROM task_events WHERE occurred_at > received_at + interval '5 minutes';

-- Turnaround başına süre: ilk görev başlangıcından son görev bitişine.
-- Kalite sorunu olan turnaroundlar dışarıda bırakılır.
CREATE MATERIALIZED VIEW mv_turnaround_durations AS
WITH bad AS (
    SELECT DISTINCT turnaround_id FROM task_event_quality_issues
), spans AS (
    SELECT turnaround_id,
           min(occurred_at) FILTER (WHERE event_type = 'STARTED')  AS first_start,
           max(occurred_at) FILTER (WHERE event_type = 'FINISHED') AS last_finish
      FROM task_events
     GROUP BY turnaround_id
)
SELECT t.id                       AS turnaround_id,
       t.op_date,
       a.airline_code,
       a.type_code,
       coalesce(st.code, '-')     AS stand_code,
       at.target_turnaround_min,
       -- tam sayı saniye: pencere sıralaması numeric'e göre belirgin hızlı (README, soru 3)
       extract(epoch FROM s.last_finish - s.first_start)::int AS duration_s
  FROM spans s
  JOIN turnarounds t      ON t.id = s.turnaround_id
  JOIN aircraft a         ON a.tail_number = t.tail_number
  JOIN aircraft_types at  ON at.code = a.type_code
  LEFT JOIN stands st     ON st.id = t.actual_stand_id
 WHERE s.first_start IS NOT NULL
   AND s.last_finish > s.first_start
   AND NOT EXISTS (SELECT 1 FROM bad b WHERE b.turnaround_id = s.turnaround_id);

CREATE UNIQUE INDEX mv_turnaround_durations_pk ON mv_turnaround_durations (turnaround_id);
CREATE INDEX mv_turnaround_durations_date_idx ON mv_turnaround_durations (op_date);
