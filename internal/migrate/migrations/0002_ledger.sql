-- 0002_ledger: the deployment ledger (§13.3).
--
-- This is the schema behind the only thing Mantle does that a competing registry
-- does not: joining an image to the commit that produced it and the hosts
-- running it. Two properties are structural rather than incidental.
--
-- First, deployments.manifest_id is ON DELETE RESTRICT. A manifest that some
-- environment is running cannot be deleted by any path, including a bug —
-- the database refuses. That is what makes the §13.4 promise ("retention cannot
-- delete the image production is running") a guarantee rather than a policy.
--
-- Second, pull_events is partitioned and disposable. It is the highest-volume
-- table by an order of magnitude and it is fed from the pull path, so it is
-- designed to be dropped by the month and to lose rows under pressure rather
-- than to slow a pull down (REQ-LEDGER-01, REQ-LEDGER-02).

-- ---------------------------------------------------------------------------
-- Hosts
-- ---------------------------------------------------------------------------

CREATE TABLE ledger_hosts (
  id              bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  uuid            uuid NOT NULL DEFAULT gen_random_uuid() UNIQUE,
  organization_id bigint NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
  hostname        text,                    -- reported (Tier 1), if known
  address         inet,                    -- observed (Tier 0)
  identity_id     bigint REFERENCES identities(id) ON DELETE SET NULL,
  environment     text,                    -- 'production' | 'staging' | …
  first_seen_at   timestamptz NOT NULL DEFAULT now(),
  last_seen_at    timestamptz NOT NULL DEFAULT now()
);

-- A host is identified by hostname when one was reported and by address when it
-- was only observed. NULLs make a plain UNIQUE constraint useless here — in
-- Postgres, NULL never equals NULL, so (org, NULL, 10.0.1.7) could be inserted
-- repeatedly. Two partial unique indexes over COALESCE give the intended
-- behaviour: one row per named host, one row per observed address.
CREATE UNIQUE INDEX ledger_hosts_hostname_idx
  ON ledger_hosts (organization_id, hostname)
  WHERE hostname IS NOT NULL;
CREATE UNIQUE INDEX ledger_hosts_address_idx
  ON ledger_hosts (organization_id, address)
  WHERE hostname IS NULL AND address IS NOT NULL;
CREATE INDEX ledger_hosts_last_seen_idx ON ledger_hosts (organization_id, last_seen_at DESC);

-- ---------------------------------------------------------------------------
-- Deployments
-- ---------------------------------------------------------------------------

CREATE TABLE deployments (
  id                  bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  uuid                uuid NOT NULL DEFAULT gen_random_uuid() UNIQUE,
  repository_id       bigint NOT NULL REFERENCES repositories(id) ON DELETE CASCADE,
  -- RESTRICT, deliberately: see the header comment. Nothing deletes a manifest
  -- that a deployment references.
  manifest_id         bigint NOT NULL REFERENCES manifests(id) ON DELETE RESTRICT,
  tag                 text,
  environment         text NOT NULL DEFAULT 'production',
  status              text NOT NULL CHECK (status IN
                        ('started','active','superseded','rolled_back','failed')),
  confidence          text NOT NULL CHECK (confidence IN ('inferred','reported','verified')),
  commit_sha          text,
  performer           text,                -- who or what ran the deploy
  deploy_tool         text,                -- 'compose' | 'ansible' | 'systemd' | …
  deploy_tool_version text,
  external_id         text,                -- caller-supplied, for idempotency
  started_at          timestamptz NOT NULL DEFAULT now(),
  completed_at        timestamptz,
  superseded_at       timestamptz,
  metadata            jsonb NOT NULL DEFAULT '{}'
);

-- The hot ledger query is "what is active in this environment", so it gets a
-- partial index rather than a general one.
CREATE INDEX deployments_active_idx ON deployments (repository_id, environment)
  WHERE status = 'active';
CREATE INDEX deployments_manifest_idx ON deployments (manifest_id);
CREATE INDEX deployments_history_idx ON deployments (repository_id, environment, started_at DESC);

-- Idempotency for POST /api/v1/deployments (§13.2, Tier 1). A caller that
-- retries — and a fire-and-forget hook will retry — collapses onto one record.
CREATE UNIQUE INDEX deployments_external_id_idx
  ON deployments (repository_id, environment, external_id)
  WHERE external_id IS NOT NULL;

CREATE TABLE deployment_hosts (
  deployment_id bigint NOT NULL REFERENCES deployments(id) ON DELETE CASCADE,
  host_id       bigint NOT NULL REFERENCES ledger_hosts(id) ON DELETE CASCADE,
  status        text NOT NULL DEFAULT 'unknown'
                  CHECK (status IN ('unknown','pulling','confirmed','failed')),
  observed_at   timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (deployment_id, host_id)
);
CREATE INDEX deployment_hosts_host_idx ON deployment_hosts (host_id);

-- ---------------------------------------------------------------------------
-- Image provenance (§13.2, Tier 0)
--
-- Kept out of the manifests table because provenance is inferred, revisable,
-- and sometimes a guess — and because a manifest row is on the pull path while
-- this is not. The source column records how each fact was obtained so the
-- interface can distinguish a fact from an inference, and so a wrong guess is
-- diagnosable rather than mysterious.
-- ---------------------------------------------------------------------------

CREATE TABLE manifest_provenance (
  manifest_id  bigint PRIMARY KEY REFERENCES manifests(id) ON DELETE CASCADE,
  commit_sha   text,
  source_url   text,
  built_at     timestamptz,
  version      text,
  title        text,
  description  text,
  -- 'annotation' | 'label' | 'tag_pattern' | 'reported'
  source       text NOT NULL CHECK (source IN ('annotation','label','tag_pattern','reported')),
  confidence   text NOT NULL DEFAULT 'certain' CHECK (confidence IN ('certain','probable')),
  raw          jsonb NOT NULL DEFAULT '{}',
  created_at   timestamptz NOT NULL DEFAULT now(),
  updated_at   timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX manifest_provenance_commit_idx ON manifest_provenance (commit_sha)
  WHERE commit_sha IS NOT NULL;

-- ---------------------------------------------------------------------------
-- Pull events (§13.3)
-- ---------------------------------------------------------------------------

CREATE TABLE pull_events (
  id            bigint GENERATED ALWAYS AS IDENTITY,
  repository_id bigint NOT NULL,
  manifest_id   bigint,
  reference     text NOT NULL,
  digest        text,
  identity_id   bigint,
  address       inet,
  user_agent    text,
  kind          text NOT NULL DEFAULT 'manifest' CHECK (kind IN ('manifest','blob')),
  occurred_at   timestamptz NOT NULL DEFAULT now(),
  -- The partition key must be part of the primary key on a partitioned table.
  PRIMARY KEY (id, occurred_at)
) PARTITION BY RANGE (occurred_at);

-- No foreign keys on this table, by choice. It is written in batches from the
-- pull path and dropped a partition at a time; referential integrity checks
-- would add per-row cost to the highest-volume write in the system in exchange
-- for consistency guarantees on data that is explicitly allowed to be lossy.
CREATE INDEX pull_events_repository_idx ON pull_events (repository_id, occurred_at DESC);
CREATE INDEX pull_events_manifest_idx ON pull_events (manifest_id, occurred_at DESC);
CREATE INDEX pull_events_identity_idx ON pull_events (identity_id, occurred_at DESC);

-- A DEFAULT partition means an insert can never fail because the scheduled
-- partition-creation job did not run. Rows landing here are a monitoring
-- signal, not an outage.
CREATE TABLE pull_events_default PARTITION OF pull_events DEFAULT;

-- Creates the monthly partition covering the given date, if absent. Called by
-- the worker for the current and next month, so the partition normally exists
-- well before any row needs it. Idempotent.
--
-- Attaching a partition fails if the DEFAULT partition already holds rows in the
-- new range, which happens when the worker was down across a month boundary.
-- Rather than failing the job at 03:00, the conflicting rows are moved: the
-- attach is retried inside a subtransaction that drains the default partition
-- first. Doing this online takes an ACCESS EXCLUSIVE lock on pull_events for the
-- duration, which is acceptable only because nothing reads this table on the
-- pull path — it is written to and queried by the ledger, never by /v2.
CREATE OR REPLACE FUNCTION mantle_ensure_pull_event_partition(target date)
RETURNS text
LANGUAGE plpgsql
AS $$
DECLARE
  start_date date := date_trunc('month', target)::date;
  end_date   date := (date_trunc('month', target) + interval '1 month')::date;
  part_name  text := 'pull_events_' || to_char(start_date, 'YYYY_MM');
  moved      bigint;
BEGIN
  IF to_regclass('public.' || part_name) IS NOT NULL THEN
    RETURN part_name;
  END IF;

  BEGIN
    EXECUTE format(
      'CREATE TABLE %I PARTITION OF pull_events FOR VALUES FROM (%L) TO (%L)',
      part_name, start_date, end_date);
    RETURN part_name;
  EXCEPTION WHEN check_violation OR invalid_table_definition THEN
    -- The default partition holds rows belonging to the new range. Relocate
    -- them into a standalone table, attach the partition, then put them back.
    EXECUTE format(
      'CREATE TEMP TABLE mantle_partition_spill AS
         WITH drained AS (
           DELETE FROM pull_events_default
           WHERE occurred_at >= %L AND occurred_at < %L
           RETURNING *
         )
         SELECT * FROM drained', start_date, end_date);
    EXECUTE format(
      'CREATE TABLE %I PARTITION OF pull_events FOR VALUES FROM (%L) TO (%L)',
      part_name, start_date, end_date);
    EXECUTE 'INSERT INTO pull_events
               (repository_id, manifest_id, reference, digest, identity_id,
                address, user_agent, kind, occurred_at)
             SELECT repository_id, manifest_id, reference, digest, identity_id,
                    address, user_agent, kind, occurred_at
             FROM mantle_partition_spill';
    GET DIAGNOSTICS moved = ROW_COUNT;
    EXECUTE 'DROP TABLE mantle_partition_spill';
    RAISE NOTICE 'mantle: relocated % pull_events rows from the default partition into %',
      moved, part_name;
    RETURN part_name;
  END;
END;
$$;
