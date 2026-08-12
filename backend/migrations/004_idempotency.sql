-- Ключи идемпотентности. Файл в диапазоне БЭК-1, но по договорённости его
-- ведёт БЭК-2: таблицу использует только middleware в internal/httpx.

CREATE TABLE IF NOT EXISTS idempotency_keys (
    user_id         text        NOT NULL,
    idempotency_key text        NOT NULL,
    request_hash    text        NOT NULL,
    status_code     integer,
    response_body   bytea,
    created_at      timestamptz NOT NULL DEFAULT now(),
    completed_at    timestamptz,

    -- Ключ уникален в пределах пользователя, а не глобально: иначе чужой
    -- Idempotency-Key дал бы прочитать чужой ответ.
    PRIMARY KEY (user_id, idempotency_key)
);

CREATE INDEX IF NOT EXISTS idempotency_keys_in_flight_idx
    ON idempotency_keys (created_at)
    WHERE status_code IS NULL;
