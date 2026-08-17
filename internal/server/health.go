package server

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mantle-sh/mantle/internal/migrate"
	"github.com/mantle-sh/mantle/internal/storage/driver"
)

// Health serves the liveness and readiness endpoints (§16.2).
//
// The distinction matters operationally and is often collapsed by mistake.
// Liveness answers "is this process alive" and must not depend on anything
// external — a database blip that fails liveness gets the container killed and
// restarted, which makes the outage worse. Readiness answers "should this node
// receive traffic" and does depend on the database and storage, so a node that
// cannot serve is removed from the load balancer instead of returning errors.
type Health struct {
	pool    *pgxpool.Pool
	storage driver.Driver

	// ready is flipped once startup has completed, so a node is not advertised
	// as ready while it is still migrating.
	ready atomic.Bool
	// shuttingDown makes readiness fail immediately on SIGTERM, so the load
	// balancer drains this node before the listener closes. Without it, a
	// rolling upgrade drops the requests in flight at the moment of shutdown,
	// which breaks the "pull availability 100% during rolling upgrade" target.
	shuttingDown atomic.Bool

	version string
}

// NewHealth builds the health endpoints.
func NewHealth(pool *pgxpool.Pool, storage driver.Driver, version string) *Health {
	return &Health{pool: pool, storage: storage, version: version}
}

// SetReady marks startup complete.
func (h *Health) SetReady(ready bool) { h.ready.Store(ready) }

// BeginShutdown makes readiness start failing while the server drains.
func (h *Health) BeginShutdown() { h.shuttingDown.Store(true) }

// Liveness reports that the process is running.
func (h *Health) Liveness(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}

// checkResult is one readiness probe's outcome.
type checkResult struct {
	Name    string `json:"name"`
	OK      bool   `json:"ok"`
	Detail  string `json:"detail,omitempty"`
	Elapsed string `json:"elapsed"`
}

// Readiness reports whether this node should receive traffic.
func (h *Health) Readiness(w http.ResponseWriter, r *http.Request) {
	// The probe is bounded well below any sensible probe timeout, so a hung
	// dependency produces a fast "not ready" rather than a hung probe.
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()

	var checks []checkResult
	healthy := true

	record := func(name string, fn func() error) {
		start := time.Now()
		err := fn()
		result := checkResult{Name: name, OK: err == nil, Elapsed: time.Since(start).String()}
		if err != nil {
			result.Detail = err.Error()
			healthy = false
		}
		checks = append(checks, result)
	}

	if h.shuttingDown.Load() {
		checks = append(checks, checkResult{
			Name: "shutdown", OK: false, Detail: "this node is draining", Elapsed: "0s",
		})
		healthy = false
	} else if !h.ready.Load() {
		checks = append(checks, checkResult{
			Name: "startup", OK: false, Detail: "startup has not completed", Elapsed: "0s",
		})
		healthy = false
	}

	record("database", func() error { return h.pool.Ping(ctx) })

	record("migrations", func() error {
		pending, err := migrate.Pending(ctx, h.pool)
		if err != nil {
			return err
		}
		var outstanding []string
		for _, s := range pending {
			if !s.Applied {
				outstanding = append(outstanding, s.Name)
			}
		}
		if len(outstanding) > 0 {
			return &notReadyError{"pending migrations: " + strings.Join(outstanding, ", ")}
		}
		return nil
	})

	record("storage", func() error {
		if h.storage == nil {
			return &notReadyError{"no storage driver configured"}
		}
		if _, _, err := h.storage.Usage(ctx); err != nil {
			return err
		}
		return nil
	})

	status := http.StatusOK
	if !healthy {
		status = http.StatusServiceUnavailable
	}
	body, _ := json.Marshal(map[string]any{
		"status":  map[bool]string{true: "ready", false: "not ready"}[healthy],
		"version": h.version,
		"checks":  checks,
	})
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

type notReadyError struct{ msg string }

func (e *notReadyError) Error() string { return e.msg }

// clientIP extracts the peer address for logging.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
