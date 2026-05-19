-- 000001_init.down.sql — rollback initial schema
DROP TABLE IF EXISTS geofences;
DROP INDEX IF EXISTS idx_positions_vehicle_time;
DROP TABLE IF EXISTS positions;
DROP TABLE IF EXISTS vehicles;
DROP TABLE IF EXISTS drivers;
