package catalog

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
)

const repositoryColumns = `id, uuid::text, organization_id, name, visibility,
	immutable_tags, quota_bytes, coalesce(source_url, ''), created_at`

func scanRepository(row pgx.Row) (*Repository, error) {
	var r Repository
	err := row.Scan(&r.ID, &r.UUID, &r.OrganizationID, &r.Name, &r.Visibility,
		&r.ImmutableTags, &r.QuotaBytes, &r.SourceURL, &r.CreatedAt)
	if err != nil {
		return nil, noRows(err)
	}
	return &r, nil
}

// RepositoryByName looks up a repository by its full OCI path.
func (s *Store) RepositoryByName(ctx context.Context, name string) (*Repository, error) {
	return scanRepository(s.pool.QueryRow(ctx,
		`SELECT `+repositoryColumns+` FROM repositories WHERE name = $1`, name))
}

// RepositoryByID looks up a repository by its internal id.
func (s *Store) RepositoryByID(ctx context.Context, id int64) (*Repository, error) {
	return scanRepository(s.pool.QueryRow(ctx,
		`SELECT `+repositoryColumns+` FROM repositories WHERE id = $1`, id))
}

// OrganizationSlug returns the organization component of an OCI repository
// name. Mantle requires every repository to sit under an organization, so the
// first path component names one.
//
// A single-component name such as "nginx" belongs to the default organization,
// which the installer creates. Rejecting those instead would break `docker push
// registry.example.com/nginx`, which is the first thing anyone tries.
func OrganizationSlug(name string, defaultOrg string) string {
	if before, _, found := strings.Cut(name, "/"); found {
		return before
	}
	return defaultOrg
}

// EnsureRepository returns the named repository, creating it if absent.
//
// Auto-creation on first push is what makes `docker push` work without an
// out-of-band setup step, and it is why the caller must have already checked
// that the identity holds push permission over the namespace — by the time this
// is reached, the authorization decision is made.
func (s *Store) EnsureRepository(ctx context.Context, name, defaultOrg string, actorID *int64) (*Repository, error) {
	if repo, err := s.RepositoryByName(ctx, name); err == nil {
		return repo, nil
	} else if err != ErrNotFound {
		return nil, err
	}

	slug := OrganizationSlug(name, defaultOrg)

	var repo *Repository
	err := s.tx(ctx, func(tx pgx.Tx) error {
		var orgID int64
		err := tx.QueryRow(ctx, `SELECT id FROM organizations WHERE slug = $1`, slug).Scan(&orgID)
		if err == pgx.ErrNoRows {
			return fmt.Errorf(
				"organization %q does not exist: create it with 'mantle org create %s' "+
					"before pushing to %s", slug, slug, name)
		}
		if err != nil {
			return err
		}

		// The arbiter must name the constraint that actually fires. This table
		// carries two unique indexes over overlapping columns — UNIQUE
		// (organization_id, name) from the table definition, and
		// repositories_name_idx over name alone — and Postgres checks them in
		// creation order, so the composite one raises first. An arbiter of
		// (name) therefore leaves that conflict unhandled and the INSERT fails
		// with a raw 23505 instead of upserting.
		row := tx.QueryRow(ctx,
			`INSERT INTO repositories (organization_id, name)
			 VALUES ($1, $2)
			 ON CONFLICT (organization_id, name) DO UPDATE SET updated_at = now()
			 RETURNING `+repositoryColumns, orgID, name)
		repo, err = scanRepository(row)
		return err
	})
	if err != nil {
		// Belt and braces for the other index. Docker uploads a manifest's
		// layers concurrently, so a first push arrives here several times at
		// once, every one of them having seen "not found" from the SELECT
		// above. Whichever loses the race can simply read what the winner
		// wrote — the row it wanted now exists, which is the outcome it asked
		// for.
		if isUniqueViolation(err) {
			return s.RepositoryByName(ctx, name)
		}
		return nil, err
	}
	return repo, nil
}

// CreateRepository creates a repository explicitly, for the admin API.
func (s *Store) CreateRepository(ctx context.Context, orgID int64, name, visibility string) (*Repository, error) {
	row := s.pool.QueryRow(ctx,
		`INSERT INTO repositories (organization_id, name, visibility)
		 VALUES ($1, $2, $3) RETURNING `+repositoryColumns,
		orgID, name, visibility)
	repo, err := scanRepository(row)
	if isUniqueViolation(err) {
		return nil, ErrAlreadyExists
	}
	return repo, err
}

