-- Uçuşlar, turnaround (yer dönüşü) ve stand tahsisi.

CREATE TABLE flights (
    id            bigserial PRIMARY KEY,
    flight_no     text NOT NULL CHECK (flight_no ~ '^[A-Z0-9]{2}[0-9]{1,4}[A-Z]?$'),
    airline_code  text NOT NULL REFERENCES airlines(code),
    direction     char(3) NOT NULL CHECK (direction IN ('ARR','DEP')),
    tail_number   text NOT NULL REFERENCES aircraft(tail_number),
    other_airport text NOT NULL CHECK (other_airport ~ '^[A-Z]{3}$'),
    op_date       date NOT NULL,
    sched_time    timestamptz NOT NULL,
    est_time      timestamptz NOT NULL,
    actual_time   timestamptz,
    status        text NOT NULL DEFAULT 'SCHEDULED'
                  CHECK (status IN ('SCHEDULED','DELAYED','CANCELLED','LANDED','DEPARTED')),
    -- uçuş bilgi beslemesinden uygulanan son sıra numarası (sırasız/tekrarlı olaylara karşı)
    feed_seq      bigint NOT NULL DEFAULT 0,
    updated_at    timestamptz NOT NULL DEFAULT now(),
    UNIQUE (flight_no, op_date, direction)
);
CREATE INDEX flights_sched_idx ON flights (sched_time, id);
CREATE INDEX flights_tail_idx ON flights (tail_number, sched_time);
CREATE INDEX flights_airline_sched_idx ON flights (airline_code, sched_time, id);

-- Bir uçağın bu havalimanında yerde kaldığı süre: geliş + gidiş çifti.
-- Sabah ilk kalkışlarda geliş, gece kalanlarda gidiş olmayabilir.
CREATE TABLE turnarounds (
    id                bigserial PRIMARY KEY,
    tail_number       text NOT NULL REFERENCES aircraft(tail_number),
    arr_flight_id     bigint UNIQUE REFERENCES flights(id) ON DELETE CASCADE,
    dep_flight_id     bigint UNIQUE REFERENCES flights(id) ON DELETE CASCADE,
    op_date           date NOT NULL,
    on_block          timestamptz NOT NULL,
    off_block         timestamptz NOT NULL,
    needs_bridge      boolean NOT NULL DEFAULT false,
    actual_on_block   timestamptz,
    actual_off_block  timestamptz,
    synthetic         boolean NOT NULL DEFAULT false,   -- analitik yük verisi
    CONSTRAINT has_some_flight CHECK (synthetic OR arr_flight_id IS NOT NULL OR dep_flight_id IS NOT NULL),
    CONSTRAINT block_order CHECK (off_block > on_block)
);
CREATE INDEX turnarounds_date_idx ON turnarounds (op_date, on_block);
CREATE INDEX turnarounds_tail_idx ON turnarounds (tail_number, on_block);

CREATE TABLE stand_allocations (
    id             bigserial PRIMARY KEY,
    turnaround_id  bigint NOT NULL REFERENCES turnarounds(id) ON DELETE CASCADE,
    stand_id       int NOT NULL REFERENCES stands(id),
    gate_id        int REFERENCES gates(id),
    on_block       timestamptz NOT NULL,
    off_block      timestamptz NOT NULL,
    -- standın gerçekten bloke olduğu aralık: [on_block, off_block + 15 dk)
    -- tetikleyici doldurur; EXCLUDE bu sütun üzerinden çalışır.
    blocked        tstzrange NOT NULL,
    active         boolean NOT NULL DEFAULT true,
    version        int NOT NULL DEFAULT 1,
    created_by     bigint REFERENCES users(id),
    created_at     timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT alloc_block_order CHECK (off_block > on_block),
    CONSTRAINT stand_no_overlap EXCLUDE USING gist (stand_id WITH =, blocked WITH &&) WHERE (active)
);
CREATE UNIQUE INDEX stand_allocations_one_active ON stand_allocations (turnaround_id) WHERE active;
CREATE INDEX stand_allocations_on_block_idx ON stand_allocations (on_block) WHERE active;

-- Tampon süresini ve stand/uçak uyumunu veritabanında garanti eden tetikleyici.
CREATE FUNCTION stand_allocation_guard() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    s_max   char(1);
    s_br    boolean;
    s_act   boolean;
    a_size  char(1);
    t_br    boolean;
BEGIN
    NEW.blocked := tstzrange(NEW.on_block, NEW.off_block + interval '15 minutes', '[)');

    SELECT max_size_class, has_bridge, active INTO s_max, s_br, s_act
      FROM stands WHERE id = NEW.stand_id;
    SELECT at.size_class, t.needs_bridge INTO a_size, t_br
      FROM turnarounds t
      JOIN aircraft a ON a.tail_number = t.tail_number
      JOIN aircraft_types at ON at.code = a.type_code
     WHERE t.id = NEW.turnaround_id;

    IF NEW.active THEN
        IF NOT s_act THEN
            RAISE EXCEPTION 'stand % aktif değil', NEW.stand_id USING ERRCODE = 'check_violation';
        END IF;
        IF a_size > s_max THEN
            RAISE EXCEPTION 'uçak boyutu % stand kapasitesini (%) aşıyor', a_size, s_max
                USING ERRCODE = 'check_violation';
        END IF;
        IF t_br AND NOT s_br THEN
            RAISE EXCEPTION 'turnaround köprü istiyor, stand köprüsüz' USING ERRCODE = 'check_violation';
        END IF;
    END IF;
    RETURN NEW;
END $$;

CREATE TRIGGER stand_allocation_guard_trg BEFORE INSERT OR UPDATE ON stand_allocations
    FOR EACH ROW EXECUTE FUNCTION stand_allocation_guard();

-- Her tahsis çalıştırmasının özeti ve çözülemeyen uçuşların nedenleri
CREATE TABLE allocation_runs (
    id            bigserial PRIMARY KEY,
    op_date       date NOT NULL,
    started_at    timestamptz NOT NULL DEFAULT now(),
    duration_ms   int NOT NULL,
    total         int NOT NULL,
    allocated     int NOT NULL,
    unallocated   jsonb NOT NULL DEFAULT '[]',
    created_by    bigint REFERENCES users(id)
);

-- Uçuş bilgi beslemesi (FIDS/AODB benzeri) ham olayları
CREATE TABLE feed_events (
    id           bigserial PRIMARY KEY,
    source       text NOT NULL,
    message_id   text NOT NULL,
    flight_no    text NOT NULL,
    op_date      date NOT NULL,
    direction    char(3) NOT NULL CHECK (direction IN ('ARR','DEP')),
    seq          bigint NOT NULL CHECK (seq > 0),
    kind         text NOT NULL CHECK (kind IN ('ESTIMATE','CANCEL','ACTUAL')),
    new_time     timestamptz,
    received_at  timestamptz NOT NULL DEFAULT now(),
    outcome      text NOT NULL CHECK (outcome IN ('APPLIED','STALE','DUPLICATE','UNKNOWN_FLIGHT')),
    UNIQUE (source, message_id)
);
CREATE INDEX feed_events_flight_idx ON feed_events (flight_no, op_date, direction, seq);
