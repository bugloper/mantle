package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mantle-sh/mantle/internal/admin"
	"github.com/mantle-sh/mantle/internal/auth/identity"
	"github.com/mantle-sh/mantle/internal/auth/token"
	"github.com/mantle-sh/mantle/internal/catalog"
	"github.com/mantle-sh/mantle/internal/config"
	"github.com/mantle-sh/mantle/internal/distribution"
	"github.com/mantle-sh/mantle/internal/events"
	"github.com/mantle-sh/mantle/internal/gc"
	"github.com/mantle-sh/mantle/internal/ledger"
	"github.com/mantle-sh/mantle/internal/migrate"
	"github.com/mantle-sh/mantle/internal/observability"
	"github.com/mantle-sh/mantle/internal/oci"
	"github.com/mantle-sh/mantle/internal/storage/driver"
	"github.com/mantle-sh/mantle/internal/storage/filesystem"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Version is set at build time.
var Version = "dev"

// Server owns every long-lived component of a mantled process.
type Server struct {
	Config  config.Config
	Logger  *slog.Logger
	Metrics *observability.Metrics

	Pool       *pgxpool.Pool
	Storage    driver.Driver
	Catalog    *catalog.Store
	Identities *identity.Store
	Ledger     *ledger.Store
	Recorder   *ledger.Recorder
	Collector  *gc.Collector

	Issuer   *token.Issuer
	Verifier *token.Verifier

	Health *Health

	handler http.Handler
	worker  *Worker
}

// New builds a server from configuration, connecting to every dependency and
// applying migrations.
//
// Failures here are startup failures and must name the thing that failed and
// what to do about it (principle 4). A registry that will not start is a
// production incident already; an error that says only "connection refused"
// makes it a longer one.
func New(ctx context.Context, cfg config.Config, logger *slog.Logger) (*Server, error) {
	metrics := observability.NewMetrics()

	// --- database ---
	poolConfig, err := pgxpool.ParseConfig(cfg.Database.URL)
	if err != nil {
		return nil, fmt.Errorf("parsing database.url (%s): %w", cfg.RedactedDatabaseURL(), err)
	}
	poolConfig.MaxConns = int32(cfg.Database.MaxConnections)
	poolConfig.MinConns = int32(cfg.Database.MinConnections)

	pool, err := pgxpool.NewWithConfig(ctx, poolConfig)
	if err != nil {
		return nil, fmt.Errorf("creating the database pool for %s: %w", cfg.RedactedDatabaseURL(), err)
	}
	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf(
			"cannot reach PostgreSQL at %s: %w\n"+
				"  Check that the server is running and that database.url is correct.\n"+
				"  'mantle doctor' re-runs this and the other startup checks.",
			cfg.RedactedDatabaseURL(), err)
	}

	// --- schema ---
	result, err := migrate.Run(ctx, pool)
	if err != nil {
		pool.Close()
		return nil, fmt.Errorf("applying database migrations: %w", err)
	}
	if len(result.Applied) > 0 {
		for _, applied := range result.Applied {
			logger.Info("applied migration", "version", applied.Version, "name", applied.Name)
		}
	}

	// --- storage ---
	store, err := buildStorage(cfg)
	if err != nil {
		pool.Close()
		return nil, err
	}

	// --- signing key ---
	signingKey, err := token.LoadOrCreateKey(cfg.Auth.SigningKeyPath)
	if err != nil {
		pool.Close()
		store.Close()
		return nil, err
	}
	issuer := token.NewIssuer(signingKey, cfg.Auth.Service, cfg.Auth.Service, cfg.Auth.TokenTTL.Std())
	verifier := token.NewVerifier(signingKey, cfg.Auth.Service, cfg.Auth.Service)

	catalogStore := catalog.New(pool)
	identityStore := identity.New(pool)
	ledgerStore := ledger.New(pool)

	s := &Server{
		Config:     cfg,
		Logger:     logger,
		Metrics:    metrics,
		Pool:       pool,
		Storage:    store,
		Catalog:    catalogStore,
		Identities: identityStore,
		Ledger:     ledgerStore,
		Issuer:     issuer,
		Verifier:   verifier,
		Health:     NewHealth(pool, store, Version),
	}

	// --- ledger ---
	var sink events.Sink = events.Discard{}
	if cfg.Ledger.Enabled {
		s.Recorder = ledger.NewRecorder(ledgerStore, ledger.RecorderConfig{
			QueueSize:        cfg.Ledger.QueueSize,
			FlushSize:        cfg.Ledger.FlushSize,
			FlushInterval:    cfg.Ledger.FlushInterval.Std(),
			InferDeployments: cfg.Ledger.InferDeployments,
		}, metrics, logger.With("component", "ledger"), s.loadConfigBlob)
		sink = s.Recorder
	}

	// --- garbage collection ---
	s.Collector = gc.New(gc.Options{
		Pool:             pool,
		Storage:          store,
		Ledger:           ledgerStore,
		Metrics:          metrics,
		Logger:           logger.With("component", "gc"),
		GracePeriod:      cfg.GC.GracePeriod.Std(),
		QuarantinePeriod: cfg.GC.QuarantinePeriod.Std(),
		BatchSize:        cfg.GC.BatchSize,
		RollbackDepth:    cfg.Retention.RollbackDepth,
		UploadSessionTTL: cfg.Limits.UploadSessionTTL.Std(),
	})

	// --- distribution surface ---
	registry := distribution.NewService(distribution.Options{
		Catalog:    catalogStore,
		Identities: identityStore,
		Storage:    store,
		Verifier:   verifier,
		Issuer:     issuer,
		Metrics:    metrics,
		Logger:     logger.With("component", "distribution"),
		Events:     sink,

		Realm:         cfg.Auth.Realm,
		Service:       cfg.Auth.Service,
		AnonymousPull: cfg.Auth.AnonymousPull,
		DefaultOrg:    DefaultOrganization,

		Limits: oci.Limits{
			MaxManifestSize: cfg.Limits.MaxManifestSize.Int64(),
			MaxLayers:       cfg.Limits.MaxLayers,
			MaxIndexDepth:   cfg.Limits.MaxIndexDepth,
			MaxIndexEntries: cfg.Limits.MaxIndexEntries,
		},
		MaxBlobSize:       cfg.Limits.MaxBlobSize.Int64(),
		UploadSessionTTL:  cfg.Limits.UploadSessionTTL.Std(),
		PaginationDefault: cfg.Limits.PaginationDefault,
		PaginationMax:     cfg.Limits.PaginationMax,
		RedirectBlobs:     cfg.Storage.Driver == "s3" && cfg.Storage.S3.Redirect,
		PresignTTL:        cfg.Auth.TokenTTL.Std(),
	})

	s.handler = s.buildHandler(registry)
	s.worker = NewWorker(s)
	return s, nil
}

