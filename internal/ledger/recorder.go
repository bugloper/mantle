package ledger

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mantle-sh/mantle/internal/events"
	"github.com/mantle-sh/mantle/internal/observability"
)

// RecorderConfig tunes the event pipeline.
type RecorderConfig struct {
	QueueSize     int
	FlushSize     int
	FlushInterval time.Duration
	// InferDeployments enables Tier 0 passive inference (§13.2).
	InferDeployments bool
	// InferenceWindow is how long after a manifest pull the layer pulls that
	// follow it are considered part of the same deployment.
	InferenceWindow time.Duration
	// MinLayersForInference is how many layer pulls must follow a manifest pull
	// before it is treated as a probable deployment rather than an inspection.
	MinLayersForInference int
}

// DefaultRecorderConfig returns sensible values.
func DefaultRecorderConfig() RecorderConfig {
	return RecorderConfig{
		QueueSize:             8192,
		FlushSize:             256,
		FlushInterval:         2 * time.Second,
		InferDeployments:      true,
		InferenceWindow:       10 * time.Minute,
		MinLayersForInference: 1,
	}
}

// Recorder buffers registry events and writes them to the ledger in batches.
//
// It implements events.Sink. Both of its methods return immediately
// and neither can block a request: the queue is bounded and a full queue drops
// the event (REQ-LEDGER-01, REQ-LEDGER-02). That is a deliberate loss of
// analytics fidelity in exchange for never adding latency to a pull.
type Recorder struct {
	store   *Store
	config  RecorderConfig
	metrics *observability.Metrics
	logger  *slog.Logger

	pulls  chan events.Pull
	pushes chan events.Push

	// configLoader supplies the image config blob for provenance extraction.
	// It is a function rather than a storage driver so the ledger does not
	// depend on the storage package, keeping this layer thin.
	configLoader func(ctx context.Context, digest string) ([]byte, error)

	inference *inferenceTracker

	stop     chan struct{}
	stopped  chan struct{}
	stopOnce sync.Once
}

// NewRecorder creates the event pipeline.
func NewRecorder(store *Store, config RecorderConfig, metrics *observability.Metrics,
	logger *slog.Logger, configLoader func(context.Context, string) ([]byte, error)) *Recorder {

	if config.QueueSize <= 0 {
		config.QueueSize = 8192
	}
	if config.FlushSize <= 0 {
		config.FlushSize = 256
	}
	if config.FlushInterval <= 0 {
		config.FlushInterval = 2 * time.Second
	}
	if config.InferenceWindow <= 0 {
		config.InferenceWindow = 10 * time.Minute
	}
	if config.MinLayersForInference <= 0 {
		config.MinLayersForInference = 1
	}

	return &Recorder{
		store:        store,
		config:       config,
		metrics:      metrics,
		logger:       logger,
		pulls:        make(chan events.Pull, config.QueueSize),
		pushes:       make(chan events.Push, config.QueueSize),
		configLoader: configLoader,
		inference:    newInferenceTracker(config.InferenceWindow),
		stop:         make(chan struct{}),
		stopped:      make(chan struct{}),
	}
}

// RecordPull queues a pull observation, dropping it if the queue is full.
func (r *Recorder) RecordPull(event events.Pull) {
	select {
	case r.pulls <- event:
		if r.metrics != nil {
			r.metrics.LedgerEventsQueued.Inc()
			r.metrics.LedgerQueueDepth.Set(float64(len(r.pulls)))
		}
	default:
		// Dropped. This is the designed behaviour under saturation, not an
		// error path — the alternative is blocking a pull on a database write.
		if r.metrics != nil {
			r.metrics.LedgerEventsDropped.Inc()
		}
	}
}

// RecordPush queues a push observation.
//
// Pushes are far rarer than pulls and carry the provenance that makes the
// ledger useful, so a full queue here is worth one log line rather than silence.
func (r *Recorder) RecordPush(event events.Push) {
	select {
	case r.pushes <- event:
	default:
		if r.metrics != nil {
			r.metrics.LedgerEventsDropped.Inc()
		}
		r.logger.Warn("dropped a push event: the ledger queue is full",
			"repository_id", event.RepositoryID, "digest", event.Digest)
	}
}

