// Package testsupport provides shared fixtures for Mantle's tests.
//
// Tests run against a real PostgreSQL server rather than a mock. Mantle's
// correctness claims are largely claims about transactions, constraints, and
// concurrent access — ON DELETE RESTRICT stopping a GC bug, a partial unique
// index collapsing duplicate deploy reports, two nodes racing to write the same
// manifest. A mock would assert that the code calls the queries it calls, which
// is not the same thing and would pass while the registry corrupted itself.
package testsupport

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/user"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mantle-sh/mantle/internal/migrate"
)

// DatabaseURLEnv names the environment variable pointing at a PostgreSQL server
// the tests may create and drop databases on.
const DatabaseURLEnv = "MANTLE_TEST_DATABASE_URL"

var (
	templateOnce sync.Once
	templateName string
	templateErr  error
	dbCounter    atomicCounter
)

// AdminURL returns the connection URL for the test server, or "" when tests
// requiring a database should be skipped.
func AdminURL() string {
	if url := os.Getenv(DatabaseURLEnv); url != "" {
		return url
	}
	// Convenience default for a local Homebrew or system PostgreSQL, where the
	// developer's own account is typically a superuser. CI sets the variable.
	u, err := user.Current()
	if err != nil {
		return ""
	}
	return fmt.Sprintf("postgres://%s@localhost/postgres?sslmode=disable", u.Username)
}

// NewDB returns a pool connected to a freshly migrated, uniquely named database
// that is dropped when the test finishes.
//
// Migrations are applied once into a template database and each test's database
// is cloned from it. Running the full migration set per test would dominate the
// suite's runtime and discourage writing database tests, which is exactly the
// wrong incentive.
func NewDB(t *testing.T) *pgxpool.Pool {
	t.Helper()

	admin := AdminURL()
	if admin == "" {
		t.Skipf("no PostgreSQL server configured; set %s to run database tests", DatabaseURLEnv)
	}

	templateOnce.Do(func() { templateName, templateErr = buildTemplate(admin) })
	if templateErr != nil {
		t.Skipf("cannot reach the PostgreSQL server at %s (%v); "+
			"set %s to a reachable server to run database tests",
			redact(admin), templateErr, DatabaseURLEnv)
	}

	name := fmt.Sprintf("mantle_test_%d_%d", time.Now().UnixNano()%1e9, dbCounter.next())
	ctx := context.Background()

	adminPool, err := pgxpool.New(ctx, admin)
	if err != nil {
		t.Fatalf("connecting to the test server: %v", err)
	}
	defer adminPool.Close()

	// A template may not be cloned while another session is connected to it,
	// and Postgres reports that as a plain error; retry briefly rather than
	// failing a test for a transient overlap between parallel packages.
	var createErr error
	for attempt := 0; attempt < 20; attempt++ {
		_, createErr = adminPool.Exec(ctx,
			fmt.Sprintf(`CREATE DATABASE %s TEMPLATE %s`, quoteIdent(name), quoteIdent(templateName)))
		if createErr == nil {
			break
		}
		if !strings.Contains(createErr.Error(), "being accessed by other users") {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if createErr != nil {
		t.Fatalf("creating test database %s: %v", name, createErr)
	}

	pool, err := pgxpool.New(ctx, replaceDatabase(admin, name))
	if err != nil {
		t.Fatalf("connecting to test database %s: %v", name, err)
	}

	t.Cleanup(func() {
		pool.Close()
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		cleanupPool, err := pgxpool.New(cleanupCtx, admin)
		if err != nil {
			return
		}
		defer cleanupPool.Close()
		_, _ = cleanupPool.Exec(cleanupCtx,
			fmt.Sprintf(`DROP DATABASE IF EXISTS %s WITH (FORCE)`, quoteIdent(name)))
	})

	return pool
}

// templateLockID serialises template creation across concurrently running test
// binaries. `go test ./...` runs packages in parallel processes, and without
// this they race: one drops the template while another is cloning from it.
const templateLockID int64 = 0x4d4e544c_545354 // "MNTLTST"

// buildTemplate creates the migrated template database if it is absent.
//
// The template's name embeds a checksum of the migration set, which makes it
// content-addressed: a schema change produces a different name and therefore a
// fresh template, with no need to drop anything. That removes the race between
// parallel test packages entirely — nothing is ever destroyed while in use —
// and it means the migration cost is paid once per schema version rather than
// once per package.
func buildTemplate(admin string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	checksum, err := migrationsChecksum()
	if err != nil {
		return "", err
	}
	name := "mantle_test_tpl_" + checksum

	adminPool, err := pgxpool.New(ctx, admin)
	if err != nil {
		return "", err
	}
	defer adminPool.Close()
	if err := adminPool.Ping(ctx); err != nil {
		return "", err
	}

	conn, err := adminPool.Acquire(ctx)
	if err != nil {
		return "", err
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, templateLockID); err != nil {
		return "", err
	}
	defer conn.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock($1)`, templateLockID)

	var exists bool
	if err := conn.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)`, name).Scan(&exists); err != nil {
		return "", err
	}
	if exists {
		return name, nil
	}

	if _, err := conn.Exec(ctx, fmt.Sprintf(`CREATE DATABASE %s`, quoteIdent(name))); err != nil {
		return "", fmt.Errorf("creating the template database: %w", err)
	}

	templatePool, err := pgxpool.New(ctx, replaceDatabase(admin, name))
	if err != nil {
		return "", err
	}
	defer templatePool.Close()

	if _, err := migrate.Run(ctx, templatePool); err != nil {
		return "", fmt.Errorf("migrating the template database: %w", err)
	}
	return name, nil
}

// migrationsChecksum fingerprints the embedded migration set, so the template
// name changes whenever the schema does.
func migrationsChecksum() (string, error) {
	migrations, err := migrate.Load()
	if err != nil {
		return "", err
	}
	hash := sha256.New()
	for _, m := range migrations {
		fmt.Fprintf(hash, "%d:%s\n", m.Version, m.Checksum)
	}
	return hex.EncodeToString(hash.Sum(nil))[:12], nil
}

// replaceDatabase swaps the database name in a PostgreSQL URL, leaving the
// credentials and parameters intact.
func replaceDatabase(rawURL, database string) string {
	scheme, rest, found := strings.Cut(rawURL, "://")
	if !found {
		return rawURL
	}
	authority, tail, hasPath := strings.Cut(rest, "/")
	if !hasPath {
		return fmt.Sprintf("%s://%s/%s", scheme, authority, database)
	}
	_, query, hasQuery := strings.Cut(tail, "?")
	if hasQuery {
		return fmt.Sprintf("%s://%s/%s?%s", scheme, authority, database, query)
	}
	return fmt.Sprintf("%s://%s/%s", scheme, authority, database)
}

// quoteIdent quotes a SQL identifier. Test database names are generated here
// rather than taken from input, but interpolating an identifier without quoting
// is a habit worth not having.
func quoteIdent(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

func redact(rawURL string) string {
	if i := strings.Index(rawURL, "//"); i >= 0 {
		if j := strings.Index(rawURL[i:], "@"); j >= 0 {
			return rawURL[:i+2] + "***@" + rawURL[i+j+1:]
		}
	}
	return rawURL
}

type atomicCounter struct {
	mu sync.Mutex
	n  int
}

func (c *atomicCounter) next() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.n++
	return c.n
}