// SetVisibility changes a repository between public and private.
func (s *Store) SetVisibility(ctx context.Context, repoID int64, visibility string) error {
	if visibility != "public" && visibility != "private" {
		return fmt.Errorf("visibility must be public or private, got %q", visibility)
	}
	tag, err := s.pool.Exec(ctx,
		`UPDATE repositories SET visibility = $2, updated_at = now() WHERE id = $1`,
		repoID, visibility)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// SetImmutableTags toggles repository-wide tag immutability (§15.2).
func (s *Store) SetImmutableTags(ctx context.Context, repoID int64, immutable bool) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE repositories SET immutable_tags = $2, updated_at = now() WHERE id = $1`,
		repoID, immutable)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteRepository removes a repository and its tags and manifests.
//
// Manifests and tags cascade, but manifest_blobs holds ON DELETE RESTRICT on
// the blob side, so blobs are left behind for garbage collection to reclaim
// once nothing references them. Deleting a repository therefore frees no space
// immediately, which is worth stating in the CLI output — the alternative is a
// support thread about a disk that did not shrink.
func (s *Store) DeleteRepository(ctx context.Context, repoID int64) error {
	return s.tx(ctx, func(tx pgx.Tx) error {
		// Unlink in dependency order: tags reference manifests with RESTRICT,
		// so they must go first, then the edges, then the manifests.
		if _, err := tx.Exec(ctx, `DELETE FROM tags WHERE repository_id = $1`, repoID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			DELETE FROM manifest_children
			WHERE parent_id IN (SELECT id FROM manifests WHERE repository_id = $1)
			   OR child_id  IN (SELECT id FROM manifests WHERE repository_id = $1)`, repoID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			DELETE FROM manifest_blobs
			WHERE manifest_id IN (SELECT id FROM manifests WHERE repository_id = $1)`, repoID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM manifests WHERE repository_id = $1`, repoID); err != nil {
			if isForeignKeyViolation(err) {
				// A deployment still references a manifest here. Refusing is
				// correct: the ledger's whole promise is that a deployed image
				// does not vanish (§13.4).
				return fmt.Errorf(
					"%w: a manifest in this repository is referenced by a deployment; "+
						"mark the deployment superseded or use 'mantle retention force-delete'",
					ErrStillReferenced)
			}
			return err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM repository_blobs WHERE repository_id = $1`, repoID); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `DELETE FROM repositories WHERE id = $1`, repoID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// RepositoryPage is one page of a catalog listing.
type RepositoryPage struct {
	Names   []string
	HasMore bool
}

// ListRepositories returns repository names in lexical order for /v2/_catalog
// (REQ-OCI-08).
//
// visibleTo filters to repositories the caller may read. The full catalog is
// never returned to a caller who cannot read it: a namespace listing leaks
// customer names, which is the same disclosure problem as REQ-OCI-11 at a
// different granularity (SEC-04).
func (s *Store) ListRepositories(ctx context.Context, filter VisibilityFilter, n int, last string) (*RepositoryPage, error) {
	// One extra row tells us whether a next page exists without a second count
	// query, which on a large catalog is the difference between a range scan
	// and a full scan.
	rows, err := s.pool.Query(ctx, `
		SELECT r.name
		FROM repositories r
		WHERE r.name > $1 AND (`+visibilityPredicate("r")+`)
		ORDER BY r.name
		LIMIT $2`,
		last, n+1, filter.All, filter.IncludePublic, filter.IdentityID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	page := &RepositoryPage{Names: []string{}}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		page.Names = append(page.Names, name)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(page.Names) > n {
		page.Names = page.Names[:n]
		page.HasMore = true
	}
	return page, nil
}

// VisibilityFilter restricts a listing to what a caller may see.
type VisibilityFilter struct {
	// All bypasses filtering, for instance administrators.
	All bool
	// IdentityID is the caller, or nil for anonymous.
	IdentityID *int64
	// IncludePublic allows public repositories through regardless of grants.
	IncludePublic bool
}

// visibilityPredicate renders the SQL that decides whether a caller may see a
// repository. Only the table alias is interpolated — a compile-time constant at
// every call site — while the filter itself is bound as parameters $3 (all),
// $4 (include public) and $5 (identity id, nullable).
//
// It reads as three independent grounds for visibility: the caller is an
// instance administrator, the repository is public and public reads are being
// counted, or the caller holds an allow grant that covers it at some scope.
// Denies are not consulted here: a deny suppresses an action, and hiding a
// repository from a listing while still permitting a direct pull would be a
// worse inconsistency than showing it.
func visibilityPredicate(alias string) string {
	return `
		$3::boolean
		OR ($4::boolean AND ` + alias + `.visibility = 'public')
		OR ($5::bigint IS NOT NULL AND EXISTS (
			SELECT 1 FROM grants g
			WHERE g.effect = 'allow'
			  AND (g.identity_id = $5::bigint
			       OR g.team_id IN (SELECT team_id FROM team_members WHERE identity_id = $5::bigint))
			  AND (g.scope_type = 'instance'
			       OR (g.scope_type = 'organization' AND g.organization_id = ` + alias + `.organization_id)
			       OR (g.scope_type = 'namespace' AND ` + alias + `.name LIKE g.namespace_prefix || '%')
			       OR (g.scope_type = 'repository' AND g.repository_id = ` + alias + `.id))))`
}