// Run processes queued events until the context is cancelled or Stop is called.
func (r *Recorder) Run(ctx context.Context) {
	defer close(r.stopped)

	ticker := time.NewTicker(r.config.FlushInterval)
	defer ticker.Stop()

	pending := make([]events.Pull, 0, r.config.FlushSize)

	flush := func() {
		if len(pending) == 0 {
			return
		}
		// Detached from the request context so a shutdown does not discard
		// events already accepted.
		writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		if err := r.flushPulls(writeCtx, pending); err != nil {
			r.logger.Warn("writing pull events to the ledger", "count", len(pending), "error", err)
		}
		cancel()
		pending = pending[:0]
		if r.metrics != nil {
			r.metrics.LedgerQueueDepth.Set(float64(len(r.pulls)))
		}
	}

	for {
		select {
		case <-ctx.Done():
			r.drain(&pending, flush)
			return
		case <-r.stop:
			r.drain(&pending, flush)
			return

		case event := <-r.pulls:
			pending = append(pending, event)
			if len(pending) >= r.config.FlushSize {
				flush()
			}

		case event := <-r.pushes:
			writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
			if err := r.handlePush(writeCtx, event); err != nil {
				r.logger.Warn("recording push provenance",
					"repository_id", event.RepositoryID, "digest", event.Digest, "error", err)
			}
			cancel()

		case <-ticker.C:
			flush()
			r.inference.expire(time.Now())
		}
	}
}

// drain flushes everything already queued during shutdown, without waiting for
// new events.
func (r *Recorder) drain(pending *[]events.Pull, flush func()) {
	for {
		select {
		case event := <-r.pulls:
			*pending = append(*pending, event)
			if len(*pending) >= r.config.FlushSize {
				flush()
			}
		default:
			flush()
			return
		}
	}
}

// Stop halts the recorder and waits for the final flush.
func (r *Recorder) Stop() {
	r.stopOnce.Do(func() { close(r.stop) })
	<-r.stopped
}

