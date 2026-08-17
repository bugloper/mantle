-- 0001_core: organizations, identities, authorization, and the object catalog.
--
-- Conventions, applied throughout and not repeated per table:
--   * bigint identity primary keys, with a separate public uuid where the
--     object is addressable from the API. Internal ids never appear in URLs.
--   * timestamptz everywhere. A registry that stores naive timestamps will
--     eventually delete the wrong thing across a DST boundary.
--   * ON DELETE RESTRICT on every edge that could orphan stored content. This
--     is load-bearing for garbage collection (§12.1): a GC bug becomes a failed
--     transaction rather than a manifest pointing at bytes that no longer exist.

CREATE EXTENSION IF NOT EXISTS citext;

-- ---------------------------------------------------------------------------
-- Organizations
-- ---------------------------------------------------------------------------

CREATE TABLE organizations (
  id           bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  uuid         uuid NOT NULL DEFAULT gen_random_uuid() UNIQUE,
  slug         citext NOT NULL UNIQUE,
  display_name text NOT NULL,
  quota_bytes  bigint,                    -- NULL = unlimited
  created_at   timestamptz NOT NULL DEFAULT now(),
  updated_at   timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT organizations_quota_positive CHECK (quota_bytes IS NULL OR quota_bytes >= 0)
);

-- ---------------------------------------------------------------------------
-- Identities (§9.2)
--
-- Users, robot accounts, deploy tokens and personal access tokens share one
-- table because they share one thing that matters: they are the subject of an
-- authorization decision and the actor in an audit record. A PAT additionally
-- carries owner_id, so the audit trail can say "the PAT 'laptop' owned by nima"
-- rather than losing the human behind the credential.
-- ---------------------------------------------------------------------------

CREATE TABLE identities (
  id              bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  uuid            uuid NOT NULL DEFAULT gen_random_uuid() UNIQUE,
  kind            text NOT NULL CHECK (kind IN ('user','pat','robot','deploy_token')),
  name            citext NOT NULL,
  organization_id bigint REFERENCES organizations(id) ON DELETE CASCADE,
  owner_id        bigint REFERENCES identities(id) ON DELETE CASCADE,
  email           citext,
  display_name    text,

  -- Authentication material. Users authenticate with a password; machine
  -- identities present a secret. Both are Argon2id hashes and neither is ever
  -- recoverable — secrets are shown exactly once, at creation.
  secret_hash     text,

  -- Machine credentials are looked up by an indexed plaintext selector rather
  -- than by hashing the presented secret against every row. Without this, a
  -- login would cost one Argon2id evaluation per identity in the table, which
  -- is both slow and a denial-of-service primitive.
  secret_selector text UNIQUE,

  instance_admin  boolean NOT NULL DEFAULT false,
  disabled        boolean NOT NULL DEFAULT false,
  disabled_reason text,
  expires_at      timestamptz,
  last_used_at    timestamptz,
  created_by      bigint REFERENCES identities(id) ON DELETE SET NULL,
  created_at      timestamptz NOT NULL DEFAULT now(),
  updated_at      timestamptz NOT NULL DEFAULT now(),

  -- A user is unique by name instance-wide; machine identities are scoped to
  -- an organization. Enforced by the two partial indexes below.
  CONSTRAINT identities_user_has_no_owner CHECK (kind <> 'user' OR owner_id IS NULL),
  CONSTRAINT identities_machine_has_org CHECK (kind = 'user' OR organization_id IS NOT NULL)
);

CREATE UNIQUE INDEX identities_user_name_idx ON identities (name) WHERE kind = 'user';
CREATE UNIQUE INDEX identities_machine_name_idx ON identities (organization_id, kind, name)
  WHERE kind <> 'user';
CREATE INDEX identities_owner_idx ON identities (owner_id) WHERE owner_id IS NOT NULL;

-- ---------------------------------------------------------------------------
-- Teams (§9.3)
-- ---------------------------------------------------------------------------

CREATE TABLE teams (
  id              bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  uuid            uuid NOT NULL DEFAULT gen_random_uuid() UNIQUE,
  organization_id bigint NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
  slug            citext NOT NULL,
  display_name    text NOT NULL,
  created_at      timestamptz NOT NULL DEFAULT now(),
  updated_at      timestamptz NOT NULL DEFAULT now(),
  UNIQUE (organization_id, slug)
);

