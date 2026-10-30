-- Temel referans tabloları: kullanıcılar, havayolları, uçak tipleri, stand/kapı envanteri.

CREATE EXTENSION IF NOT EXISTS btree_gist;

CREATE TYPE user_role AS ENUM (
    'ops_controller',   -- Operasyon kontrolörü
    'ground_chief',     -- Yer hizmetleri şefi
    'crew_lead',        -- Ekip lideri (mobil)
    'airline_rep',      -- Havayolu temsilcisi
    'management'        -- Yönetim
);

CREATE TABLE airlines (
    code  text PRIMARY KEY CHECK (code ~ '^[A-Z0-9]{2}$'),
    name  text NOT NULL
);

-- ICAO havaalanı referans kodu harfi: B < C < D < E < F (kanat açıklığına göre).
CREATE TABLE aircraft_types (
    code                  text PRIMARY KEY CHECK (code ~ '^[A-Z0-9]{3,4}$'),
    name                  text NOT NULL,
    size_class            char(1) NOT NULL CHECK (size_class IN ('B','C','D','E','F')),
    wingspan_m            numeric(5,2) NOT NULL CHECK (wingspan_m > 0),
    min_turnaround_min    int NOT NULL CHECK (min_turnaround_min BETWEEN 15 AND 300),
    target_turnaround_min int NOT NULL CHECK (target_turnaround_min >= min_turnaround_min)
);

CREATE TABLE aircraft (
    tail_number   text PRIMARY KEY CHECK (tail_number ~ '^[A-Z0-9-]{4,10}$'),
    airline_code  text NOT NULL REFERENCES airlines(code),
    type_code     text NOT NULL REFERENCES aircraft_types(code)
);
CREATE INDEX aircraft_airline_idx ON aircraft(airline_code);

CREATE TABLE gates (
    id        serial PRIMARY KEY,
    code      text NOT NULL UNIQUE,
    terminal  text NOT NULL,
    is_bus    boolean NOT NULL DEFAULT false   -- uzak standlara otobüsle yolcu taşıyan kapı
);

CREATE TABLE stands (
    id              serial PRIMARY KEY,
    code            text NOT NULL UNIQUE,
    max_size_class  char(1) NOT NULL CHECK (max_size_class IN ('B','C','D','E','F')),
    has_bridge      boolean NOT NULL DEFAULT false,
    gate_id         int REFERENCES gates(id),
    active          boolean NOT NULL DEFAULT true,
    -- köprülü stand mutlaka bir kapıya bağlı olmalı
    CONSTRAINT bridge_needs_gate CHECK (NOT has_bridge OR gate_id IS NOT NULL)
);

CREATE TABLE users (
    id             bigserial PRIMARY KEY,
    email          text NOT NULL,
    full_name      text NOT NULL,
    password_hash  text NOT NULL,
    role           user_role NOT NULL,
    airline_code   text REFERENCES airlines(code),
    active         boolean NOT NULL DEFAULT true,
    created_at     timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT airline_rep_has_airline CHECK (role <> 'airline_rep' OR airline_code IS NOT NULL)
);
CREATE UNIQUE INDEX users_email_uq ON users (lower(email));

CREATE TABLE refresh_tokens (
    token_hash  text PRIMARY KEY,
    user_id     bigint NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    expires_at  timestamptz NOT NULL,
    revoked_at  timestamptz,
    created_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX refresh_tokens_user_idx ON refresh_tokens(user_id);

-- Eklemeli-yalnız denetim kaydı
CREATE TABLE audit_log (
    id          bigserial PRIMARY KEY,
    at          timestamptz NOT NULL DEFAULT now(),
    actor_id    bigint,
    actor_role  text,
    action      text NOT NULL,
    entity      text NOT NULL,
    entity_id   text NOT NULL,
    old_value   jsonb,
    new_value   jsonb
);
CREATE INDEX audit_log_entity_idx ON audit_log(entity, entity_id, at DESC);

CREATE FUNCTION audit_log_immutable() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'audit_log eklemeli-yalnızdır (% engellendi)', TG_OP
        USING ERRCODE = 'insufficient_privilege';
END $$;

CREATE TRIGGER audit_log_no_update BEFORE UPDATE OR DELETE ON audit_log
    FOR EACH ROW EXECUTE FUNCTION audit_log_immutable();

-- İdempotans anahtarları: aynı kullanıcı + anahtar = aynı yanıt
CREATE TABLE idempotency_keys (
    user_id       bigint NOT NULL,
    key           text NOT NULL CHECK (length(key) BETWEEN 8 AND 128),
    method        text NOT NULL,
    path          text NOT NULL,
    request_hash  text NOT NULL,
    status_code   int,
    response_body bytea,
    created_at    timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, key)
);
CREATE INDEX idempotency_keys_created_idx ON idempotency_keys(created_at);
