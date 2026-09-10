-- 001_init.sql: схема индекса памяти (arch-док B11).
-- Применяется вручную или через runner: psql -f migrations/001_init.sql

CREATE EXTENSION IF NOT EXISTS vector;

-- сырые чанки (raw memory)
CREATE TABLE IF NOT EXISTS chunks (
    id           text PRIMARY KEY,      -- prt_xxx или сгенерированный для text-чанков
    session_id   text NOT NULL,
    project_id   text,
    project_path text NOT NULL,
    message_id   text,
    msg_role     text,                  -- user | assistant
    part_type    text NOT NULL,         -- text | tool | patch | reasoning
    tool         text,                  -- для tool-частей
    command      text,                  -- для tool-частей
    content      text NOT NULL,         -- нормализованный текст для FTS
    snippet      text NOT NULL,         -- сниппет для выдачи
    files        text[],                -- для patch-частей
    position     integer NOT NULL,      -- координата внутри сессии
    time_created bigint NOT NULL,       -- epoch ms
    time_updated bigint NOT NULL,
    truncated    boolean NOT NULL DEFAULT false,
    embedding    vector(1024)           -- bge-m3
);

-- полнотекстовый поиск (русский)
CREATE INDEX IF NOT EXISTS chunks_fts_idx ON chunks
    USING gin (to_tsvector('russian', content));

-- векторный поиск
CREATE INDEX IF NOT EXISTS chunks_vec_idx ON chunks
    USING hnsw (embedding vector_cosine_ops);

-- фильтры
CREATE INDEX IF NOT EXISTS chunks_session_idx ON chunks (session_id);
CREATE INDEX IF NOT EXISTS chunks_project_idx ON chunks (project_path, part_type, time_created);

-- метаданные сессий
CREATE TABLE IF NOT EXISTS sessions (
    id            text PRIMARY KEY,
    project_id    text,
    project_path  text,
    title         text,
    agent         text,
    model         text,
    directory     text,
    time_created  bigint,
    time_updated  bigint,
    compacted     boolean DEFAULT false,
    tail_start_id text,
    parts_count   integer
);

-- состояние синхронизации
CREATE TABLE IF NOT EXISTS sync_state (
    session_id        text PRIMARY KEY,
    last_time_updated bigint NOT NULL,
    status            text NOT NULL,     -- pending | indexed | error
    error             text,
    time_processed    timestamptz
);

-- трассы вызовов MCP (наблюдаемость, step-4)
CREATE TABLE IF NOT EXISTS traces (
    id          bigserial PRIMARY KEY,
    tool        text NOT NULL,
    request     jsonb NOT NULL,
    results     jsonb,
    time        timestamptz NOT NULL DEFAULT now()
);