CREATE TABLE team_members (
  team_id     bigint NOT NULL REFERENCES teams(id) ON DELETE CASCADE,
  identity_id bigint NOT NULL REFERENCES identities(id) ON DELETE CASCADE,
  created_at  timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (team_id, identity_id)
);
CREATE INDEX team_members_identity_idx ON team_members (identity_id);

-- ---------------------------------------------------------------------------
-- Repositories
-- ---------------------------------------------------------------------------

CREATE TABLE repositories (
  id              bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  uuid            uuid NOT NULL DEFAULT gen_random_uuid() UNIQUE,
  organization_id bigint NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
  name            text NOT NULL,          -- full OCI path, e.g. 'acme/web'
  visibility      text NOT NULL DEFAULT 'private'
                    CHECK (visibility IN ('private','public')),
  immutable_tags  boolean NOT NULL DEFAULT false,
  quota_bytes     bigint,
  project_kind    text,                   -- inferred from image config, §13.2
  source_url      text,                   -- org.opencontainers.image.source
  description     text,
  created_at      timestamptz NOT NULL DEFAULT now(),
  updated_at      timestamptz NOT NULL DEFAULT now(),
  UNIQUE (organization_id, name),
  CONSTRAINT repositories_quota_positive CHECK (quota_bytes IS NULL OR quota_bytes >= 0)
);

-- The OCI name is globally unique across the instance: a client asks for
-- /v2/acme/web/… with no organization qualifier, so the name must resolve
-- without one. The organization is derived from the name's first component at
-- creation time.
CREATE UNIQUE INDEX repositories_name_idx ON repositories (name);

-- Tag protection patterns (§15.2). Held separately from the repository so a
-- repository can have several, which is the common real configuration:
-- v* immutable while latest and main-* stay mutable.
CREATE TABLE tag_protection_rules (
  id            bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  repository_id bigint NOT NULL REFERENCES repositories(id) ON DELETE CASCADE,
  pattern       text NOT NULL,
  immutable     boolean NOT NULL DEFAULT false,
  min_role      text NOT NULL DEFAULT 'maintainer'
                  CHECK (min_role IN ('contributor','maintainer','owner')),
  created_at    timestamptz NOT NULL DEFAULT now(),
  UNIQUE (repository_id, pattern)
);

-- ---------------------------------------------------------------------------
-- Authorization grants (§9.3)
--
-- A grant binds a principal (identity or team) to a role over a scope
-- (instance, organization, namespace prefix, or a single repository).
-- Evaluation is deny-by-default and an explicit deny always wins; denies exist
-- so a compromised credential can be locked out without deleting it, which
-- would take the audit trail with it.
-- ---------------------------------------------------------------------------

CREATE TABLE grants (
  id               bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  uuid             uuid NOT NULL DEFAULT gen_random_uuid() UNIQUE,
  principal_type   text NOT NULL CHECK (principal_type IN ('identity','team')),
  identity_id      bigint REFERENCES identities(id) ON DELETE CASCADE,
  team_id          bigint REFERENCES teams(id) ON DELETE CASCADE,

  scope_type       text NOT NULL CHECK (scope_type IN ('instance','organization','namespace','repository')),
  organization_id  bigint REFERENCES organizations(id) ON DELETE CASCADE,
  namespace_prefix text,
  repository_id    bigint REFERENCES repositories(id) ON DELETE CASCADE,

  role             text NOT NULL CHECK (role IN ('reader','contributor','maintainer','owner','org_admin')),
  effect           text NOT NULL DEFAULT 'allow' CHECK (effect IN ('allow','deny')),
  created_by       bigint REFERENCES identities(id) ON DELETE SET NULL,
  created_at       timestamptz NOT NULL DEFAULT now(),

  CONSTRAINT grants_principal_exclusive CHECK (
    (principal_type = 'identity' AND identity_id IS NOT NULL AND team_id IS NULL) OR
    (principal_type = 'team'     AND team_id     IS NOT NULL AND identity_id IS NULL)),
  CONSTRAINT grants_scope_consistent CHECK (
    (scope_type = 'instance'     AND organization_id IS NULL     AND namespace_prefix IS NULL AND repository_id IS NULL) OR
    (scope_type = 'organization' AND organization_id IS NOT NULL AND namespace_prefix IS NULL AND repository_id IS NULL) OR
    (scope_type = 'namespace'    AND namespace_prefix IS NOT NULL AND repository_id IS NULL) OR
    (scope_type = 'repository'   AND repository_id IS NOT NULL   AND namespace_prefix IS NULL))
);

