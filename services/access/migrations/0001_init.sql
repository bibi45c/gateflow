-- Tenants, gateway keys, channels and price versions. pkg/pgmigrate runs this with the
-- search path set to the access schema, as the access role, which then owns every
-- table. The channel rules are repeated in Go in services/access/internal/channel.

CREATE TABLE tenants (
  id         text PRIMARY KEY,
  name       text NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now()
);

-- Prices in micro-units per token. A version never changes once written, so every
-- charge made with it can be recomputed later; new prices need a new version.
CREATE TABLE price_versions (
  id                     text PRIMARY KEY,
  input_micro_per_token  bigint NOT NULL CHECK (input_micro_per_token BETWEEN 0 AND 1000000),
  output_micro_per_token bigint NOT NULL CHECK (output_micro_per_token BETWEEN 0 AND 1000000),
  created_at             timestamptz NOT NULL DEFAULT now()
);

CREATE FUNCTION reject_price_version_change() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  RAISE EXCEPTION 'price versions are immutable: % of % refused', TG_OP, OLD.id;
END
$$;

CREATE TRIGGER price_versions_immutable
  BEFORE UPDATE OR DELETE ON price_versions
  FOR EACH ROW EXECUTE FUNCTION reject_price_version_change();

-- base_url is http or https, a host and an optional port, with no path, query or
-- trailing slash. credential_env names the gateway's variable holding the upstream
-- credential, never the credential itself; a credential is only sent over https.
CREATE TABLE channels (
  id             text PRIMARY KEY,
  protocol       text NOT NULL CHECK (protocol = 'anthropic-messages'),
  base_url       text NOT NULL CHECK (base_url ~ '^https?://[A-Za-z0-9.-]+(:[0-9]{1,5})?$'),
  credential_env text NOT NULL DEFAULT ''
    CHECK (credential_env = '' OR credential_env ~ '^UPSTREAM_CREDENTIAL_[A-Z0-9_]+$'),
  created_at     timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT channels_credential_needs_https
    CHECK (credential_env = '' OR base_url ~ '^https://')
);

-- model_pattern is an exact model name, or a prefix ending in *.
CREATE TABLE channel_models (
  channel_id    text NOT NULL REFERENCES channels (id),
  model_pattern text NOT NULL CHECK (model_pattern ~ '^[^*]+\*?$'),
  PRIMARY KEY (channel_id, model_pattern)
);

-- The raw key is never stored: key_hmac is HMAC-SHA256(ACCESS_KEY_PEPPER, key).
CREATE TABLE api_keys (
  id               text PRIMARY KEY,
  tenant_id        text NOT NULL REFERENCES tenants (id),
  key_hmac         bytea NOT NULL UNIQUE CHECK (length(key_hmac) = 32),
  price_version_id text NOT NULL REFERENCES price_versions (id),
  revoked_at       timestamptz,
  created_at       timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE api_key_channels (
  key_id     text NOT NULL REFERENCES api_keys (id),
  channel_id text NOT NULL REFERENCES channels (id),
  PRIMARY KEY (key_id, channel_id)
);
