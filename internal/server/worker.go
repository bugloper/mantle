package server

import (
	"context"
	"os"
	"time"

	"github.com/mantle-sh/mantle/internal/config"
)

// workerLeaderLockID is the advisory lock that elects the single node running
// background work.
//
// A Postgres advisory lock is the right primitive here because it is released
// automatically when the holding connection dies. A leader that is killed, or
// whose network partitions, stops being the leader without anyone having to
// notice or run a timeout — which is exactly the property a lease row in a
// table would not give without a lot more code.
const workerLeaderLockID int64 = 0x4d4e544c_574b52 // "MNTLWKR"

// Worker runs Mantle's background jobs on whichever node holds leadership
// (NG-2: no feature may require the operator to run a second process).
type Worker struct {
	server   *Server
	schedule *config.Schedule
	node     string
}

// NewWorker creates the background worker.
func NewWorker(s *Server) *Worker {
	node, err := os.Hostname()
	if err != nil || node == "" {
		node = "unknown"
	}
	schedule, err := config.ParseSchedule(s.Config.GC.Schedule)
	if err != nil {
		// Validation already rejected an unparseable schedule at startup, so
		// this is unreachable through normal configuration. Falling back keeps
		// collection running rather than silently disabling it.
		s.Logger.Warn("garbage collection schedule is unparseable; falling back to daily at 03:00",
			"schedule", s.Config.GC.Schedule, "error", err)
		schedule, _ = config.ParseSchedule("0 3 * * *")
	}
	return &Worker{server: s, schedule: schedule, node: node}
}

// leaderRetryInterval is how often a follower re-attempts to acquire
// leadership. It bounds how long background work is paused after a leader dies.
const leaderRetryInterval = 30 * time.Second

// Run campaigns for leadership and, while holding it, runs scheduled jobs.
func (w *Worker) Run(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}
		if err := w.runAsLeader(ctx); err != nil && ctx.Err() == nil {
			w.server.Logger.Warn("background worker stopped; will retry",
				"error", err, "retry_in", leaderRetryInterval)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(leaderRetryInterval):
		}
	}
}

// runAsLeader acquires the leader lock and runs the job loop until it is lost.
func (w *Worker) runAsLeader(ctx context.Context) error {
	// A dedicated connection: an advisory lock belongs to a session, and
	// returning the connection to the pool would release the lock while this
	// node still believed it was the leader.
	conn, err := w.server.Pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()

	var acquired bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, workerLeaderLockID).Scan(&acquired); err != nil {
		return err
	}
	if !acquired {
		// Another node is the leader. This is the normal state for every node
		// but one and is not worth logging above debug.
		w.server.Logger.Debug("another node holds the worker leader lock")
		w.server.Metrics.WorkerIsLeader.Set(0)
		return nil
	}

	w.server.Logger.Info("acquired the background worker leader lock", "node", w.node)
	w.server.Metrics.WorkerIsLeader.Set(1)
	defer func() {
		w.server.Metrics.WorkerIsLeader.Set(0)
		_, _ = conn.Exec(context.WithoutCancel(ctx),
			`SELECT pg_advisory_unlock($1)`, workerLeaderLockID)
		w.server.Logger.Info("released the background worker leader lock", "node", w.node)
	}()

	// A short tick drives everything. Jobs decide for themselves whether they
	// are due, which keeps the loop trivial and means a job that overruns its
	// slot simply runs late rather than concurrently with itself.
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()

	// Housekeeping that should happen promptly on becoming leader rather than
	// waiting for the first tick.
	w.ensurePartitions(ctx)

	nextGC := w.schedule.Next(time.Now())
	if w.server.Config.GC.Enabled {
		w.server.Logger.Info("garbage collection scheduled",
			"schedule", w.server.Config.GC.Schedule, "next_run", nextGC.Format(time.RFC3339))
	}

	lastHousekeeping := time.Now()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()

		case now := <-ticker.C:
			// Confirm leadership is still held. A connection that dropped and
			// was re-established would have lost the lock silently, and two
			// nodes running GC concurrently is exactly what the lock prevents.
			if err := conn.Ping(ctx); err != nil {
				return err
			}

			if w.server.Config.GC.Enabled && !now.Before(nextGC) {
				w.runGC(ctx)
				nextGC = w.schedule.Next(now)
				w.server.Logger.Info("next garbage collection scheduled",
					"next_run", nextGC.Format(time.RFC3339))
			}

			if now.Sub(lastHousekeeping) >= time.Hour {
				w.ensurePartitions(ctx)
				w.trimPullEvents(ctx)
				lastHousekeeping = now
			}
		}
	}
}