// flushPulls writes a batch of pull events and runs deployment inference.
func (r *Recorder) flushPulls(ctx context.Context, batchEvents []events.Pull) error {
	batch := &pgx.Batch{}
	for _, e := range batchEvents {
		batch.Queue(`
			INSERT INTO pull_events (repository_id, manifest_id, reference, digest,
			                         identity_id, address, user_agent, kind, occurred_at)
			VALUES ($1, $2, $3, nullif($4, ''), $5, nullif($6, '')::inet, nullif($7, ''), $8, $9)`,
			e.RepositoryID, e.ManifestID, e.Reference, e.Digest,
			e.IdentityID, e.Address, truncate(e.UserAgent, 512), e.Kind, e.OccurredAt)
	}

	results := r.store.pool.SendBatch(ctx, batch)
	var firstErr error
	for range batchEvents {
		if _, err := results.Exec(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if err := results.Close(); err != nil && firstErr == nil {
		firstErr = err
	}

	if r.config.InferDeployments {
		r.inferDeployments(ctx, batchEvents)
	}
	return firstErr
}

// handlePush extracts and stores Tier 0 provenance for a pushed manifest.
func (r *Recorder) handlePush(ctx context.Context, event events.Push) error {
	var configBlob []byte
	if event.ConfigDigest != "" && r.configLoader != nil {
		// A missing or unreadable config is not an error: an index has none,
		// and provenance degrades to annotations and the tag.
		if blob, err := r.configLoader(ctx, event.ConfigDigest); err == nil {
			configBlob = blob
		}
	}

	provenance := ExtractProvenance(event.Annotations, configBlob, event.Tag)
	if err := r.store.SaveProvenance(ctx, event.ManifestID, provenance); err != nil {
		return err
	}

	// Record the source URL on the repository, so the ledger can link to it
	// without joining through a manifest.
	if provenance.SourceURL != "" {
		_, _ = r.store.pool.Exec(ctx, `
			UPDATE repositories SET source_url = $2, updated_at = now()
			WHERE id = $1 AND (source_url IS NULL OR source_url = '')`,
			event.RepositoryID, provenance.SourceURL)
	}
	return nil
}

// inferDeployments implements Tier 0 passive inference (§13.2).
//
// The signal is a manifest pull followed by pulls of that manifest's layers
// from the same client within a short window: a client that reads a manifest
// and then fetches its layers is pulling the image to run it, whereas one that
// reads only the manifest is inspecting it. The result is recorded with
// confidence "inferred" so nothing downstream mistakes it for a report.
func (r *Recorder) inferDeployments(ctx context.Context, batchEvents []events.Pull) {
	now := time.Now()

	for _, e := range batchEvents {
		key := clientKey{repositoryID: e.RepositoryID, address: e.Address}
		if e.IdentityID != nil {
			key.identityID = *e.IdentityID
		}

		switch e.Kind {
		case "manifest":
			if e.ManifestID != nil {
				r.inference.observeManifest(key, *e.ManifestID, e.Reference, now)
			}
		case "blob":
			candidate, ready := r.inference.observeLayer(key, now, r.config.MinLayersForInference)
			if !ready {
				continue
			}
			if err := r.recordInferred(ctx, e.RepositoryID, candidate, e.Address, e.IdentityID); err != nil {
				r.logger.Debug("recording inferred deployment",
					"repository_id", e.RepositoryID, "error", err)
			}
		}
	}
	r.inference.expire(now)
}

// recordInferred writes a low-confidence deployment observed from pull traffic.
func (r *Recorder) recordInferred(ctx context.Context, repoID int64, candidate manifestObservation, address string, identityID *int64) error {
	var orgID int64
	if err := r.store.pool.QueryRow(ctx,
		`SELECT organization_id FROM repositories WHERE id = $1`, repoID).Scan(&orgID); err != nil {
		return err
	}

	// An existing reported deployment of the same image is better evidence than
	// this inference and must not be downgraded.
	var existingConfidence string
	err := r.store.pool.QueryRow(ctx, `
		SELECT confidence FROM deployments
		WHERE repository_id = $1 AND manifest_id = $2 AND status = 'active'`,
		repoID, candidate.manifestID).Scan(&existingConfidence)
	if err == nil && existingConfidence != ConfidenceInferred {
		// Still attach the host: a reported deploy that did not list its hosts
		// gains one from the observation.
		return r.attachObservedHost(ctx, orgID, repoID, candidate.manifestID, address, identityID)
	}

	commitSHA := ""
	if provenance, err := r.store.ProvenanceFor(ctx, candidate.manifestID); err == nil {
		commitSHA = provenance.CommitSHA
	}

	_, err = r.store.RecordDeployment(ctx, orgID, RecordDeploymentParams{
		RepositoryID: repoID,
		ManifestID:   candidate.manifestID,
		Tag:          candidate.reference,
		Environment:  "production",
		Status:       StatusActive,
		Confidence:   ConfidenceInferred,
		CommitSHA:    commitSHA,
		DeployTool:   "observed",
		// The external id makes inference idempotent: repeated pulls of the
		// same image by the same host update one record rather than producing
		// a deployment per pull.
		ExternalID: "inferred:" + address + ":" + candidate.reference,
		Addresses:  nonEmpty(address),
	})
	return err
}

func (r *Recorder) attachObservedHost(ctx context.Context, orgID, repoID, manifestID int64, address string, identityID *int64) error {
	if address == "" {
		return nil
	}
	return pgx.BeginFunc(ctx, r.store.pool, func(tx pgx.Tx) error {
		var deploymentID int64
		if err := tx.QueryRow(ctx, `
			SELECT id FROM deployments
			WHERE repository_id = $1 AND manifest_id = $2 AND status = 'active'
			ORDER BY started_at DESC LIMIT 1`, repoID, manifestID).Scan(&deploymentID); err != nil {
			return err
		}
		hostID, err := upsertHost(ctx, tx, orgID, "", address, "")
		if err != nil {
			return err
		}
		if identityID != nil {
			_, _ = tx.Exec(ctx,
				`UPDATE ledger_hosts SET identity_id = $2 WHERE id = $1 AND identity_id IS NULL`,
				hostID, *identityID)
		}
		return attachHost(ctx, tx, deploymentID, hostID, "pulling")
	})
}

func nonEmpty(s string) []string {
	if s == "" {
		return nil
	}
	return []string{s}
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max]
}
