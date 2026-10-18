-- The model of each call and manual wallet adjustments, for the console (issue #137).
-- Only adds a nullable column and a table, so the previous metering image still
-- starts on this schema.

-- The model the gateway named at reserve; null for calls reserved before this column
-- or by a gateway that does not send it.
ALTER TABLE reservations ADD COLUMN model text;

CREATE INDEX reservations_tenant_created_idx ON reservations (tenant_id, created_at DESC);

-- A top-up (amount > 0) or a correction (amount < 0) made by an admin. adjustment_id
-- is the caller's idempotency key. balance_after is the wallet balance right after.
-- No foreign key to wallets, so deploy/aws/reset_db.py can still TRUNCATE the wallets
-- without CASCADE; the history outlives such a reset.
CREATE TABLE wallet_adjustments (
    adjustment_id text PRIMARY KEY,
    tenant_id     text NOT NULL,
    amount        bigint NOT NULL CHECK (amount <> 0),
    note          text NOT NULL DEFAULT '',
    balance_after bigint NOT NULL,
    created_at    timestamptz NOT NULL DEFAULT now()
);

CREATE FUNCTION reject_adjustment_change() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'wallet_adjustments is append-only: % is not allowed', TG_OP;
END
$$;

CREATE TRIGGER wallet_adjustments_append_only
    BEFORE UPDATE OR DELETE ON wallet_adjustments
    FOR EACH ROW EXECUTE FUNCTION reject_adjustment_change();
