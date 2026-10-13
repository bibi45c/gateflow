-- Roles and schemas for the local database. Postgres runs this as the superuser when
-- the compose volume is first created, and pkg/pgtest runs it in every test database,
-- so it must be safe to run again and concurrently. Each service logs in with its own
-- role and owns only its own schema; the other service's schema is closed to it.
-- The passwords are local-only test values, never used outside this stack.

-- Roles are shared by every database on the server.
DO $$
BEGIN
  CREATE ROLE access LOGIN PASSWORD 'access-local-only';
EXCEPTION WHEN duplicate_object OR unique_violation THEN
  NULL;
END
$$;

DO $$
BEGIN
  CREATE ROLE metering LOGIN PASSWORD 'metering-local-only';
EXCEPTION WHEN duplicate_object OR unique_violation THEN
  NULL;
END
$$;

-- Only the service roles and the superuser may connect to this database.
DO $$
BEGIN
  EXECUTE format('REVOKE ALL ON DATABASE %I FROM PUBLIC', current_database());
  EXECUTE format('GRANT CONNECT ON DATABASE %I TO access, metering', current_database());
END
$$;

-- No shared tables: nobody but the superuser may use the public schema.
REVOKE ALL ON SCHEMA public FROM PUBLIC;

CREATE SCHEMA IF NOT EXISTS access AUTHORIZATION access;
CREATE SCHEMA IF NOT EXISTS accounting AUTHORIZATION metering;
REVOKE ALL ON SCHEMA access, accounting FROM PUBLIC;
