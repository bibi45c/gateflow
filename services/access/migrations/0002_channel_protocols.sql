-- Channels may speak openai-responses (POST /v1/responses) as well as
-- anthropic-messages. This only widens the protocol CHECK of 0001 and changes no row,
-- so the previous access image still starts on this schema. The list is repeated in
-- Go in services/access/internal/channel.

ALTER TABLE channels DROP CONSTRAINT channels_protocol_check;

ALTER TABLE channels ADD CONSTRAINT channels_protocol_check
  CHECK (protocol IN ('anthropic-messages', 'openai-responses'));