CREATE INDEX grants_identity_idx ON grants (identity_id) WHERE identity_id IS NOT NULL;
CREATE INDEX grants_team_idx ON grants (team_id) WHERE team_id IS NOT NULL;
CREATE INDEX grants_repository_idx ON grants (repository_id) WHERE repository_id IS NOT NULL;

-- ---------------------------------------------------------------------------
-- Blobs
-- ---------------------------------------------------------------------------

CREATE TABLE blobs (
  id             bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  digest         text NOT NULL UNIQUE,    -- 'sha256:…'
  size_bytes     bigint NOT NULL CHECK (size_bytes >= 0),
  media_type     text,
  storage_ref    text NOT NULL,
  state          text NOT NULL DEFAULT 'available'
                   CHECK (state IN ('available','quarantined','deleting')),
  quarantined_at timestamptz,
  created_at     timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX blobs_gc_idx ON blobs (state, created_at);
CREATE INDEX blobs_quarantine_idx ON blobs (state, quarantined_at) WHERE state <> 'available';

-- A blob is globally deduplicated; this table records which repositories may
-- serve it. Without it, any authenticated user could pull any layer in the
-- instance by digest, which is the same exfiltration hole as an unchecked
-- cross-repository mount (SEC-03).
CREATE TABLE repository_blobs (
  repository_id bigint NOT NULL REFERENCES repositories(id) ON DELETE CASCADE,
  blob_id       bigint NOT NULL REFERENCES blobs(id) ON DELETE RESTRICT,
  created_at    timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (repository_id, blob_id)
);
CREATE INDEX repository_blobs_blob_idx ON repository_blobs (blob_id);

-- ---------------------------------------------------------------------------
-- Manifests (§11.3 — stored in Postgres, byte-exact, never reserialized)
-- ---------------------------------------------------------------------------

CREATE TABLE manifests (
  id             bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  repository_id  bigint NOT NULL REFERENCES repositories(id) ON DELETE CASCADE,
  digest         text NOT NULL,
  media_type     text NOT NULL,
  artifact_type  text,
  subject_digest text,                    -- referrers
  size_bytes     integer NOT NULL CHECK (size_bytes >= 0),
  payload        bytea NOT NULL,          -- byte-exact, REQ-OCI-02
  config_digest  text,
  annotations    jsonb NOT NULL DEFAULT '{}',
  platform       jsonb,                   -- os/arch for child manifests
  pinned         boolean NOT NULL DEFAULT false,
  state          text NOT NULL DEFAULT 'available'
                   CHECK (state IN ('available','quarantined','deleting')),
  quarantined_at timestamptz,
  pushed_by      bigint REFERENCES identities(id) ON DELETE SET NULL,
  created_at     timestamptz NOT NULL DEFAULT now(),
  UNIQUE (repository_id, digest)
);
CREATE INDEX manifests_subject_idx ON manifests (repository_id, subject_digest)
  WHERE subject_digest IS NOT NULL;
CREATE INDEX manifests_gc_idx ON manifests (state, created_at);
CREATE INDEX manifests_repository_idx ON manifests (repository_id, created_at DESC);

-- manifest → blob edges (layers + config)
CREATE TABLE manifest_blobs (
  manifest_id bigint NOT NULL REFERENCES manifests(id) ON DELETE CASCADE,
  blob_id     bigint NOT NULL REFERENCES blobs(id) ON DELETE RESTRICT,
  PRIMARY KEY (manifest_id, blob_id)
);
CREATE INDEX manifest_blobs_blob_idx ON manifest_blobs (blob_id);

-- index → child manifest edges
CREATE TABLE manifest_children (
  parent_id bigint NOT NULL REFERENCES manifests(id) ON DELETE CASCADE,
  child_id  bigint NOT NULL REFERENCES manifests(id) ON DELETE RESTRICT,
  PRIMARY KEY (parent_id, child_id)
);
CREATE INDEX manifest_children_child_idx ON manifest_children (child_id);

-- ---------------------------------------------------------------------------
-- Tags
-- ---------------------------------------------------------------------------

CREATE TABLE tags (
  id            bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  repository_id bigint NOT NULL REFERENCES repositories(id) ON DELETE CASCADE,
  name          text NOT NULL,
  manifest_id   bigint NOT NULL REFERENCES manifests(id) ON DELETE RESTRICT,
  protected     boolean NOT NULL DEFAULT false,
  pushed_by     bigint REFERENCES identities(id) ON DELETE SET NULL,
  created_at    timestamptz NOT NULL DEFAULT now(),
  updated_at    timestamptz NOT NULL DEFAULT now(),
  UNIQUE (repository_id, name)
);
CREATE INDEX tags_manifest_idx ON tags (manifest_id);
-- tags/list returns lexical order with keyset pagination (REQ-OCI-08); this is
-- the index that makes 'WHERE name > $last ORDER BY name LIMIT n' a range scan.
CREATE INDEX tags_lexical_idx ON tags (repository_id, name);

CREATE TABLE tag_history (
  id              bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  repository_id   bigint NOT NULL REFERENCES repositories(id) ON DELETE CASCADE,
  name            text NOT NULL,
  manifest_digest text NOT NULL,
  action          text NOT NULL CHECK (action IN ('set','moved','deleted')),
  actor_id        bigint REFERENCES identities(id) ON DELETE SET NULL,
  occurred_at     timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX tag_history_lookup_idx ON tag_history (repository_id, name, occurred_at DESC);

-- ---------------------------------------------------------------------------
-- Upload sessions (§11.2)
--
-- Session state lives in Postgres rather than in a node's memory so that a
-- plain L7 load balancer may route consecutive chunks of one push to different
-- nodes. hash_state is the checkpointed SHA-256, which is what makes resuming a
-- 2 GB layer cost nothing instead of a 2 GB re-read (§10.4).
-- ---------------------------------------------------------------------------

CREATE TABLE upload_sessions (
  id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  repository_id bigint NOT NULL REFERENCES repositories(id) ON DELETE CASCADE,
  identity_id   bigint NOT NULL REFERENCES identities(id) ON DELETE CASCADE,
  byte_offset   bigint NOT NULL DEFAULT 0 CHECK (byte_offset >= 0),
  hash_state    bytea,
  storage_ref   text NOT NULL,
  s3_upload_id  text,
  s3_parts      jsonb NOT NULL DEFAULT '[]',
  created_at    timestamptz NOT NULL DEFAULT now(),
  updated_at    timestamptz NOT NULL DEFAULT now(),
  expires_at    timestamptz NOT NULL
);
CREATE INDEX upload_sessions_expiry_idx ON upload_sessions (expires_at);
CREATE INDEX upload_sessions_repository_idx ON upload_sessions (repository_id);

-- ---------------------------------------------------------------------------
-- Retention policies (§15.1)
-- ---------------------------------------------------------------------------

CREATE TABLE retention_policies (
  id              bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  uuid            uuid NOT NULL DEFAULT gen_random_uuid() UNIQUE,
  name            text NOT NULL,
  -- Scope: instance-wide when both are null, otherwise organization or
  -- repository. The most specific matching policy set wins.
  organization_id bigint REFERENCES organizations(id) ON DELETE CASCADE,
  repository_id   bigint REFERENCES repositories(id) ON DELETE CASCADE,
  -- rules is the declarative policy document from §15.1, stored verbatim so
  -- that what the operator wrote is what 'retention simulate' explains.
  rules           jsonb NOT NULL DEFAULT '[]',
  enabled         boolean NOT NULL DEFAULT true,
  last_run_at     timestamptz,
  last_result     jsonb,
  created_at      timestamptz NOT NULL DEFAULT now(),
  updated_at      timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT retention_scope_exclusive CHECK (organization_id IS NULL OR repository_id IS NULL)
);

-- ---------------------------------------------------------------------------
-- Background job bookkeeping
--
-- Worker leadership is a Postgres advisory lock rather than a row, so a crashed
-- leader releases it with its connection (NG-2). This table records outcomes,
-- which is what 'mantle gc status' and 'mantle doctor' read.
-- ---------------------------------------------------------------------------

CREATE TABLE job_runs (
  id           bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  job          text NOT NULL,
  status       text NOT NULL CHECK (status IN ('running','succeeded','failed')),
  started_at   timestamptz NOT NULL DEFAULT now(),
  finished_at  timestamptz,
  node         text,
  stats        jsonb NOT NULL DEFAULT '{}',
  error        text
);
CREATE INDEX job_runs_recent_idx ON job_runs (job, started_at DESC);
