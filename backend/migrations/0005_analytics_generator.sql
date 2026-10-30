-- T-5 yük verisi üretici. Gerçek akıştan ayrı tutulur (turnarounds.synthetic = true).
-- setseed ile aynı tohum aynı veriyi verir. ~%0.5 oranında bilerek bozuk zaman damgası
-- (bitiş < başlangıç, gelecekte olay) eklenir; veri kalitesi testleri bunları yakalamalı.

CREATE FUNCTION analytics_generate(p_events bigint, p_seed double precision DEFAULT 0.42)
RETURNS TABLE (turnarounds bigint, events bigint)
LANGUAGE plpgsql AS $$
DECLARE
    n_ta      bigint := ceil(p_events / 12.0);   -- 4 görev x 3 olay
    first_id  bigint;
BEGIN
    PERFORM setseed(p_seed);

    CREATE TEMP TABLE _gen_ta ON COMMIT DROP AS
    WITH ac AS (
        SELECT array_agg(a.tail_number ORDER BY a.tail_number) AS tails,
               array_agg(at.target_turnaround_min ORDER BY a.tail_number) AS tgts
          FROM aircraft a JOIN aircraft_types at ON at.code = a.type_code
    ), st AS (SELECT array_agg(id ORDER BY id) AS ids FROM stands), raw AS (
        SELECT g AS k, random() AS ra, random() AS rd, random() AS rm, random() AS rs, random() AS r1, random() AS r2
          FROM generate_series(1, n_ta) g
    )
    SELECT raw.k,
           ac.tails[1 + floor(raw.ra * array_length(ac.tails, 1))::int] AS tail_number,
           (current_date - 1 - (raw.rd * 364)::int) AS op_date,
           (raw.rm * 1080)::int AS start_min,
           ac.tgts[1 + floor(raw.ra * array_length(ac.tails, 1))::int] AS tgt,
           st.ids[1 + floor(raw.rs * array_length(st.ids, 1))::int] AS stand_id,
           raw.r1, raw.r2
      FROM raw, ac, st;

    SELECT coalesce(max(id), 0) + 1 INTO first_id FROM turnarounds;

    INSERT INTO turnarounds (id, tail_number, op_date, on_block, off_block, synthetic, actual_stand_id,
                             actual_on_block, actual_off_block)
    SELECT first_id + k, tail_number, op_date,
           op_date + interval '6 hours' + make_interval(mins => start_min),
           op_date + interval '6 hours' + make_interval(mins => start_min + tgt),
           true, stand_id,
           op_date + interval '6 hours' + make_interval(mins => start_min),
           op_date + interval '6 hours' + make_interval(mins => start_min + tgt)
      FROM _gen_ta;
    PERFORM setval(pg_get_serial_sequence('turnarounds','id'), (SELECT max(id) FROM turnarounds));

    -- her turnaround için 4 görev; görev süreleri hedef sürenin etrafında saçılır
    INSERT INTO task_events (task_id, turnaround_id, task_type, event_type, occurred_at, received_at, client_event_id, device_id)
    SELECT -((first_id + g.k) * 10 + t.n),
           first_id + g.k,
           (ARRAY['BRIDGE','BAGGAGE','CLEANING','FUEL'])[t.n],
           e.typ,
           CASE e.typ
             WHEN 'ASSIGNED' THEN ts.start_at - interval '30 minutes'
             WHEN 'STARTED'  THEN ts.start_at
             ELSE CASE WHEN g.r2 < 0.003 AND t.n = 2
                       THEN ts.start_at - interval '10 minutes'           -- bozuk: bitiş < başlangıç
                       ELSE ts.start_at + ts.dur END
           END,
           CASE WHEN g.r2 BETWEEN 0.003 AND 0.005 AND e.typ = 'FINISHED'
                THEN ts.start_at - interval '1 day'                        -- bozuk: alınmadan önce gerçekleşmiş (gelecek)
                ELSE ts.start_at + ts.dur + interval '1 minute' END,
           md5(g.k::text || '-' || t.n || '-' || e.typ || p_seed)::uuid,
           'sim-' || (g.k % 40)
      FROM _gen_ta g
      CROSS JOIN generate_series(1, 4) AS t(n)
      CROSS JOIN LATERAL (
          SELECT g.op_date + interval '6 hours' + make_interval(mins => g.start_min + (t.n - 1) * 5) AS start_at,
                 make_interval(mins => greatest(5, (g.tgt * (0.35 + g.r1 * 0.9))::int - (t.n - 1) * 5)) AS dur
      ) ts
      CROSS JOIN unnest(ARRAY['ASSIGNED','STARTED','FINISHED']) AS e(typ);

    turnarounds := n_ta;
    SELECT count(*) INTO events FROM task_events WHERE turnaround_id > first_id;
    RETURN NEXT;
END $$;

CREATE FUNCTION analytics_clear() RETURNS void LANGUAGE sql AS $$
    DELETE FROM task_events WHERE task_id < 0;
    DELETE FROM turnarounds WHERE synthetic;
$$;

CREATE TABLE analytics_refreshes (
    id          bigserial PRIMARY KEY,
    applied_at  timestamptz NOT NULL DEFAULT now(),
    duration_ms bigint NOT NULL
);
