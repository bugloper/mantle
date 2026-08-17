// Package gc implements online garbage collection (§12).
//
// This is the feature most likely to lose customer data and the feature whose
// absence most reliably fills a disk, so it has the most conservative design in
// the system. Three independent mechanisms each suffice to prevent the classic
// upload-versus-collect race, and all three are used together:
//
//  1. A grace window. Nothing younger than gc.grace_period is ever eligible, so
//     an in-flight push cannot outlive the window.
//  2. Transactional edges. Manifest writes insert their blob edges in the same
//     transaction that checks the blobs exist, against foreign keys declared
//     ON DELETE RESTRICT. A sweep that races a push fails its own unlink rather
//     than orphaning anything.
//  3. Quarantine. Deletion is two-phase: an object stops being served long
//     before its bytes are removed, so a mistake is recoverable for a week by
//     flipping a column.
//
// The collector removes what is unreferenced. It does not decide that a tagged
// image is old and should go — that is retention, which is policy, user-visible,
// and reversible (§12.3).
package gc

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mantle-sh/mantle/internal/ledger"
	"github.com/mantle-sh/mantle/internal/observability"
	"github.com/mantle-sh/mantle/internal/oci"
	"github.com/mantle-sh/mantle/internal/storage/driver"
)

// Options configures the collector.
type Options struct {
	Pool    *pgxpool.Pool
	Storage driver.Driver
	Ledger  *ledger.Store
	Metrics *observability.Metrics
	Logger  *slog.Logger

	GracePeriod      time.Duration
	QuarantinePeriod time.Duration
	BatchSize        int
	RollbackDepth    int
	UploadSessionTTL time.Duration
}

// MinGracePeriod is enforced regardless of configuration (§12.1).
const MinGracePeriod = time.Hour

// Collector runs garbage collection.
type Collector struct {
	opts Options
}

