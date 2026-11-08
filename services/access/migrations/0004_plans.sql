-- Plans give each tenant a request rate and a concurrency limit, which the gateway
-- enforces (issue #157). Only adds a table and a defaulted column, so the previous
-- access image still starts on this schema; every existing tenant gets the
-- unlimited plan and keeps today's behaviour.

-- rpm_limit is requests per sliding minute and max_concurrent the calls in flight at
-- once, per tenant; 0 means no limit.
CREATE TABLE plans (
  id             text PRIMARY KEY CHECK (id ~ '^[a-z0-9-]{1,32}$'),
  rpm_limit      integer NOT NULL CHECK (rpm_limit BETWEEN 0 AND 1000000),
  max_concurrent integer NOT NULL CHECK (max_concurrent BETWEEN 0 AND 100000),
  created_at     timestamptz NOT NULL DEFAULT now()
);

INSERT INTO plans (id, rpm_limit, max_concurrent) VALUES
  ('unlimited', 0, 0),
  ('free', 60, 5),
  ('standard', 600, 50),
  ('premium', 6000, 200);

ALTER TABLE tenants ADD COLUMN plan_id text NOT NULL DEFAULT 'unlimited' REFERENCES plans (id);
