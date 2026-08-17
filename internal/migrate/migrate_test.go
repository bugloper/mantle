package migrate_test

import (
	"context"
	"testing"

	"github.com/mantle-sh/mantle/internal/migrate"
	"github.com/mantle-sh/mantle/internal/testsupport"
)

func TestMigrationsApplyAndAreIdempotent(t *testing.T) {
	pool := testsupport.NewDB(t)
	ctx := context.Background()

	// The template database is already migrated, so a fresh run should find
	// everything applied and change nothing.
	result, err := migrate.Run(ctx, pool)
	if err != nil {
		t.Fatalf("re-running migrations: %v", err)
	}
	if len(result.Applied) != 0 {
		t.Errorf("re-run applied %d migrations, want 0", len(result.Applied))
	}
	if result.Already == 0 {
		t.Error("re-run reported no already-applied migrations; the schema_migrations table looks empty")
	}

	pending, err := migrate.Pending(ctx, pool)
	if err != nil {
		t.Fatalf("querying pending migrations: %v", err)
	}
	for _, s := range pending {
		if !s.Applied {
			t.Errorf("migration %04d_%s is not applied", s.Version, s.Name)
		}
		if s.Drifted {
			t.Errorf("migration %04d_%s has drifted from its recorded checksum", s.Version, s.Name)
		}
	}
}

// The schema is the product's data-safety story; these are the constraints the
// rest of the design leans on, so they are asserted directly rather than
// inferred from behaviour elsewhere.
func TestSchemaEnforcesReferentialGuarantees(t *testing.T) {
	pool := testsupport.NewDB(t)
	ctx := context.Background()

	// Every edge that could orphan stored content must be RESTRICT, not CASCADE
	// (§11.1). A CASCADE here would let a delete quietly remove a manifest that
	// something still points at.
	restrictEdges := []struct{ table, column string }{
		{"manifest_blobs", "blob_id"},
		{"manifest_children", "child_id"},
		{"tags", "manifest_id"},
		{"repository_blobs", "blob_id"},
		{"deployments", "manifest_id"},
	}
	for _, edge := range restrictEdges {
		var action string
		err := pool.QueryRow(ctx, `
			SELECT c.confdeltype
			FROM pg_constraint c
			JOIN pg_class t ON t.oid = c.conrelid
			JOIN pg_attribute a ON a.attrelid = t.oid AND a.attnum = ANY (c.conkey)
			WHERE c.contype = 'f' AND t.relname = $1 AND a.attname = $2`,
			edge.table, edge.column).Scan(&action)
		if err != nil {
			t.Errorf("looking up the foreign key on %s.%s: %v", edge.table, edge.column, err)
			continue
		}
		// 'r' is NO ACTION/RESTRICT, 'a' is NO ACTION. Either refuses the delete.
		if action != "r" && action != "a" {
			t.Errorf("%s.%s has ON DELETE '%s', want RESTRICT: this edge must refuse to orphan content",
				edge.table, edge.column, action)
		}
	}
}

func TestAuditLogIsAppendOnly(t *testing.T) {
	pool := testsupport.NewDB(t)
	ctx := context.Background()

	if _, err := pool.Exec(ctx, `
		INSERT INTO audit_events (action, actor_name, prev_hash, hash)
		VALUES ('test.event', 'tester', repeat('0', 64), repeat('a', 64))`); err != nil {
		t.Fatalf("inserting an audit event: %v", err)
	}

	// SEC-11: the database must refuse mutation, not merely the application.
	if _, err := pool.Exec(ctx, `UPDATE audit_events SET action = 'tampered'`); err == nil {
		t.Error("UPDATE on audit_events succeeded; the append-only trigger is not protecting the log")
	}
	if _, err := pool.Exec(ctx, `DELETE FROM audit_events`); err == nil {
		t.Error("DELETE on audit_events succeeded; the append-only trigger is not protecting the log")
	}
}

func TestPullEventPartitionHelper(t *testing.T) {
	pool := testsupport.NewDB(t)
	ctx := context.Background()

	var name string
	if err := pool.QueryRow(ctx,
		`SELECT mantle_ensure_pull_event_partition('2026-08-17'::date)`).Scan(&name); err != nil {
		t.Fatalf("creating a pull_events partition: %v", err)
	}
	if name != "pull_events_2026_08" {
		t.Errorf("partition name = %q, want pull_events_2026_08", name)
	}

	// Idempotent: the worker calls this on every cycle.
	if err := pool.QueryRow(ctx,
		`SELECT mantle_ensure_pull_event_partition('2026-08-17'::date)`).Scan(&name); err != nil {
		t.Fatalf("re-creating an existing partition should be a no-op, got: %v", err)
	}
}

// The worker can be down across a month boundary, leaving rows for the new
// month in the default partition. Creating that month's partition afterwards
// must relocate them rather than failing the job.
func TestPullEventPartitionRelocatesDefaultRows(t *testing.T) {
	pool := testsupport.NewDB(t)
	ctx := context.Background()

	_, repoID := testsupport.OrgAndRepo(t, pool, "acme", "acme/web")

	if _, err := pool.Exec(ctx, `
		INSERT INTO pull_events (repository_id, reference, occurred_at)
		VALUES ($1, 'v1', '2027-03-15T10:00:00Z'), ($1, 'v2', '2027-03-16T10:00:00Z')`,
		repoID); err != nil {
		t.Fatalf("inserting into the default partition: %v", err)
	}

	var inDefault int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM pull_events_default`).Scan(&inDefault); err != nil {
		t.Fatal(err)
	}
	if inDefault != 2 {
		t.Fatalf("default partition holds %d rows, want 2", inDefault)
	}

	var name string
	if err := pool.QueryRow(ctx,
		`SELECT mantle_ensure_pull_event_partition('2027-03-01'::date)`).Scan(&name); err != nil {
		t.Fatalf("creating a partition over occupied default rows: %v", err)
	}

	var relocated, remaining int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM pull_events_2027_03`).Scan(&relocated); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM pull_events_default`).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if relocated != 2 {
		t.Errorf("new partition holds %d rows, want 2", relocated)
	}
	if remaining != 0 {
		t.Errorf("default partition still holds %d rows, want 0", remaining)
	}
}
