package observability

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
)

// Metrics is Mantle's Prometheus instrumentation (§16.2).
//
// Endpoint labels are the endpoint *class* — "manifest_get", "blob_put" — never
// the request path. A label containing a repository name or a digest would
// produce unbounded cardinality, and a metrics endpoint that grows with the
// catalog is a memory leak with a scrape interval.
type Metrics struct {
	registry *prometheus.Registry

	RequestsTotal   *prometheus.CounterVec
	RequestDuration *prometheus.HistogramVec
	BytesIn         *prometheus.CounterVec
	BytesOut        *prometheus.CounterVec

	UploadsActive    prometheus.Gauge
	UploadsAbandoned prometheus.Counter

	StorageBytes *prometheus.GaugeVec

	GCPhaseDuration *prometheus.HistogramVec
	GCObjects       *prometheus.CounterVec
	GCRuns          *prometheus.CounterVec

	AuthFailures *prometheus.CounterVec
	TokensIssued prometheus.Counter

	LedgerQueueDepth   prometheus.Gauge
	LedgerEventsQueued prometheus.Counter
	// LedgerEventsDropped counts pull events discarded because the queue was
	// full. A nonzero rate here is an analytics gap and explicitly not an
	// error: dropping is the designed behaviour, since a slow pull is a failed
	// deploy (REQ-LEDGER-02).
	LedgerEventsDropped prometheus.Counter

	WorkerIsLeader prometheus.Gauge
	DBPoolInUse    prometheus.Gauge
	DBPoolTotal    prometheus.Gauge
}

// NewMetrics registers and returns the metric set.
func NewMetrics() *Metrics {
	registry := prometheus.NewRegistry()
	registry.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)

	m := &Metrics{
		registry: registry,

		RequestsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "mantle_http_requests_total",
			Help: "HTTP requests by endpoint class, method, and status.",
		}, []string{"endpoint", "method", "status"}),

		RequestDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "mantle_http_request_duration_seconds",
			Help: "HTTP request duration by endpoint class.",
			// Buckets are tuned to the §16.3 targets: manifest GET p99 under
			// 50 ms and token issue p99 under 100 ms both need resolution well
			// below the default 0.005–10 spread.
			Buckets: []float64{0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30},
		}, []string{"endpoint", "method"}),

		BytesIn: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "mantle_bytes_received_total",
			Help: "Bytes received, by endpoint class.",
		}, []string{"endpoint"}),

		BytesOut: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "mantle_bytes_sent_total",
			Help: "Bytes sent, by endpoint class.",
		}, []string{"endpoint"}),

		UploadsActive: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "mantle_uploads_active",
			Help: "Upload sessions currently open.",
		}),

		UploadsAbandoned: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "mantle_uploads_abandoned_total",
			Help: "Upload sessions expired and reclaimed without completing.",
		}),

		StorageBytes: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "mantle_storage_bytes",
			Help: "Stored bytes by state: available, quarantined, or reclaimable.",
		}, []string{"state"}),

		GCPhaseDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "mantle_gc_phase_duration_seconds",
			Help:    "Garbage collection phase duration.",
			Buckets: prometheus.ExponentialBuckets(0.01, 3, 10),
		}, []string{"phase"}),

		GCObjects: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "mantle_gc_objects_total",
			Help: "Objects processed by garbage collection, by action.",
		}, []string{"action", "kind"}),

		GCRuns: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "mantle_gc_runs_total",
			Help: "Garbage collection runs by outcome.",
		}, []string{"outcome"}),

		AuthFailures: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "mantle_auth_failures_total",
			Help: "Authentication and authorization failures by reason.",
		}, []string{"reason"}),

		TokensIssued: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "mantle_tokens_issued_total",
			Help: "Registry tokens issued.",
		}),

		LedgerQueueDepth: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "mantle_ledger_queue_depth",
			Help: "Pull events buffered and awaiting a batched write.",
		}),

		LedgerEventsQueued: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "mantle_ledger_events_queued_total",
			Help: "Pull events accepted into the ledger queue.",
		}),

		LedgerEventsDropped: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "mantle_ledger_events_dropped_total",
			Help: "Pull events dropped because the ledger queue was full. " +
				"Expected under load; a pull is never slowed to record one.",
		}),

		WorkerIsLeader: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "mantle_worker_is_leader",
			Help: "1 when this node holds the background worker leader lock.",
		}),

		DBPoolInUse: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "mantle_db_pool_connections_in_use",
			Help: "Database connections currently checked out.",
		}),

		DBPoolTotal: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "mantle_db_pool_connections_total",
			Help: "Database connections currently open.",
		}),
	}

	registry.MustRegister(
		m.RequestsTotal, m.RequestDuration, m.BytesIn, m.BytesOut,
		m.UploadsActive, m.UploadsAbandoned, m.StorageBytes,
		m.GCPhaseDuration, m.GCObjects, m.GCRuns,
		m.AuthFailures, m.TokensIssued,
		m.LedgerQueueDepth, m.LedgerEventsQueued, m.LedgerEventsDropped,
		m.WorkerIsLeader, m.DBPoolInUse, m.DBPoolTotal,
	)
	return m
}

// Registry exposes the Prometheus registry for the metrics handler.
func (m *Metrics) Registry() *prometheus.Registry { return m.registry }