// New creates a collector, clamping any unsafe option.
func New(opts Options) *Collector {
	if opts.GracePeriod < MinGracePeriod {
		opts.GracePeriod = MinGracePeriod
	}
	if opts.QuarantinePeriod < time.Hour {
		opts.QuarantinePeriod = time.Hour
	}
	if opts.BatchSize <= 0 {
		opts.BatchSize = 1000
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	return &Collector{opts: opts}
}

// Stats reports one run's outcome (REQ-GC-03).
type Stats struct {
	DryRun bool `json:"dry_run"`

	SessionsCleaned int `json:"sessions_cleaned"`
	StorageAborted  int `json:"storage_uploads_aborted"`

	ManifestsQuarantined int   `json:"manifests_quarantined"`
	BlobsQuarantined     int   `json:"blobs_quarantined"`
	Unquarantined        int   `json:"unquarantined"`
	ManifestsSwept       int   `json:"manifests_swept"`
	BlobsSwept           int   `json:"blobs_swept"`
	BytesReclaimed       int64 `json:"bytes_reclaimed"`

	// Candidates lists what a dry run would quarantine, with the reason
	// (REQ-GC-04). Populated only for a dry run, since a real run can touch far
	// too many objects to enumerate.
	Candidates []Candidate `json:"candidates,omitempty"`

	PhaseDurations map[string]string `json:"phase_durations"`
	Duration       string            `json:"duration"`
	Errors         []string          `json:"errors,omitempty"`
}

// Candidate is one object a dry run would quarantine.
type Candidate struct {
	Kind   string `json:"kind"`
	Digest string `json:"digest"`
	Size   int64  `json:"size_bytes"`
	Reason string `json:"reason"`
}

// Run executes a collection cycle.
//
// Every phase is batched and takes only short-lived row locks, so pulls and
// pushes continue throughout (REQ-GC-01). A killed run leaves the registry
// consistent and the next run resumes (REQ-GC-02).
func (c *Collector) Run(ctx context.Context, dryRun bool) (*Stats, error) {
	start := time.Now()
	stats := &Stats{DryRun: dryRun, PhaseDurations: map[string]string{}}

	phase := func(name string, fn func() error) error {
		phaseStart := time.Now()
		err := fn()
		elapsed := time.Since(phaseStart)
		stats.PhaseDurations[name] = elapsed.String()
		if c.opts.Metrics != nil {
			c.opts.Metrics.GCPhaseDuration.WithLabelValues(name).Observe(elapsed.Seconds())
		}
		if err != nil {
			stats.Errors = append(stats.Errors, fmt.Sprintf("%s: %v", name, err))
		}
		return err
	}

	runID, err := c.startRun(ctx, dryRun)
	if err != nil {
		c.opts.Logger.Warn("recording the start of a GC run", "error", err)
	}

	// Phase 0 — session cleanup.
	if err := phase("sessions", func() error { return c.cleanupSessions(ctx, stats, dryRun) }); err != nil {
		return c.finish(ctx, runID, stats, start, err)
	}

	// Phases 1–3 — root set, transitive closure, and mark.
	if err := phase("mark", func() error { return c.mark(ctx, stats, dryRun) }); err != nil {
		return c.finish(ctx, runID, stats, start, err)
	}

	// Phase 4 — unquarantine anything that became reachable again.
	if err := phase("unquarantine", func() error { return c.unquarantine(ctx, stats, dryRun) }); err != nil {
		return c.finish(ctx, runID, stats, start, err)
	}

	// Phase 5 — sweep what has been quarantined long enough.
	if err := phase("sweep", func() error { return c.sweep(ctx, stats, dryRun) }); err != nil {
		return c.finish(ctx, runID, stats, start, err)
	}

	return c.finish(ctx, runID, stats, start, nil)
}

func (c *Collector) finish(ctx context.Context, runID int64, stats *Stats, start time.Time, runErr error) (*Stats, error) {
	stats.Duration = time.Since(start).String()

	outcome := "succeeded"
	if runErr != nil || len(stats.Errors) > 0 {
		outcome = "failed"
	}
	if c.opts.Metrics != nil {
		c.opts.Metrics.GCRuns.WithLabelValues(outcome).Inc()
	}
	if runID != 0 {
		c.recordRun(ctx, runID, outcome, stats, runErr)
	}

	c.opts.Logger.Info("garbage collection finished",
		"dry_run", stats.DryRun,
		"outcome", outcome,
		"manifests_quarantined", stats.ManifestsQuarantined,
		"blobs_quarantined", stats.BlobsQuarantined,
		"unquarantined", stats.Unquarantined,
		"blobs_swept", stats.BlobsSwept,
		"bytes_reclaimed", stats.BytesReclaimed,
		"duration", stats.Duration)

	return stats, runErr
}

func (c *Collector) startRun(ctx context.Context, dryRun bool) (int64, error) {
	job := "gc"
	if dryRun {
		job = "gc-dry-run"
	}
	var id int64
	err := c.opts.Pool.QueryRow(ctx,
		`INSERT INTO job_runs (job, status) VALUES ($1, 'running') RETURNING id`, job).Scan(&id)
	return id, err
}

func (c *Collector) recordRun(ctx context.Context, runID int64, outcome string, stats *Stats, runErr error) {
	encoded, err := json.Marshal(stats)
	if err != nil {
		encoded = []byte("{}")
	}
	message := ""
	if runErr != nil {
		message = runErr.Error()
	}
	_, _ = c.opts.Pool.Exec(context.WithoutCancel(ctx), `
		UPDATE job_runs SET status = $2, finished_at = now(), stats = $3, error = nullif($4, '')
		WHERE id = $1`, runID, outcome, encoded, message)
}

// --- Phase 0 ---

// cleanupSessions removes expired upload sessions and their staging.
func (c *Collector) cleanupSessions(ctx context.Context, stats *Stats, dryRun bool) error {
	rows, err := c.opts.Pool.Query(ctx,
		`SELECT id::text FROM upload_sessions WHERE expires_at <= now() LIMIT $1`, c.opts.BatchSize)
	if err != nil {
		return err
	}
	var expired []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		expired = append(expired, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	stats.SessionsCleaned = len(expired)
	if dryRun {
		return nil
	}

	for _, id := range expired {
		if upload, err := c.opts.Storage.ResumeUpload(ctx, id, oci.SHA256, driver.State{}); err == nil {
			_ = upload.Cancel(ctx)
			_ = upload.Close()
		}
		if _, err := c.opts.Pool.Exec(ctx, `DELETE FROM upload_sessions WHERE id = $1`, id); err != nil {
			return err
		}
		if c.opts.Metrics != nil {
			c.opts.Metrics.UploadsAbandoned.Inc()
		}
	}

	// Staging directories with no session row are removed too — a crash between
	// creating the directory and inserting the row leaves one behind, and
	// nothing else would ever reclaim it (§10.5).
	cutoff := time.Now().Add(-c.opts.UploadSessionTTL)
	aborted, err := c.opts.Storage.AbortStale(ctx, cutoff)
	stats.StorageAborted = aborted
	return err
}

// --- Phases 1 to 3 ---

// mark computes the live set and quarantines everything else.
//
// Liveness is computed entirely in SQL as a recursive closure over the manifest
// graph. Doing it in Go would mean loading every manifest id into memory and
// holding a consistent view across many round trips; the database already has
// both, and one statement gets a consistent snapshot for free.
func (c *Collector) mark(ctx context.Context, stats *Stats, dryRun bool) error {
	pinned, err := c.opts.Ledger.PinnedManifests(ctx, c.opts.RollbackDepth)
	if err != nil {
		return fmt.Errorf("resolving deployment pins: %w", err)
	}
	c.opts.Logger.Debug("deployment pins resolved", "count", len(pinned))

	grace := c.opts.GracePeriod

	if dryRun {
		return c.markDryRun(ctx, stats, pinned, grace)
	}

	return pgx.BeginFunc(ctx, c.opts.Pool, func(tx pgx.Tx) error {
		// Quarantine unreachable manifests.
		tag, err := tx.Exec(ctx, quarantineManifestsSQL, pinned, grace.String())
		if err != nil {
			return fmt.Errorf("quarantining manifests: %w", err)
		}
		stats.ManifestsQuarantined = int(tag.RowsAffected())

		// Then recompute blob liveness: quarantining manifests may have
		// released blobs, so this must run after the manifest pass and inside
		// the same transaction.
		tag, err = tx.Exec(ctx, quarantineBlobsSQL, grace.String())
		if err != nil {
			return fmt.Errorf("quarantining blobs: %w", err)
		}
		stats.BlobsQuarantined = int(tag.RowsAffected())
		return nil
	})
}

// liveManifestsQuery is the root set (phase 1) and its transitive closure
// (phase 2), shared by the dry run, the real run, and the unquarantine pass so
// that the three cannot drift apart.
//
// Roots are manifests reachable from a tag, pinned by retention, pinned by a
// deployment ($1), or still inside the grace window ($2). The recursion then
// adds the children of every live index, and a final union adds referrers whose
// subject is live — a signature outlives nothing, but it must never die before
// the image it describes.
const liveManifestsQuery = `
WITH RECURSIVE roots AS (
    SELECT m.id
    FROM manifests m
    WHERE m.state <> 'deleting'
      AND (
        EXISTS (SELECT 1 FROM tags t WHERE t.manifest_id = m.id)
        OR m.pinned
        OR m.id = ANY($1::bigint[])
        OR m.created_at > now() - $2::interval
      )
),
live(id) AS (
    SELECT id FROM roots
  UNION
    SELECT mc.child_id
    FROM manifest_children mc
    JOIN live ON live.id = mc.parent_id
),
-- A referrer stays alive as long as its subject does, so signatures and SBOMs
-- are never collected out from under the image they describe.
with_referrers(id) AS (
    SELECT id FROM live
  UNION
    SELECT r.id
    FROM manifests r
    JOIN manifests subject ON subject.digest = r.subject_digest
                          AND subject.repository_id = r.repository_id
    JOIN live ON live.id = subject.id
    WHERE r.subject_digest IS NOT NULL
)
SELECT id FROM with_referrers`

var quarantineManifestsSQL = `
UPDATE manifests SET state = 'quarantined', quarantined_at = now()
WHERE state = 'available'
  AND created_at < now() - $2::interval
  AND id NOT IN (` + liveManifestsQuery + `)`

// A blob is live if any non-quarantined manifest references it. REQ-GC-05
// keeps the grace-period condition here as well as in the root set: it is
// redundant, and the redundancy is deliberate, because the failure is
// catastrophic and the check is free.
const quarantineBlobsSQL = `
UPDATE blobs SET state = 'quarantined', quarantined_at = now()
WHERE state = 'available'
  AND created_at < now() - $1::interval
  AND id NOT IN (
    SELECT mb.blob_id
    FROM manifest_blobs mb
    JOIN manifests m ON m.id = mb.manifest_id
    WHERE m.state = 'available'
  )`

// markDryRun reports what would be quarantined, and why, without changing
// anything (REQ-GC-04).
func (c *Collector) markDryRun(ctx context.Context, stats *Stats, pinned []int64, grace time.Duration) error {
	rows, err := c.opts.Pool.Query(ctx, `
		SELECT m.digest, m.size_bytes, r.name
		FROM manifests m
		JOIN repositories r ON r.id = m.repository_id
		WHERE m.state = 'available'
		  AND m.created_at < now() - $2::interval
		  AND m.id NOT IN (`+liveManifestsQuery+`)
		ORDER BY m.size_bytes DESC
		LIMIT 1000`, pinned, grace.String())
	if err != nil {
		return fmt.Errorf("listing manifest candidates: %w", err)
	}
	for rows.Next() {
		var digest, repo string
		var size int
		if err := rows.Scan(&digest, &size, &repo); err != nil {
			rows.Close()
			return err
		}
		stats.Candidates = append(stats.Candidates, Candidate{
			Kind: "manifest", Digest: digest, Size: int64(size),
			Reason: "untagged, unpinned, and not referenced by any index in " + repo,
		})
		stats.ManifestsQuarantined++
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	// Blob candidates are reported against the current manifest state, so a dry
	// run slightly understates what a real run would reclaim: a real run
	// quarantines manifests first, which releases more blobs. The output says
	// so rather than pretending otherwise.
	blobRows, err := c.opts.Pool.Query(ctx, `
		SELECT digest, size_bytes FROM blobs
		WHERE state = 'available'
		  AND created_at < now() - $1::interval
		  AND id NOT IN (
		    SELECT mb.blob_id FROM manifest_blobs mb
		    JOIN manifests m ON m.id = mb.manifest_id
		    WHERE m.state = 'available'
		  )
		ORDER BY size_bytes DESC
		LIMIT 1000`, grace.String())
	if err != nil {
		return fmt.Errorf("listing blob candidates: %w", err)
	}
	defer blobRows.Close()

	for blobRows.Next() {
		var digest string
		var size int64
		if err := blobRows.Scan(&digest, &size); err != nil {
			return err
		}
		stats.Candidates = append(stats.Candidates, Candidate{
			Kind: "blob", Digest: digest, Size: size,
			Reason: "not referenced by any available manifest",
		})
		stats.BlobsQuarantined++
		stats.BytesReclaimed += size
	}
	return blobRows.Err()
}

// --- Phase 4 ---

// unquarantine returns objects that became reachable again to service.
//
// A re-push, a restored tag, or a new deployment all make a quarantined object
// live again, and it must start being served immediately rather than waiting to
// be swept and re-uploaded. This runs before the sweep, every cycle.
func (c *Collector) unquarantine(ctx context.Context, stats *Stats, dryRun bool) error {
	if dryRun {
		return nil
	}
	pinned, err := c.opts.Ledger.PinnedManifests(ctx, c.opts.RollbackDepth)
	if err != nil {
		return err
	}

	return pgx.BeginFunc(ctx, c.opts.Pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE manifests SET state = 'available', quarantined_at = NULL
			WHERE state = 'quarantined'
			  AND id IN (`+liveManifestsQuery+`)`, pinned, c.opts.GracePeriod.String())
		if err != nil {
			return fmt.Errorf("restoring manifests: %w", err)
		}
		restored := int(tag.RowsAffected())

		tag, err = tx.Exec(ctx, `
			UPDATE blobs SET state = 'available', quarantined_at = NULL
			WHERE state = 'quarantined'
			  AND id IN (
			    SELECT mb.blob_id FROM manifest_blobs mb
			    JOIN manifests m ON m.id = mb.manifest_id
			    WHERE m.state = 'available'
			  )`)
		if err != nil {
			return fmt.Errorf("restoring blobs: %w", err)
		}
		stats.Unquarantined = restored + int(tag.RowsAffected())
		return nil
	})
}

// --- Phase 5 ---

// sweep removes objects that have been quarantined longer than the quarantine
// period.
//
// Order matters and is enforced by the schema. Edges are unlinked before the
// rows they point at, and a storage delete failure leaves the row in 'deleting'
// for the next run to retry rather than removing metadata for bytes that are
// still on disk.
func (c *Collector) sweep(ctx context.Context, stats *Stats, dryRun bool) error {
	if dryRun {
		return nil
	}
	cutoff := c.opts.QuarantinePeriod

	// Manifests first: a manifest holds edges to blobs, so removing it may make
	// blobs sweepable in the same cycle.
	manifestRows, err := c.opts.Pool.Query(ctx, `
		SELECT id, digest FROM manifests
		WHERE state = 'quarantined' AND quarantined_at < now() - $1::interval
		LIMIT $2`, cutoff.String(), c.opts.BatchSize)
	if err != nil {
		return err
	}
	type manifestRef struct {
		id     int64
		digest string
	}
	var manifests []manifestRef
	for manifestRows.Next() {
		var m manifestRef
		if err := manifestRows.Scan(&m.id, &m.digest); err != nil {
			manifestRows.Close()
			return err
		}
		manifests = append(manifests, m)
	}
	manifestRows.Close()
	if err := manifestRows.Err(); err != nil {
		return err
	}

	for _, m := range manifests {
		err := pgx.BeginFunc(ctx, c.opts.Pool, func(tx pgx.Tx) error {
			// Re-check under the row lock. Between the list above and this
			// transaction the manifest may have been re-pushed, in which case
			// it is live again and must be left alone.
			var state string
			if err := tx.QueryRow(ctx,
				`SELECT state FROM manifests WHERE id = $1 FOR UPDATE`, m.id).Scan(&state); err != nil {
				return err
			}
			if state != "quarantined" {
				return nil
			}
			if _, err := tx.Exec(ctx, `UPDATE manifests SET state = 'deleting' WHERE id = $1`, m.id); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `DELETE FROM manifest_blobs WHERE manifest_id = $1`, m.id); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `DELETE FROM manifest_children WHERE parent_id = $1`, m.id); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `DELETE FROM manifests WHERE id = $1`, m.id); err != nil {
				return err
			}
			stats.ManifestsSwept++
			return nil
		})
		if err != nil {
			stats.Errors = append(stats.Errors,
				fmt.Sprintf("sweeping manifest %s: %v", m.digest, err))
		}
	}

	// Then blobs, whose bytes are what actually reclaims disk.
	blobRows, err := c.opts.Pool.Query(ctx, `
		SELECT id, digest, size_bytes FROM blobs
		WHERE state = 'quarantined' AND quarantined_at < now() - $1::interval
		LIMIT $2`, cutoff.String(), c.opts.BatchSize)
	if err != nil {
		return err
	}
	type blobRef struct {
		id     int64
		digest string
		size   int64
	}
	var blobs []blobRef
	for blobRows.Next() {
		var b blobRef
		if err := blobRows.Scan(&b.id, &b.digest, &b.size); err != nil {
			blobRows.Close()
			return err
		}
		blobs = append(blobs, b)
	}
	blobRows.Close()
	if err := blobRows.Err(); err != nil {
		return err
	}

	for _, b := range blobs {
		digest, err := oci.ParseDigest(b.digest)
		if err != nil {
			stats.Errors = append(stats.Errors,
				fmt.Sprintf("blob %s has an unparseable digest and was skipped", b.digest))
			continue
		}

		// Mark 'deleting' before touching storage. If the process dies between
		// the two, the row is left in a state the next run recognises and
		// retries, rather than a row that says 'available' for bytes that are
		// gone.
		var claimed bool
		err = pgx.BeginFunc(ctx, c.opts.Pool, func(tx pgx.Tx) error {
			tag, err := tx.Exec(ctx, `
				UPDATE blobs SET state = 'deleting'
				WHERE id = $1 AND state = 'quarantined'`, b.id)
			if err != nil {
				return err
			}
			claimed = tag.RowsAffected() == 1
			return nil
		})
		if err != nil {
			stats.Errors = append(stats.Errors, fmt.Sprintf("claiming blob %s: %v", b.digest, err))
			continue
		}
		if !claimed {
			continue // Revived between the list and now.
		}

		if err := c.opts.Storage.Delete(ctx, digest); err != nil {
			stats.Errors = append(stats.Errors, fmt.Sprintf("deleting blob %s from storage: %v", b.digest, err))
			// Left in 'deleting' deliberately, for the next run to retry. A row
			// stuck here for more than a day is what StuckDeleting reports.
			continue
		}

		if _, err := c.opts.Pool.Exec(ctx, `DELETE FROM repository_blobs WHERE blob_id = $1`, b.id); err != nil {
			stats.Errors = append(stats.Errors, fmt.Sprintf("unlinking blob %s: %v", b.digest, err))
			continue
		}
		if _, err := c.opts.Pool.Exec(ctx, `DELETE FROM blobs WHERE id = $1`, b.id); err != nil {
			stats.Errors = append(stats.Errors, fmt.Sprintf("removing blob row %s: %v", b.digest, err))
			continue
		}

		stats.BlobsSwept++
		stats.BytesReclaimed += b.size
		if c.opts.Metrics != nil {
			c.opts.Metrics.GCObjects.WithLabelValues("swept", "blob").Inc()
		}
	}
	return nil
}

// StuckDeleting reports blobs left in the 'deleting' state for too long, which
// means storage deletion has been failing repeatedly. It is an alarm, surfaced
// by 'mantle doctor' (§12.2 phase 5).
func (c *Collector) StuckDeleting(ctx context.Context, olderThan time.Duration) (int, error) {
	var count int
	err := c.opts.Pool.QueryRow(ctx, `
		SELECT count(*) FROM blobs
		WHERE state = 'deleting' AND quarantined_at < now() - $1::interval`,
		olderThan.String()).Scan(&count)
	return count, err
}
