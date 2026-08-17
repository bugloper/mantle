// Package distribution implements the OCI Distribution Specification HTTP
// surface. It is the only package that speaks /v2.
//
// The dependency rule from §7.2 applies here and is enforced by a test in
// test/architecture: this package may import catalog, storage, auth, oci, and
// events, and nothing may import it. In particular ledger, gc, retention, and
// admin must never appear in its import graph, so that no product feature can
// become a dependency of the pull path (principle 2, NG-1).
//
// The ledger is reached through the narrow events.Sink interface rather
// than by importing the ledger package, which is what keeps that rule true
// while still recording every pull.
package distribution

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/mantle-sh/mantle/internal/auth/identity"
	"github.com/mantle-sh/mantle/internal/auth/token"
	"github.com/mantle-sh/mantle/internal/catalog"
	"github.com/mantle-sh/mantle/internal/events"
	"github.com/mantle-sh/mantle/internal/observability"
	"github.com/mantle-sh/mantle/internal/oci"
	"github.com/mantle-sh/mantle/internal/storage/driver"
)

// Options configures a Service.
type Options struct {
	Catalog    *catalog.Store
	Identities *identity.Store
	Storage    driver.Driver
	Verifier   *token.Verifier
	Issuer     *token.Issuer
	Metrics    *observability.Metrics
	Logger     *slog.Logger
	Events     events.Sink

	// Realm and Service are advertised in the WWW-Authenticate challenge.
	Realm   string
	Service string

	AnonymousPull bool
	// DefaultOrg receives repositories pushed under a single-component name.
	DefaultOrg string

	Limits            oci.Limits
	MaxBlobSize       int64
	UploadSessionTTL  time.Duration
	MaxUploadsPerID   int
	PaginationDefault int
	PaginationMax     int

	// PresignTTL bounds a redirect URL's lifetime. It is never allowed to
	// outlive the token that authorised the request (§7.3).
	PresignTTL time.Duration
	// RedirectBlobs enables 307 redirects to presigned URLs where the driver
	// supports them.
	RedirectBlobs bool
}

// Service is the /v2 HTTP surface.
type Service struct {
	opts   Options
	routes []route
}

// NewService wires the distribution surface.
func NewService(opts Options) *Service {
	if opts.Events == nil {
		opts.Events = events.Discard{}
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.PaginationDefault <= 0 {
		opts.PaginationDefault = 100
	}
	if opts.PaginationMax < opts.PaginationDefault {
		opts.PaginationMax = opts.PaginationDefault
	}
	if opts.MaxUploadsPerID <= 0 {
		opts.MaxUploadsPerID = 32
	}
	if opts.PresignTTL <= 0 {
		opts.PresignTTL = 5 * time.Minute
	}
	s := &Service{opts: opts}
	s.routes = s.buildRoutes()
	return s
}

// logger returns the request logger.
func (s *Service) logger(ctx context.Context) *slog.Logger {
	if id := observability.RequestID(ctx); id != "" {
		return s.opts.Logger.With("request_id", id)
	}
	return s.opts.Logger
}

// observeBytesOut records response bytes against an endpoint class.
func (s *Service) observeBytesOut(endpoint string, n int64) {
	if s.opts.Metrics != nil && n > 0 {
		s.opts.Metrics.BytesOut.WithLabelValues(endpoint).Add(float64(n))
	}
}

func (s *Service) observeBytesIn(endpoint string, n int64) {
	if s.opts.Metrics != nil && n > 0 {
		s.opts.Metrics.BytesIn.WithLabelValues(endpoint).Add(float64(n))
	}
}

// ServeHTTP dispatches a /v2 request.
func (s *Service) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.dispatch(w, r)
}
