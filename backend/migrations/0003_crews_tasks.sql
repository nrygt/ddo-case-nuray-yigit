-- Yer hizmeti ekipleri, belgeler, vardiyalar, ekipman ve görevler.

CREATE TABLE crews (
    id            serial PRIMARY KEY,
    name          text NOT NULL UNIQUE,
    task_type     text NOT NULL CHECK (task_type IN ('FUEL','BAGGAGE','CLEANING','BRIDGE')),
    lead_user_id  bigint UNIQUE REFERENCES users(id),
    active        boolean NOT NULL DEFAULT true
);

-- Belge / sertifika geçerliliği (ör. yakıt ikmal sertifikası)
CREATE TABLE crew_certifications (
    crew_id      int NOT NULL REFERENCES crews(id) ON DELETE CASCADE,
    cert_type    text NOT NULL CHECK (cert_type IN ('FUEL_HANDLING','BRIDGE_OPERATION','RAMP_SAFETY')),
    valid_until  date NOT NULL,
    PRIMARY KEY (crew_id, cert_type)
);

CREATE TABLE crew_shifts (
    id       bigserial PRIMARY KEY,
    crew_id  int NOT NULL REFERENCES crews(id) ON DELETE CASCADE,
    period   tstzrange NOT NULL CHECK (NOT isempty(period) AND upper(period) - lower(period) <= interval '12 hours'),
    CONSTRAINT shift_no_overlap EXCLUDE USING gist (crew_id WITH =, period WITH &&)
);

CREATE TABLE equipment (
    id      serial PRIMARY KEY,
    code    text NOT NULL UNIQUE,
    kind    text NOT NULL CHECK (kind IN ('FUEL_TRUCK','BELT_LOADER','GPU')),
    active  boolean NOT NULL DEFAULT true
);

CREATE TABLE tasks (
    id             bigserial PRIMARY KEY,
    turnaround_id  bigint NOT NULL REFERENCES turnarounds(id) ON DELETE CASCADE,
    task_type      text NOT NULL CHECK (task_type IN ('FUEL','BAGGAGE','CLEANING','BRIDGE')),
    planned_start  timestamptz NOT NULL,
    planned_end    timestamptz NOT NULL,
    planned        tstzrange GENERATED ALWAYS AS (tstzrange(planned_start, planned_end, '[)')) STORED,
    crew_id        int REFERENCES crews(id),
    equipment_id   int REFERENCES equipment(id),
    status         text NOT NULL DEFAULT 'PENDING'
                   CHECK (status IN ('PENDING','ASSIGNED','STARTED','DONE','CANCELLED')),
    started_at     timestamptz,
    finished_at    timestamptz,
    version        int NOT NULL DEFAULT 1,
    updated_at     timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT task_time_order CHECK (planned_end > planned_start),
    CONSTRAINT task_unique_type UNIQUE (turnaround_id, task_type),
    -- aynı ekip / aynı araç aynı anda iki işte olamaz
    CONSTRAINT crew_no_overlap EXCLUDE USING gist (crew_id WITH =, planned WITH &&)
        WHERE (crew_id IS NOT NULL AND status <> 'CANCELLED'),
    CONSTRAINT equipment_no_overlap EXCLUDE USING gist (equipment_id WITH =, planned WITH &&)
        WHERE (equipment_id IS NOT NULL AND status <> 'CANCELLED')
);
CREATE INDEX tasks_crew_idx ON tasks (crew_id, planned_start) WHERE crew_id IS NOT NULL;
CREATE INDEX tasks_turnaround_idx ON tasks (turnaround_id);
CREATE INDEX tasks_planned_start_idx ON tasks (planned_start, id);
