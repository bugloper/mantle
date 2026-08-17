-- 0003_audit: the tamper-evident audit log (SEC-11) and webhook delivery.

-- ---------------------------------------------------------------------------
-- Audit log
--
-- Append-only and hash-chained: each row's hash covers its own content and the
-- previous row's hash, so removing or editing any record invalidates every
-- record after it. This does not prevent tampering by someone with database
-- write access — nothing in the database can — but it makes tampering
-- detectable, which is what an auditor actually asks for.
--
-- The chain is per-instance and strictly ordered by sequence. Appends therefore
-- serialize on the tail; audit writes are off the pull path and low-volume, so
-- that is a fair trade for a chain that is simple enough to verify by hand.
-- ---------------------------------------------------------------------------

CREATE TABLE audit_events (
  sequence      bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  uuid          uuid NOT NULL DEFAULT gen_random_uuid() UNIQUE,
  occurred_at   timestamptz NOT NULL DEFAULT now(),

  action        text NOT NULL,           -- 'repository.delete', 'token.create', …
  outcome       text NOT NULL DEFAULT 'success' CHECK (outcome IN ('success','failure','denied')),

  actor_id      bigint REFERENCES identities(id) ON DELETE SET NULL,
  actor_name    text,                    -- denormalised: survives identity deletion
  actor_kind    text,
  actor_address inet,

  target_type   text,                    -- 'repository', 'manifest', 'identity', …
  target_id     text,                    -- natural key, not a bigint: targets outlive rows
  organization  text,

  detail        jsonb NOT NULL DEFAULT '{}',
  request_id    text,

  -- Hash chain. prev_hash is the previous row's hash; the genesis row uses a
  -- 64-character zero string so that verification has no special case.
  prev_hash     text NOT NULL,
  hash          text NOT NULL
);

CREATE INDEX audit_events_time_idx ON audit_events (occurred_at DESC);
CREATE INDEX audit_events_actor_idx ON audit_events (actor_id, occurred_at DESC);
CREATE INDEX audit_events_action_idx ON audit_events (action, occurred_at DESC);
CREATE INDEX audit_events_target_idx ON audit_events (target_type, target_id, occurred_at DESC);

-- Enforce append-only at the database level. Application discipline is not a
-- control when the threat model includes a compromised application.
CREATE OR REPLACE FUNCTION mantle_audit_immutable()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
  RAISE EXCEPTION 'audit_events is append-only: % is not permitted', TG_OP
    USING HINT = 'Audit records are hash-chained; correct a mistaken record by appending a correction.';
END;
$$;

CREATE TRIGGER audit_events_no_update
  BEFORE UPDATE OR DELETE ON audit_events
  FOR EACH ROW EXECUTE FUNCTION mantle_audit_immutable();

-- Periodic chain anchors (SEC-11). An anchor records the chain head at a point
-- in time and is additionally emitted to the log stream and, optionally, to
-- object storage — so that truncating the table to before a known anchor is
-- detectable even if the whole table is replaced.
CREATE TABLE audit_anchors (
  id           bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  sequence     bigint NOT NULL,
  hash         text NOT NULL,
  anchored_at  timestamptz NOT NULL DEFAULT now(),
  exported_to  text
);

-- ---------------------------------------------------------------------------
-- Webhooks (§15.5)
-- ---------------------------------------------------------------------------

CREATE TABLE webhooks (
  id              bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  uuid            uuid NOT NULL DEFAULT gen_random_uuid() UNIQUE,
  organization_id bigint REFERENCES organizations(id) ON DELETE CASCADE,
  repository_id   bigint REFERENCES repositories(id) ON DELETE CASCADE,
  name            text NOT NULL,
  url             text NOT NULL,
  secret          text NOT NULL,          -- HMAC-SHA256 key, per endpoint
  events          text[] NOT NULL DEFAULT '{}',
  enabled         boolean NOT NULL DEFAULT true,
  disabled_reason text,
  created_at      timestamptz NOT NULL DEFAULT now(),
  updated_at      timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE webhook_deliveries (
  id            bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  webhook_id    bigint NOT NULL REFERENCES webhooks(id) ON DELETE CASCADE,
  event         text NOT NULL,
  payload       jsonb NOT NULL,
  status        text NOT NULL DEFAULT 'pending'
                  CHECK (status IN ('pending','delivered','failed','abandoned')),
  attempts      integer NOT NULL DEFAULT 0,
  next_attempt_at timestamptz NOT NULL DEFAULT now(),
  last_status_code integer,
  last_error    text,
  created_at    timestamptz NOT NULL DEFAULT now(),
  delivered_at  timestamptz
);
-- The dispatcher polls for due work; this index is what keeps that poll cheap.
CREATE INDEX webhook_deliveries_due_idx ON webhook_deliveries (next_attempt_at)
  WHERE status = 'pending';
CREATE INDEX webhook_deliveries_webhook_idx ON webhook_deliveries (webhook_id, created_at DESC);
