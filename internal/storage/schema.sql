CREATE TABLE IF NOT EXISTS servers (
    id              TEXT PRIMARY KEY,
    name            TEXT NOT NULL,
    host            TEXT NOT NULL,
    port            INTEGER NOT NULL,
    uuid            TEXT NOT NULL,
    encryption      TEXT NOT NULL DEFAULT 'none',
    flow            TEXT NOT NULL DEFAULT '',
    packet_encoding TEXT NOT NULL DEFAULT '',
    security        TEXT NOT NULL,
    sni             TEXT NOT NULL DEFAULT '',
    fingerprint     TEXT NOT NULL DEFAULT '',
    public_key      TEXT NOT NULL DEFAULT '',
    short_id        TEXT NOT NULL DEFAULT '',
    alpn            TEXT NOT NULL DEFAULT '',
    network         TEXT NOT NULL,
    host_header     TEXT NOT NULL DEFAULT '',
    path            TEXT NOT NULL DEFAULT '',
    service_name    TEXT NOT NULL DEFAULT '',
    mode            TEXT NOT NULL DEFAULT '',
    fragment        TEXT NOT NULL DEFAULT '',
    source_id       TEXT,
    weight          INTEGER NOT NULL DEFAULT 100,
    enabled         INTEGER NOT NULL DEFAULT 1,
    created_at      TEXT NOT NULL,
    updated_at      TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_servers_source ON servers(source_id);

CREATE TABLE IF NOT EXISTS subscriptions (
    id               TEXT PRIMARY KEY,
    name             TEXT NOT NULL,
    url              TEXT NOT NULL,
    interval_seconds INTEGER NOT NULL DEFAULT 21600,
    server_check_seconds INTEGER NOT NULL DEFAULT 30,
    enabled          INTEGER NOT NULL DEFAULT 1,
    etag             TEXT NOT NULL DEFAULT '',
    last_modified    TEXT NOT NULL DEFAULT '',
    last_checked_at  TEXT NOT NULL DEFAULT '',
    last_success_at  TEXT NOT NULL DEFAULT '',
    last_error       TEXT NOT NULL DEFAULT '',
    server_count     INTEGER NOT NULL DEFAULT 0,
    created_at       TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS users (
    id              TEXT PRIMARY KEY,
    username        TEXT NOT NULL UNIQUE,
    password_hash   TEXT NOT NULL,
    enabled         INTEGER NOT NULL DEFAULT 1,
    max_connections INTEGER NOT NULL DEFAULT 0,
    bypass_torrent  INTEGER NOT NULL DEFAULT 0,
    created_at      TEXT NOT NULL,
    last_login_at   TEXT NOT NULL DEFAULT '',
    bytes_in        INTEGER NOT NULL DEFAULT 0,
    bytes_out       INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS settings (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);