// DefaultOrganization receives repositories pushed under a single-component
// name, so that `docker push registry.example.com/nginx` works rather than
// failing on a missing namespace.
const DefaultOrganization = "library"

func buildStorage(cfg config.Config) (driver.Driver, error) {
	switch cfg.Storage.Driver {
	case "filesystem":
		store, err := filesystem.New(cfg.Storage.Filesystem.Root)
		if err != nil {
			return nil, fmt.Errorf("initialising filesystem storage at %s: %w",
				cfg.Storage.Filesystem.Root, err)
		}
		return store, nil
	case "s3":
		// The S3 driver is specified in §10.5 but is not implemented in this
		// build. Failing loudly at startup beats appearing to work and then
		// losing every push.
		return nil, fmt.Errorf(
			"storage.driver is \"s3\", which this build does not implement; " +
				"set storage.driver to \"filesystem\"")
	default:
		return nil, fmt.Errorf("unknown storage.driver %q: expected filesystem or s3", cfg.Storage.Driver)
	}
}

// loadConfigBlob reads an image config so the ledger can extract Tier 0
// provenance from its labels. It reads config blobs only — layer bytes are
// never opened (§3.5, SEC-06).
func (s *Server) loadConfigBlob(ctx context.Context, digest string) ([]byte, error) {
	parsed, err := oci.ParseDigest(digest)
	if err != nil {
		return nil, err
	}
	info, err := s.Storage.Stat(ctx, parsed)
	if err != nil {
		return nil, err
	}
	// An image config is a few kilobytes. A cap prevents a crafted manifest
	// from pointing its config at a multi-gigabyte layer and having the ledger
	// read it into memory.
	const maxConfigSize = 4 << 20
	if info.Size > maxConfigSize {
		return nil, fmt.Errorf("image config %s is %d bytes, larger than the %d-byte limit",
			digest, info.Size, maxConfigSize)
	}

	reader, err := s.Storage.Open(ctx, parsed)
	if err != nil {
		return nil, err
	}
	defer reader.Close()

	buffer := make([]byte, info.Size)
	if _, err := io.ReadFull(reader, buffer); err != nil {
		return nil, err
	}
	return buffer, nil
}

// buildHandler assembles the routing tree.
func (s *Server) buildHandler(registry *distribution.Service) http.Handler {
	mux := http.NewServeMux()

	// The distribution surface. Authentication runs inside this subtree only:
	// health endpoints must answer without credentials, and the token endpoint
	// authenticates on its own terms.
	mux.Handle("/v2/", registry.Authenticate(registry))

	adminAPI := admin.New(admin.Options{
		Pool:          s.Pool,
		Catalog:       s.Catalog,
		Identities:    s.Identities,
		Ledger:        s.Ledger,
		Collector:     s.Collector,
		Verifier:      s.Verifier,
		Logger:        s.Logger.With("component", "admin"),
		Version:       Version,
		RollbackDepth: s.Config.Retention.RollbackDepth,
	})
	mux.Handle("/api/v1/", adminAPI)

	tokenEndpoint := NewTokenEndpoint(s.Issuer, s.Identities, s.Catalog, s.Metrics,
		s.Logger.With("component", "token"), s.Config.Auth.Realm, s.Config.Auth.AnonymousPull)
	mux.Handle("/auth/token", tokenEndpoint)
	mux.Handle("/auth/jwks.json", JWKSHandler(s.Issuer))

	mux.HandleFunc("/healthz", s.Health.Liveness)
	mux.HandleFunc("/readyz", s.Health.Readiness)

	// A bare / gets a short identifying response rather than a 404, so that
	// someone who opens the hostname in a browser learns what they reached.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintf(w, "Mantle %s — an OCI registry.\nThe registry API is at /v2/.\n", Version)
	})

	return Chain(mux,
		RequestID,
		Recover(s.Logger),
		Observe(s.Metrics, s.Logger),
	)
}

