// Package migrate applies Mantle's embedded schema migrations.
//
// Migrations are embedded in the binary so that "replace one file and restart"
// is genuinely the whole upgrade procedure (§18.2). There is no migration tool
// to install, no directory to keep in sync with the binary, and no way for the
// two to disagree about what the schema should be.
package migrate

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// advisoryLockID guards the migration sequence. N nodes restarting together
// after an upgrade will all try to migrate; exactly one should, and the rest
// should wait and then find there is nothing to do.
//
// The value is arbitrary but must never collide with another advisory lock in
// the same database — it is derived from a fixed string rather than chosen by
// hand so that the worker leader lock in the same namespace cannot alias onto it.
const advisoryLockID int64 = 0x4d4e544c_4d4947 // "MNTLMIG"

// Migration is one embedded SQL file.
type Migration struct {
	Version int
	Name    string
	SQL     string
	// Checksum detects a migration that changed after it was applied, which
	// means the binary and the database disagree about history. That is a
	// developer error in almost every case, and a silent one if unchecked.
	Checksum string
}

// Load reads and orders the embedded migrations.
func Load() ([]Migration, error) {
	entries, err := fs.ReadDir(migrationFS, "migrations")
	if err != nil {
		return nil, fmt.Errorf("reading embedded migrations: %w", err)
	}

	var migrations []Migration
	seen := map[int]string{}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		versionStr, name, found := strings.Cut(strings.TrimSuffix(entry.Name(), ".sql"), "_")
		if !found {
			return nil, fmt.Errorf("migration %q is misnamed: expected NNNN_name.sql", entry.Name())
		}
		version, err := strconv.Atoi(versionStr)
		if err != nil {
			return nil, fmt.Errorf("migration %q has a non-numeric version prefix", entry.Name())
		}
		if other, dup := seen[version]; dup {
			return nil, fmt.Errorf("migrations %q and %q share version %d", other, entry.Name(), version)
		}
		seen[version] = entry.Name()

		body, err := migrationFS.ReadFile("migrations/" + entry.Name())
		if err != nil {
			return nil, fmt.Errorf("reading migration %q: %w", entry.Name(), err)
		}
		sum := sha256.Sum256(body)
		migrations = append(migrations, Migration{
			Version:  version,
			Name:     name,
			SQL:      string(body),
			Checksum: hex.EncodeToString(sum[:]),
		})
	}
	sort.Slice(migrations, func(i, j int) bool { return migrations[i].Version < migrations[j].Version })
	return migrations, nil
}

// Status describes one migration's state in a database.
type Status struct {
	Version   int
	Name      string
	Applied   bool
	AppliedAt time.Time
	// Drifted reports that the migration was applied but its content has since
	// changed in the binary.
	Drifted bool
}

const createVersionTable = `
CREATE TABLE IF NOT EXISTS schema_migrations (
  version     integer PRIMARY KEY,
  name        text NOT NULL,
  checksum    text NOT NULL,
  applied_at  timestamptz NOT NULL DEFAULT now(),
  duration_ms bigint NOT NULL DEFAULT 0
)`

// Pending reports which migrations have not been applied, without applying
// anything. This backs 'mantle upgrade --check' and 'mantle doctor'.
func Pending(ctx context.Context, pool *pgxpool.Pool) ([]Status, error) {
	migrations, err := Load()
	if err != nil {
		return nil, err
	}
	if _, err := pool.Exec(ctx, createVersionTable); err != nil {
		return nil, fmt.Errorf("creating schema_migrations: %w", err)
	}

	rows, err := pool.Query(ctx, `SELECT version, checksum, applied_at FROM schema_migrations`)
	if err != nil {
		return nil, fmt.Errorf("reading schema_migrations: %w", err)
	}
	defer rows.Close()

	type record struct {
		checksum  string
		appliedAt time.Time
	}
	applied := map[int]record{}
	for rows.Next() {
		var v int
		var r record
		if err := rows.Scan(&v, &r.checksum, &r.appliedAt); err != nil {
			return nil, fmt.Errorf("scanning schema_migrations: %w", err)
		}
		applied[v] = r
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading schema_migrations: %w", err)
	}

	statuses := make([]Status, 0, len(migrations))
	for _, m := range migrations {
		s := Status{Version: m.Version, Name: m.Name}
		if rec, ok := applied[m.Version]; ok {
			s.Applied = true
			s.AppliedAt = rec.appliedAt
			s.Drifted = rec.checksum != m.Checksum
		}
		statuses = append(statuses, s)
	}
	return statuses, nil
}

// Result reports what Run did.
type Result struct {
	Applied []Status
	Already int
}

// Run applies every pending migration in order.
//
// Each migration runs inside its own transaction together with the insert that
// records it, so a migration and the claim that it was applied cannot come
// apart. Postgres supports transactional DDL, which is the reason this can be
// as simple as it is.
func Run(ctx context.Context, pool *pgxpool.Pool) (*Result, error) {
	migrations, err := Load()
	if err != nil {
		return nil, err
	}

	// Take a dedicated connection: an advisory lock belongs to a session, and a
	// pooled connection could be handed to someone else mid-sequence.
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquiring a connection for migration: %w", err)
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, advisoryLockID); err != nil {
		return nil, fmt.Errorf("acquiring the migration lock: %w", err)
	}
	defer func() {
		// Best effort: if this fails the session is being torn down anyway, and
		// the lock is released when the connection closes.
		_, _ = conn.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock($1)`, advisoryLockID)
	}()

	if _, err := conn.Exec(ctx, createVersionTable); err != nil {
		return nil, fmt.Errorf("creating schema_migrations: %w", err)
	}

	applied := map[int]string{}
	rows, err := conn.Query(ctx, `SELECT version, checksum FROM schema_migrations`)
	if err != nil {
		return nil, fmt.Errorf("reading schema_migrations: %w", err)
	}
	for rows.Next() {
		var v int
		var sum string
		if err := rows.Scan(&v, &sum); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scanning schema_migrations: %w", err)
		}
		applied[v] = sum
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading schema_migrations: %w", err)
	}

	result := &Result{}
	for _, m := range migrations {
		if sum, ok := applied[m.Version]; ok {
			if sum != m.Checksum {
				return nil, fmt.Errorf(
					"migration %04d_%s was applied with a different definition than this binary contains "+
						"(recorded checksum %s, embedded checksum %s); the database and binary disagree "+
						"about schema history and continuing could corrupt data",
					m.Version, m.Name, short(sum), short(m.Checksum))
			}
			result.Already++
			continue
		}

		start := time.Now()
		err := pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, m.SQL); err != nil {
				return err
			}
			_, err := tx.Exec(ctx,
				`INSERT INTO schema_migrations (version, name, checksum, duration_ms)
				 VALUES ($1, $2, $3, $4)`,
				m.Version, m.Name, m.Checksum, time.Since(start).Milliseconds())
			return err
		})
		if err != nil {
			return result, fmt.Errorf("applying migration %04d_%s: %w", m.Version, m.Name, err)
		}
		result.Applied = append(result.Applied, Status{
			Version:   m.Version,
			Name:      m.Name,
			Applied:   true,
			AppliedAt: start,
		})
	}
	return result, nil
}

func short(checksum string) string {
	if len(checksum) > 12 {
		return checksum[:12]
	}
	return checksum
}
