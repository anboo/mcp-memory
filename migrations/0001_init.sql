-- 0001_init.sql: local memory index (SQLite, FTS5, sqlite-vec).
--
-- Applied by internal/migrate inside a transaction. The schema version is
-- tracked in PRAGMA user_version. The vec0 vector table is created separately
-- by Go code because its dimension depends on configuration; see
-- migrate.EnsureVectorTable.

-- Key/value metadata: embed dimension, embed model, and any future state.
CREATE TABLE IF NOT EXISTS meta (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

-- One row per indexed OpenCode part.
CREATE TABLE IF NOT EXISTS chunks (
    id           TEXT PRIMARY KEY,
    session_id   TEXT NOT NULL,
    project_id   TEXT,
    project_path TEXT NOT NULL,
    message_id   TEXT,
    msg_role     TEXT,
    part_type    TEXT NOT NULL,
    tool         TEXT,
    command      TEXT,
    content      TEXT NOT NULL,
    snippet      TEXT NOT NULL,
    files        TEXT,
    position     INTEGER NOT NULL,
    time_created INTEGER NOT NULL,
    time_updated INTEGER NOT NULL,
    truncated    INTEGER NOT NULL DEFAULT 0,
    -- embed_text is the exact text sent to the embedder. It stays NULL for
    -- part types that are not embedded (patch, reasoning).
    embed_text   TEXT,
    -- stemmed is a Snowball-russian copy of content for the second FTS table.
    stemmed      TEXT NOT NULL DEFAULT '',
    -- embedded is 1 once a vector exists in vec_chunks.
    embedded     INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX IF NOT EXISTS chunks_session_idx ON chunks(session_id);
CREATE INDEX IF NOT EXISTS chunks_project_idx
    ON chunks(project_path, part_type, time_created);
CREATE INDEX IF NOT EXISTS chunks_pending_idx
    ON chunks(embedded) WHERE embed_text IS NOT NULL;

-- Raw full-text index over content.
CREATE VIRTUAL TABLE IF NOT EXISTS fts_unicode USING fts5(
    content,
    content='chunks',
    content_rowid='rowid',
    tokenize='unicode61'
);

-- Stemmed full-text index over the Snowball-russian copy.
CREATE VIRTUAL TABLE IF NOT EXISTS fts_stem USING fts5(
    stemmed,
    content='chunks',
    content_rowid='rowid',
    tokenize='unicode61'
);

-- Keep the external-content FTS tables in sync with chunks.
CREATE TRIGGER IF NOT EXISTS chunks_ai AFTER INSERT ON chunks BEGIN
    INSERT INTO fts_unicode(rowid, content) VALUES (new.rowid, new.content);
    INSERT INTO fts_stem(rowid, stemmed) VALUES (new.rowid, new.stemmed);
END;

CREATE TRIGGER IF NOT EXISTS chunks_ad AFTER DELETE ON chunks BEGIN
    INSERT INTO fts_unicode(fts_unicode, rowid, content)
        VALUES ('delete', old.rowid, old.content);
    INSERT INTO fts_stem(fts_stem, rowid, stemmed)
        VALUES ('delete', old.rowid, old.stemmed);
END;

CREATE TRIGGER IF NOT EXISTS chunks_au AFTER UPDATE ON chunks BEGIN
    INSERT INTO fts_unicode(fts_unicode, rowid, content)
        VALUES ('delete', old.rowid, old.content);
    INSERT INTO fts_stem(fts_stem, rowid, stemmed)
        VALUES ('delete', old.rowid, old.stemmed);
    INSERT INTO fts_unicode(rowid, content) VALUES (new.rowid, new.content);
    INSERT INTO fts_stem(rowid, stemmed) VALUES (new.rowid, new.stemmed);
END;

-- Session metadata for status output and for mapping a hit back to a project.
CREATE TABLE IF NOT EXISTS sessions (
    id            TEXT PRIMARY KEY,
    project_id    TEXT,
    project_path  TEXT,
    title         TEXT,
    agent         TEXT,
    model         TEXT,
    directory     TEXT,
    time_created  INTEGER,
    time_updated  INTEGER,
    compacted     INTEGER NOT NULL DEFAULT 0,
    tail_start_id TEXT,
    parts_count   INTEGER
);

-- Per-session sync status: pending | indexed | error.
CREATE TABLE IF NOT EXISTS sync_state (
    session_id        TEXT PRIMARY KEY,
    last_time_updated INTEGER NOT NULL,
    status            TEXT NOT NULL,
    error             TEXT,
    time_processed    INTEGER
);

-- Single-row lease so at most one process writes the index at a time.
CREATE TABLE IF NOT EXISTS sync_lease (
    id         INTEGER PRIMARY KEY CHECK (id = 1),
    owner      TEXT,
    expires_at INTEGER
);

INSERT OR IGNORE INTO sync_lease(id, owner, expires_at) VALUES (1, NULL, 0);