// Handler exposes the routing tree, for tests and for embedding.
func (s *Server) Handler() http.Handler { return s.handler }

// MetricsHandler serves the Prometheus endpoint on the separate metrics
// listener, so that metrics are not exposed on the public registry port.
func (s *Server) MetricsHandler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(s.Metrics.Registry(), promhttp.HandlerOpts{}))
	mux.HandleFunc("/healthz", s.Health.Liveness)
	mux.HandleFunc("/readyz", s.Health.Readiness)
	return mux
}

// Run starts the listeners and background workers, and blocks until the context
// is cancelled.
func (s *Server) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	if s.Recorder != nil {
		go s.Recorder.Run(ctx)
	}
	go s.worker.Run(ctx)

	httpServer := &http.Server{
		Addr:              s.Config.Server.Listen,
		Handler:           s.handler,
		ReadHeaderTimeout: s.Config.Server.ReadHeaderTimeout.Std(),
		// No WriteTimeout or ReadTimeout: a 10 GiB layer transfer over a slow
		// link is a legitimately long request, and a blanket timeout here would
		// fail exactly the pushes that most need to succeed. Slow-header
		// attacks are bounded by ReadHeaderTimeout instead.
		ErrorLog: slog.NewLogLogger(s.Logger.Handler(), slog.LevelWarn),
	}

	errCh := make(chan error, 2)

	go func() {
		s.Logger.Info("registry listening",
			"address", s.Config.Server.Listen,
			"tls", s.Config.Server.TLS.Mode,
			"version", Version)
		errCh <- s.listenAndServe(httpServer)
	}()

	var metricsServer *http.Server
	if s.Config.Observability.MetricsListen != "" {
		metricsServer = &http.Server{
			Addr:              s.Config.Observability.MetricsListen,
			Handler:           s.MetricsHandler(),
			ReadHeaderTimeout: 10 * time.Second,
		}
		go func() {
			s.Logger.Info("metrics listening", "address", s.Config.Observability.MetricsListen)
			if err := metricsServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				s.Logger.Error("metrics listener failed", "error", err)
			}
		}()
	}

	s.Health.SetReady(true)
	go s.pollPoolStats(ctx)

	select {
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
	}

	return s.shutdown(httpServer, metricsServer)
}

func (s *Server) listenAndServe(httpServer *http.Server) error {
	switch s.Config.Server.TLS.Mode {
	case "file":
		return httpServer.ListenAndServeTLS(s.Config.Server.TLS.Cert, s.Config.Server.TLS.Key)
	case "acme":
		return fmt.Errorf(
			"server.tls.mode is \"acme\", which this build does not implement; " +
				"use \"file\" with a certificate from your own ACME client, or \"off\" behind a terminating proxy")
	default:
		return httpServer.ListenAndServe()
	}
}

// shutdown drains gracefully: readiness fails first so the load balancer stops
// sending work, then in-flight requests are given time to finish.
func (s *Server) shutdown(httpServer, metricsServer *http.Server) error {
	s.Logger.Info("shutting down")
	s.Health.BeginShutdown()

	grace := s.Config.Server.ShutdownGrace.Std()
	if grace <= 0 {
		grace = 30 * time.Second
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), grace)
	defer cancel()

	err := httpServer.Shutdown(shutdownCtx)
	if metricsServer != nil {
		_ = metricsServer.Shutdown(shutdownCtx)
	}

	if s.Recorder != nil {
		// Flush queued ledger events rather than discarding them.
		s.Recorder.Stop()
	}
	return err
}

// Close releases resources. It is separate from shutdown so that tests and CLI
// commands can build a server without running it.
func (s *Server) Close() {
	if s.Pool != nil {
		s.Pool.Close()
	}
	if s.Storage != nil {
		_ = s.Storage.Close()
	}
}

// pollPoolStats keeps the connection-pool gauges current.
func (s *Server) pollPoolStats(ctx context.Context) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			stats := s.Pool.Stat()
			s.Metrics.DBPoolInUse.Set(float64(stats.AcquiredConns()))
			s.Metrics.DBPoolTotal.Set(float64(stats.TotalConns()))
		}
	}
}
