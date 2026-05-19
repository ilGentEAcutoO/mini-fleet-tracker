-- 000001_init.up.sql — initial schema for Mini Fleet Tracker
-- Generated app-side IDs are UUID v7 (lexicographically sortable by creation time).
-- Timestamps are unix-ms (INTEGER) for portability across SQLite/D1 and Go time.UnixMilli.

CREATE TABLE drivers (
    id TEXT PRIMARY KEY,
    email TEXT UNIQUE NOT NULL,
    password_hash TEXT NOT NULL,
    name TEXT NOT NULL,
    role TEXT NOT NULL CHECK (role IN ('driver', 'manager')),
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
) STRICT;

CREATE TABLE vehicles (
    id TEXT PRIMARY KEY,
    plate_number TEXT UNIQUE NOT NULL,
    model TEXT,
    driver_id TEXT REFERENCES drivers(id) ON DELETE SET NULL,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
) STRICT;

-- positions intentionally omits STRICT because INTEGER PRIMARY KEY AUTOINCREMENT
-- is an alias for ROWID with its own well-defined semantics in SQLite; mixing it
-- with STRICT mode is allowed but offers no benefit and forces the explicit type
-- list to handle the AUTOINCREMENT rowid alias, which only adds friction.
CREATE TABLE positions (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    vehicle_id TEXT NOT NULL REFERENCES vehicles(id) ON DELETE CASCADE,
    lat REAL NOT NULL,
    lng REAL NOT NULL,
    speed_kmh REAL,
    recorded_at INTEGER NOT NULL,
    created_at INTEGER NOT NULL
);
CREATE INDEX idx_positions_vehicle_time ON positions(vehicle_id, recorded_at DESC);

CREATE TABLE geofences (
    id TEXT PRIMARY KEY,
    vehicle_id TEXT NOT NULL REFERENCES vehicles(id) ON DELETE CASCADE,
    center_lat REAL NOT NULL,
    center_lng REAL NOT NULL,
    radius_m INTEGER NOT NULL,
    created_at INTEGER NOT NULL
) STRICT;
