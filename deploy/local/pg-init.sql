-- Runs once, on the first start of an empty Postgres volume (docker-entrypoint-initdb.d).
-- The main database is created by POSTGRES_DB; integration tests use their own database.
CREATE DATABASE goalstream_test;
