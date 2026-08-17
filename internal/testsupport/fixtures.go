package testsupport

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Org creates an organization and returns its id.
func Org(t *testing.T, pool *pgxpool.Pool, slug string) int64 {
	t.Helper()
	var id int64
	err := pool.QueryRow(context.Background(),
		`INSERT INTO organizations (slug, display_name) VALUES ($1, $2) RETURNING id`,
		slug, slug).Scan(&id)
	if err != nil {
		t.Fatalf("creating organization %q: %v", slug, err)
	}
	return id
}

// Repo creates a repository under an organization and returns its id. The name
// is the full OCI path, so it normally begins with the organization slug.
func Repo(t *testing.T, pool *pgxpool.Pool, orgID int64, name string) int64 {
	t.Helper()
	var id int64
	err := pool.QueryRow(context.Background(),
		`INSERT INTO repositories (organization_id, name) VALUES ($1, $2) RETURNING id`,
		orgID, name).Scan(&id)
	if err != nil {
		t.Fatalf("creating repository %q: %v", name, err)
	}
	return id
}

// User creates a user identity and returns its id.
func User(t *testing.T, pool *pgxpool.Pool, name string) int64 {
	t.Helper()
	var id int64
	err := pool.QueryRow(context.Background(),
		`INSERT INTO identities (kind, name) VALUES ('user', $1) RETURNING id`, name).Scan(&id)
	if err != nil {
		t.Fatalf("creating user %q: %v", name, err)
	}
	return id
}

// OrgAndRepo is the common two-line setup, collapsed.
func OrgAndRepo(t *testing.T, pool *pgxpool.Pool, orgSlug, repoName string) (orgID, repoID int64) {
	t.Helper()
	orgID = Org(t, pool, orgSlug)
	repoID = Repo(t, pool, orgID, repoName)
	return orgID, repoID
}
