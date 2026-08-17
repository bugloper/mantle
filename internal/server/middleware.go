// Package server assembles Mantle's HTTP surfaces into one listener: the OCI
// distribution API, the token service, the admin API, and health endpoints.
package server

import (
	"crypto/rand"
	"encoding/base64"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strconv"
	"time"

	"github.com/mantle-sh/mantle/internal/distribution"
	ocierrors "github.com/mantle-sh/mantle/internal/distribution/errors"
	"github.com/mantle-sh/mantle/internal/observability"
)

// statusRecorder captures the response status and size for logging and metrics.
type statusRecorder struct {
	http.ResponseWriter
	status  int
	written int64
	// wroteHeader guards against a double WriteHeader, which http would log as
	// "superfluous" and which would otherwise misreport the recorded status.
	wroteHeader bool
}

func (s *statusRecorder) WriteHeader(status int) {
	if s.wroteHeader {
		return
	}
	s.status = status
	s.wroteHeader = true
	s.ResponseWriter.WriteHeader(status)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if !s.wroteHeader {
		s.WriteHeader(http.StatusOK)
	}
	n, err := s.ResponseWriter.Write(b)
	s.written += int64(n)
	return n, err
}

// Flush forwards to the underlying writer when it supports flushing. Without
// this, wrapping the writer would silently disable streaming for large blob
// responses.
func (s *statusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap lets http.ResponseController reach the underlying writer, which
// ServeContent uses for range handling.
func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }

// RequestID assigns a correlation id to every request and echoes it back.
//
// An inbound X-Request-Id is honoured so a trace started at a load balancer
// carries through, but it is length-bounded and re-generated if implausible:
// the value ends up in logs, and an unbounded client-controlled string in a log
// line is a log-injection vector.
func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-Id")
		if !plausibleRequestID(id) {
			id = newRequestID()
		}
		r.Header.Set("X-Request-Id", id)
		w.Header().Set("X-Request-Id", id)
		next.ServeHTTP(w, r.WithContext(observability.WithRequestID(r.Context(), id)))
	})
}

func plausibleRequestID(id string) bool {
	if len(id) == 0 || len(id) > 64 {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		alphanumeric := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
		if !alphanumeric && c != '-' && c != '_' {
			return false
		}
	}
	return true
}

func newRequestID() string {
	raw := make([]byte, 12)
	if _, err := rand.Read(raw); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return base64.RawURLEncoding.EncodeToString(raw)
}

// Recover turns a panic into a 500 rather than a dropped connection.
//
// A panic in one request must not take the process down: on a registry, that
// would fail every concurrent pull, and the pull path is the one place where an
// outage is a production incident for the customer (principle 2).
func Recover(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				recovered := recover()
				if recovered == nil {
					return
				}
				// A client disconnecting mid-write surfaces as this sentinel
				// and is not an error worth a stack trace.
				if recovered == http.ErrAbortHandler {
					panic(recovered)
				}
				requestLogger(logger, r).Error("panic while handling request",
					"panic", recovered,
					"method", r.Method,
					"path", r.URL.Path,
					"stack", string(debug.Stack()))
				ocierrors.ServeJSON(w, ocierrors.New(ocierrors.CodeUnknown,
					"the registry encountered an internal error (request "+
						observability.RequestID(r.Context())+")"), nil)
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// Observe records metrics and an access log line for every request.
func Observe(metrics *observability.Metrics, logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			recorder := &statusRecorder{ResponseWriter: w, status: http.StatusOK}

			next.ServeHTTP(recorder, r)

			duration := time.Since(start)
			endpoint := distribution.EndpointClass(r.Context())

			if metrics != nil {
				metrics.RequestsTotal.
					WithLabelValues(endpoint, r.Method, strconv.Itoa(recorder.status)).Inc()
				metrics.RequestDuration.
					WithLabelValues(endpoint, r.Method).Observe(duration.Seconds())
			}

			// Successful reads are logged at debug: a busy registry serves
			// thousands per minute and an info-level line for each would drown
			// the events an operator actually needs to see.
			level := slog.LevelInfo
			switch {
			case recorder.status >= 500:
				level = slog.LevelError
			case recorder.status < 400 && (r.Method == http.MethodGet || r.Method == http.MethodHead):
				level = slog.LevelDebug
			}

			requestLogger(logger, r).Log(r.Context(), level, "request",
				"method", r.Method,
				"path", r.URL.Path,
				"endpoint", endpoint,
				"status", recorder.status,
				"bytes", recorder.written,
				"duration_ms", duration.Milliseconds(),
				"user_agent", r.UserAgent(),
			)
		})
	}
}

// Chain applies middleware in the order given, so the first listed is outermost.
func Chain(handler http.Handler, middleware ...func(http.Handler) http.Handler) http.Handler {
	for i := len(middleware) - 1; i >= 0; i-- {
		handler = middleware[i](handler)
	}
	return handler
}

// requestLogger attaches the correlation id to a logger, so every line emitted
// while handling a request can be joined back to it.
func requestLogger(logger *slog.Logger, r *http.Request) *slog.Logger {
	if id := observability.RequestID(r.Context()); id != "" {
		return logger.With("request_id", id)
	}
	return logger
}