// runGC executes one collection cycle.
func (w *Worker) runGC(ctx context.Context) {
	// Generous but bounded: a collection that has run for six hours is stuck,
	// and letting it run forever would block every later cycle.
	gcCtx, cancel := context.WithTimeout(ctx, 6*time.Hour)
	defer cancel()

	stats, err := w.server.Collector.Run(gcCtx, false)
	if err != nil {
		w.server.Logger.Error("scheduled garbage collection failed", "error", err)
		return
	}
	if stats.BytesReclaimed > 0 {
		w.server.Logger.Info("garbage collection reclaimed storage",
			"bytes", stats.BytesReclaimed, "blobs", stats.BlobsSwept)
	}
}

// ensurePartitions creates the pull_events partitions for the current and next
// month, so that a month boundary never lands rows in the default partition.
func (w *Worker) ensurePartitions(ctx context.Context) {
	now := time.Now()
	for _, target := range []time.Time{now, now.AddDate(0, 1, 0)} {
		var name string
		err := w.server.Pool.QueryRow(ctx,
			`SELECT mantle_ensure_pull_event_partition($1::date)`,
			target.Format("2006-01-02")).Scan(&name)
		if err != nil {
			w.server.Logger.Warn("creating a pull_events partition",
				"month", target.Format("2006-01"), "error", err)
			continue
		}
		w.server.Logger.Debug("pull_events partition ready", "partition", name)
	}
}

// trimPullEvents drops partitions older than the configured retention.
//
// Dropping a whole partition is why this table is partitioned at all: deleting
// ninety days of rows from a single table would be a long-running write against
// the highest-volume table in the system, whereas DROP TABLE is instant.
func (w *Worker) trimPullEvents(ctx context.Context) {
	retention := w.server.Config.Ledger.PullEventRetention.Std()
	if retention <= 0 {
		return
	}
	cutoff := time.Now().Add(-retention)

	rows, err := w.server.Pool.Query(ctx, `
		SELECT c.relname
		FROM pg_class c
		JOIN pg_inherits i ON i.inhrelid = c.oid
		JOIN pg_class parent ON parent.oid = i.inhparent
		WHERE parent.relname = 'pull_events'
		  AND c.relname ~ '^pull_events_[0-9]{4}_[0-9]{2}$'`)
	if err != nil {
		w.server.Logger.Warn("listing pull_events partitions", "error", err)
		return
	}
	var partitions []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			break
		}
		partitions = append(partitions, name)
	}
	rows.Close()

	for _, name := range partitions {
		// pull_events_YYYY_MM
		monthStart, err := time.Parse("2006_01", name[len("pull_events_"):])
		if err != nil {
			continue
		}
		// Drop only once the whole month is behind the cutoff.
		if monthStart.AddDate(0, 1, 0).After(cutoff) {
			continue
		}
		// The partition name comes from pg_class and matched a strict pattern
		// above, so it is not attacker-controlled; it is still quoted.
		if _, err := w.server.Pool.Exec(ctx, `DROP TABLE IF EXISTS "`+name+`"`); err != nil {
			w.server.Logger.Warn("dropping an expired pull_events partition",
				"partition", name, "error", err)
			continue
		}
		w.server.Logger.Info("dropped an expired pull_events partition",
			"partition", name, "retention", retention.String())
	}
}
