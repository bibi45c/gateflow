-- Wallets, reservations and the ledger. Amounts and prices are integer micro-units
-- (1 micro-unit = 0.000001 USD); prices are per token. pkg/pgmigrate runs this file
-- with the search path set to the accounting schema.

-- One wallet per tenant. held is the sum still reserved by open reservations, so the
-- available balance is balance - held, and it never goes negative.
CREATE TABLE wallets (
    tenant_id  text PRIMARY KEY,
    balance    bigint NOT NULL,
    held       bigint NOT NULL DEFAULT 0,
    updated_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT wallets_held_check CHECK (held >= 0),
    CONSTRAINT wallets_available_check CHECK (balance - held >= 0)
);

-- One reservation per call attempt. The prices and bounds are those fetched at
-- reserve; the settlement columns stay null until the row is settled. Token counts
-- are kept as sent, so a settlement without counts keeps them null.
CREATE TABLE reservations (
    reservation_id    text PRIMARY KEY,
    call_id           text NOT NULL,
    attempt_id        integer NOT NULL,
    tenant_id         text NOT NULL REFERENCES wallets (tenant_id),
    price_version_id  text NOT NULL,
    input_price       bigint NOT NULL,
    output_price      bigint NOT NULL,
    max_input_tokens  bigint NOT NULL,
    max_output_tokens bigint NOT NULL,
    max_cost          bigint NOT NULL,
    status            text NOT NULL DEFAULT 'reserved',
    -- Digest of the reserve request content, compared when a reserve is replayed.
    reserve_digest    bytea NOT NULL,
    input_tokens      bigint,
    output_tokens     bigint,
    outcome           text,
    charged           bigint,
    -- Digest of the settle request content, compared when a settle is replayed.
    settle_digest     bytea,
    settled_at        timestamptz,
    created_at        timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT reservations_call_attempt_key UNIQUE (call_id, attempt_id),
    CONSTRAINT reservations_status_check CHECK (status IN ('reserved', 'settled')),
    CONSTRAINT reservations_outcome_check CHECK (outcome IN
        ('completed', 'upstream_error', 'client_cancelled', 'interrupted', 'not_dispatched')),
    CONSTRAINT reservations_nonnegative_check CHECK (
        input_price >= 0 AND output_price >= 0
        AND max_input_tokens >= 0 AND max_output_tokens >= 0 AND max_cost >= 0
        AND input_tokens >= 0 AND output_tokens >= 0 AND charged >= 0),
    CONSTRAINT reservations_charged_check CHECK (charged <= max_cost),
    CONSTRAINT reservations_reserved_check CHECK (status <> 'reserved' OR (
        input_tokens IS NULL AND output_tokens IS NULL AND outcome IS NULL
        AND charged IS NULL AND settle_digest IS NULL AND settled_at IS NULL)),
    CONSTRAINT reservations_settled_check CHECK (status <> 'settled' OR (
        outcome IS NOT NULL AND charged IS NOT NULL AND settled_at IS NOT NULL))
);

-- At most one entry per reservation, with the call's identity and tenant copied in.
CREATE TABLE ledger_entries (
    ledger_entry_id text PRIMARY KEY,
    reservation_id  text NOT NULL UNIQUE REFERENCES reservations (reservation_id),
    call_id         text NOT NULL,
    attempt_id      integer NOT NULL,
    tenant_id       text NOT NULL,
    amount          bigint NOT NULL,
    input_tokens    bigint,
    output_tokens   bigint,
    outcome         text NOT NULL,
    created_at      timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT ledger_entries_nonnegative_check CHECK (
        amount >= 0 AND input_tokens >= 0 AND output_tokens >= 0),
    CONSTRAINT ledger_entries_outcome_check CHECK (outcome IN
        ('completed', 'upstream_error', 'client_cancelled', 'interrupted', 'not_dispatched'))
);

-- The ledger only grows: a correction is a new entry, never an edit or a removal.
CREATE FUNCTION reject_ledger_change() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'ledger_entries is append-only: % is not allowed', TG_OP;
END
$$;

CREATE TRIGGER ledger_entries_append_only
    BEFORE UPDATE OR DELETE ON ledger_entries
    FOR EACH ROW EXECUTE FUNCTION reject_ledger_change();
