// Package ledger implements the deployment ledger (§13): the join between an
// image, the commit that produced it, and the hosts running it.
//
// This is the layer that justifies the project. Everything below it makes Mantle
// a competent registry; this makes it a different product. Two constraints
// shape the whole package:
//
//   - No ledger write may sit in the synchronous path of a pull or a push
//     (REQ-LEDGER-01). Events are queued and flushed in batches.
//   - Ledger unavailability degrades to no recording, never to a failed
//     registry operation (REQ-LEDGER-02). A dropped pull event is a small
//     analytics gap; a slow pull is a failed deploy.
package ledger

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrNotFound = errors.New("not found")

// Status values for a deployment.
const (
	StatusStarted    = "started"
	StatusActive     = "active"
	StatusSuperseded = "superseded"
	StatusRolledBack = "rolled_back"
	StatusFailed     = "failed"
)

// Confidence tiers (§13.2).
const (
	ConfidenceInferred = "inferred"
	ConfidenceReported = "reported"
	ConfidenceVerified = "verified"
)

// Store is the ledger's data access layer.
type Store struct {
	pool *pgxpool.Pool
}

func New(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// Deployment is one recorded deployment.
type Deployment struct {
	ID             int64
	UUID           string
	RepositoryID   int64
	ManifestID     int64
	ManifestDigest string
	Tag            string
	Environment    string
	Status         string
	Confidence     string
	CommitSHA      string
	Performer      string
	DeployTool     string
	ExternalID     string
	StartedAt      time.Time
	CompletedAt    *time.Time
	SupersededAt   *time.Time
	Hosts          []Host
}

// Host is a machine observed or reported to be running an image.
type Host struct {
	ID          int64
	Hostname    string
	Address     string
	Environment string
	Status      string
	LastSeenAt  time.Time
}

// RecordDeploymentParams describes a deploy report (§13.2, Tier 1).
//
// Every field except the repository and the manifest is optional. A deploy
// reported with only those two still upgrades confidence over passive
// inference, which is the point: the endpoint has to be forgiving, because it
// is the one thing a user must wire up themselves.
type RecordDeploymentParams struct {
	RepositoryID int64
	ManifestID   int64
	Tag          string
	Environment  string
	Status       string
	Confidence   string
	CommitSHA    string
	Performer    string
	DeployTool   string
	ToolVersion  string
	// ExternalID makes the call idempotent. A retried hook, or a hook that
	// fires twice, collapses onto one record.
	ExternalID string
	Hostnames  []string
	Addresses  []string
	Metadata   map[string]any
}

// RecordDeployment writes or updates a deployment record.
//
// Recording an active deployment supersedes the previous active one in the same
// environment, in the same transaction. Two simultaneously active deployments
// of one repository to one environment is not a state the ledger should ever
// show, and resolving it after the fact would leave a window where the rollback
// list is wrong.
func (s *Store) RecordDeployment(ctx context.Context, orgID int64, p RecordDeploymentParams) (*Deployment, error) {
	if p.Environment == "" {
		p.Environment = "production"
	}
	if p.Status == "" {
		p.Status = StatusActive
	}
	if p.Confidence == "" {
		p.Confidence = ConfidenceReported
	}

	metadata, err := json.Marshal(p.Metadata)
	if err != nil || p.Metadata == nil {
		metadata = []byte("{}")
	}

	var deployment *Deployment
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		// Idempotency first: an existing record for this external id is updated
		// rather than duplicated.
		var id int64
		if p.ExternalID != "" {
			err := tx.QueryRow(ctx, `
				SELECT id FROM deployments
				WHERE repository_id = $1 AND environment = $2 AND external_id = $3`,
				p.RepositoryID, p.Environment, p.ExternalID).Scan(&id)
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
		}

		if p.Status == StatusActive {
			// Supersede the outgoing deployment. Restricted to this repository
			// and environment so a staging deploy never disturbs production.
			if _, err := tx.Exec(ctx, `
				UPDATE deployments
				SET status = 'superseded', superseded_at = now()
				WHERE repository_id = $1 AND environment = $2 AND status = 'active'
				  AND ($3 = 0 OR id <> $3)`,
				p.RepositoryID, p.Environment, id); err != nil {
				return err
			}
		}

		row := tx.QueryRow(ctx, `
			INSERT INTO deployments (repository_id, manifest_id, tag, environment, status,
			                         confidence, commit_sha, performer, deploy_tool,
			                         deploy_tool_version, external_id, metadata, completed_at)
			VALUES ($1, $2, nullif($3, ''), $4, $5, $6, nullif($7, ''), nullif($8, ''),
			        nullif($9, ''), nullif($10, ''), nullif($11, ''), $12,
			        CASE WHEN $5 IN ('active','failed','rolled_back') THEN now() END)
			ON CONFLICT (repository_id, environment, external_id)
			  WHERE external_id IS NOT NULL
			DO UPDATE SET
			  manifest_id = EXCLUDED.manifest_id,
			  tag         = EXCLUDED.tag,
			  status      = EXCLUDED.status,
			  confidence  = EXCLUDED.confidence,
			  commit_sha  = coalesce(EXCLUDED.commit_sha, deployments.commit_sha),
			  performer   = coalesce(EXCLUDED.performer, deployments.performer),
			  deploy_tool = coalesce(EXCLUDED.deploy_tool, deployments.deploy_tool),
			  metadata    = EXCLUDED.metadata,
			  completed_at = EXCLUDED.completed_at
			RETURNING `+deploymentColumns,
			p.RepositoryID, p.ManifestID, p.Tag, p.Environment, p.Status,
			p.Confidence, p.CommitSHA, p.Performer, p.DeployTool,
			p.ToolVersion, p.ExternalID, metadata)

		deployment, err = scanDeployment(row)
		if err != nil {
			return err
		}

		// Attach hosts, creating ledger host records as needed.
		for _, hostname := range p.Hostnames {
			hostID, err := upsertHost(ctx, tx, orgID, hostname, "", p.Environment)
			if err != nil {
				return err
			}
			if err := attachHost(ctx, tx, deployment.ID, hostID, "confirmed"); err != nil {
				return err
			}
		}
		for _, address := range p.Addresses {
			hostID, err := upsertHost(ctx, tx, orgID, "", address, p.Environment)
			if err != nil {
				return err
			}
			if err := attachHost(ctx, tx, deployment.ID, hostID, "confirmed"); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	// Re-read hosts outside the write transaction for the response.
	hosts, err := s.DeploymentHosts(ctx, deployment.ID)
	if err == nil {
		deployment.Hosts = hosts
	}
	return deployment, nil
}

const deploymentColumns = `id, uuid::text, repository_id, manifest_id, coalesce(tag, ''),
	environment, status, confidence, coalesce(commit_sha, ''), coalesce(performer, ''),
	coalesce(deploy_tool, ''), coalesce(external_id, ''), started_at, completed_at, superseded_at`

func scanDeployment(row pgx.Row) (*Deployment, error) {
	var d Deployment
	err := row.Scan(&d.ID, &d.UUID, &d.RepositoryID, &d.ManifestID, &d.Tag,
		&d.Environment, &d.Status, &d.Confidence, &d.CommitSHA, &d.Performer,
		&d.DeployTool, &d.ExternalID, &d.StartedAt, &d.CompletedAt, &d.SupersededAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &d, nil
}

// upsertHost finds or creates a ledger host.
//
// A host reported by name and the same host previously observed only by address
// are two rows until something links them. Tier 1 reporting is what supplies
// the name; until then the address is the identity, which is why the schema has
// two partial unique indexes rather than one composite key.
func upsertHost(ctx context.Context, tx pgx.Tx, orgID int64, hostname, address, environment string) (int64, error) {
	var id int64
	if hostname != "" {
		err := tx.QueryRow(ctx, `
			INSERT INTO ledger_hosts (organization_id, hostname, address, environment)
			VALUES ($1, $2, nullif($3, '')::inet, nullif($4, ''))
			ON CONFLICT (organization_id, hostname) WHERE hostname IS NOT NULL
			DO UPDATE SET last_seen_at = now(),
			              environment = coalesce(EXCLUDED.environment, ledger_hosts.environment),
			              address = coalesce(EXCLUDED.address, ledger_hosts.address)
			RETURNING id`, orgID, hostname, address, environment).Scan(&id)
		return id, err
	}
	if address == "" {
		return 0, fmt.Errorf("a host needs either a hostname or an address")
	}
	err := tx.QueryRow(ctx, `
		INSERT INTO ledger_hosts (organization_id, address, environment)
		VALUES ($1, $2::inet, nullif($3, ''))
		ON CONFLICT (organization_id, address) WHERE hostname IS NULL AND address IS NOT NULL
		DO UPDATE SET last_seen_at = now(),
		              environment = coalesce(EXCLUDED.environment, ledger_hosts.environment)
		RETURNING id`, orgID, address, environment).Scan(&id)
	return id, err
}

func attachHost(ctx context.Context, tx pgx.Tx, deploymentID, hostID int64, status string) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO deployment_hosts (deployment_id, host_id, status)
		VALUES ($1, $2, $3)
		ON CONFLICT (deployment_id, host_id)
		DO UPDATE SET status = EXCLUDED.status, observed_at = now()`,
		deploymentID, hostID, status)
	return err
}

// DeploymentHosts lists the hosts attached to a deployment.
func (s *Store) DeploymentHosts(ctx context.Context, deploymentID int64) ([]Host, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT h.id, coalesce(h.hostname, ''), coalesce(host(h.address), ''),
		       coalesce(h.environment, ''), dh.status, h.last_seen_at
		FROM deployment_hosts dh
		JOIN ledger_hosts h ON h.id = dh.host_id
		WHERE dh.deployment_id = $1
		ORDER BY h.hostname, h.address`, deploymentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	hosts := []Host{}
	for rows.Next() {
		var h Host
		if err := rows.Scan(&h.ID, &h.Hostname, &h.Address, &h.Environment, &h.Status, &h.LastSeenAt); err != nil {
			return nil, err
		}
		hosts = append(hosts, h)
	}
	return hosts, rows.Err()
}

// ActiveDeployment returns what is currently running in an environment.
func (s *Store) ActiveDeployment(ctx context.Context, repoID int64, environment string) (*Deployment, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT `+deploymentColumns+`
		FROM deployments
		WHERE repository_id = $1 AND environment = $2 AND status = 'active'
		ORDER BY started_at DESC LIMIT 1`, repoID, environment)
	deployment, err := scanDeployment(row)
	if err != nil {
		return nil, err
	}
	deployment.ManifestDigest, _ = s.manifestDigest(ctx, deployment.ManifestID)
	deployment.Hosts, _ = s.DeploymentHosts(ctx, deployment.ID)
	return deployment, nil
}

// DeploymentHistory returns recent deployments for a repository, newest first.
func (s *Store) DeploymentHistory(ctx context.Context, repoID int64, environment string, limit int) ([]*Deployment, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+deploymentColumns+`
		FROM deployments
		WHERE repository_id = $1 AND ($2 = '' OR environment = $2)
		ORDER BY started_at DESC
		LIMIT $3`, repoID, environment, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var deployments []*Deployment
	for rows.Next() {
		d, err := scanDeployment(rows)
		if err != nil {
			return nil, err
		}
		deployments = append(deployments, d)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, d := range deployments {
		d.ManifestDigest, _ = s.manifestDigest(ctx, d.ManifestID)
	}
	return deployments, nil
}

func (s *Store) manifestDigest(ctx context.Context, manifestID int64) (string, error) {
	var digest string
	err := s.pool.QueryRow(ctx, `SELECT digest FROM manifests WHERE id = $1`, manifestID).Scan(&digest)
	return digest, err
}

// SaveProvenance records what was learned about an image's origin.
func (s *Store) SaveProvenance(ctx context.Context, manifestID int64, p *Provenance) error {
	if p == nil || p.Empty() {
		return nil
	}
	raw, err := json.Marshal(p.Raw)
	if err != nil {
		raw = []byte("{}")
	}
	_, err = s.pool.Exec(ctx, `
		INSERT INTO manifest_provenance (manifest_id, commit_sha, source_url, built_at,
		                                 version, title, description, source, confidence, raw)
		VALUES ($1, nullif($2, ''), nullif($3, ''), $4, nullif($5, ''), nullif($6, ''),
		        nullif($7, ''), $8, $9, $10)
		ON CONFLICT (manifest_id) DO UPDATE SET
		  -- Never overwrite a known fact with a blank. A later push that
		  -- carries fewer annotations must not erase provenance we already have.
		  commit_sha  = coalesce(EXCLUDED.commit_sha, manifest_provenance.commit_sha),
		  source_url  = coalesce(EXCLUDED.source_url, manifest_provenance.source_url),
		  built_at    = coalesce(EXCLUDED.built_at, manifest_provenance.built_at),
		  version     = coalesce(EXCLUDED.version, manifest_provenance.version),
		  title       = coalesce(EXCLUDED.title, manifest_provenance.title),
		  description = coalesce(EXCLUDED.description, manifest_provenance.description),
		  source      = EXCLUDED.source,
		  confidence  = EXCLUDED.confidence,
		  raw         = EXCLUDED.raw,
		  updated_at  = now()`,
		manifestID, p.CommitSHA, p.SourceURL, p.BuiltAt, p.Version, p.Title,
		p.Description, string(p.Source), string(p.Confidence), raw)
	return err
}

// ProvenanceFor returns what is known about an image's origin.
func (s *Store) ProvenanceFor(ctx context.Context, manifestID int64) (*Provenance, error) {
	var p Provenance
	var raw []byte
	err := s.pool.QueryRow(ctx, `
		SELECT coalesce(commit_sha, ''), coalesce(source_url, ''), built_at,
		       coalesce(version, ''), coalesce(title, ''), coalesce(description, ''),
		       source, confidence, raw
		FROM manifest_provenance WHERE manifest_id = $1`, manifestID).Scan(
		&p.CommitSHA, &p.SourceURL, &p.BuiltAt, &p.Version, &p.Title,
		&p.Description, &p.Source, &p.Confidence, &raw)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	_ = json.Unmarshal(raw, &p.Raw)
	return &p, nil
}

// PinnedManifests returns the manifest ids that must never be collected: what
// is active in any environment, plus the rollback window behind each (§13.4).
//
// This is the query that makes the product's central promise true. Garbage
// collection treats the result as roots, and retention excludes them entirely.
func (s *Store) PinnedManifests(ctx context.Context, rollbackDepth int) ([]int64, error) {
	rows, err := s.pool.Query(ctx, `
		WITH ranked AS (
			SELECT d.manifest_id,
			       d.repository_id,
			       d.environment,
			       d.status,
			       row_number() OVER (
			         PARTITION BY d.repository_id, d.environment
			         ORDER BY d.started_at DESC
			       ) AS generation
			FROM deployments d
			WHERE d.status IN ('active', 'started', 'superseded')
		)
		SELECT DISTINCT manifest_id
		FROM ranked
		-- Generation 1 is what is running now; the next `+"`rollbackDepth`"+` are what
		-- an operator could roll back to. Anything older is collectable.
		WHERE status IN ('active', 'started') OR generation <= $1 + 1`,
		rollbackDepth)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// BlockingDeployments lists the deployments preventing a manifest's deletion,
// so that 'mantle retention force-delete' can name them (§13.4).
func (s *Store) BlockingDeployments(ctx context.Context, manifestID int64) ([]*Deployment, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+deploymentColumns+`
		FROM deployments
		WHERE manifest_id = $1 AND status IN ('active', 'started')
		ORDER BY started_at DESC`, manifestID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var deployments []*Deployment
	for rows.Next() {
		d, err := scanDeployment(rows)
		if err != nil {
			return nil, err
		}
		deployments = append(deployments, d)
	}
	return deployments, rows.Err()
}
