-- Variables are read from the environment, never interpolated into shell commands.
\getenv access_password ACCESS_PASSWORD
\getenv metering_password METERING_PASSWORD
BEGIN;
SELECT pg_advisory_xact_lock(714071);
SELECT 'CREATE ROLE access LOGIN' WHERE NOT EXISTS (SELECT FROM pg_roles WHERE rolname='access') \gexec
SELECT 'CREATE ROLE metering LOGIN' WHERE NOT EXISTS (SELECT FROM pg_roles WHERE rolname='metering') \gexec
-- The RDS master role is not a superuser; PG16+ requires SET on a role to use it in AUTHORIZATION.
GRANT access, metering TO CURRENT_USER;
-- Do not echo password-bearing statements; psql reports only ALTER ROLE.
SELECT format('ALTER ROLE access PASSWORD %L', :'access_password') \gexec
SELECT format('ALTER ROLE metering PASSWORD %L', :'metering_password') \gexec
SELECT format('REVOKE ALL ON DATABASE %I FROM PUBLIC', current_database()) \gexec
SELECT format('GRANT CONNECT ON DATABASE %I TO access, metering', current_database()) \gexec
REVOKE ALL ON SCHEMA public FROM PUBLIC;
CREATE SCHEMA IF NOT EXISTS access AUTHORIZATION access;
CREATE SCHEMA IF NOT EXISTS accounting AUTHORIZATION metering;
REVOKE ALL ON SCHEMA access, accounting FROM PUBLIC;
COMMIT;
