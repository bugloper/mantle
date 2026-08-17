// Package observability provides Mantle's logging, metrics, and request
// correlation (§16.2).
package observability

import (
	"context"
	"io"
	"log/slog"
	"regexp"
	"strings"

	"github.com/mantle-sh/mantle/internal/auth/identity"
)

// requestIDKey carries the per-request correlation id through context.
type contextKey int

const (
	requestIDKey contextKey = iota
	loggerKey
)

// NewLogger builds the process logger.
func NewLogger(w io.Writer, format, level string) *slog.Logger {
	var lvl slog.Level
	switch strings.ToLower(level) {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}

	options := &slog.HandlerOptions{Level: lvl, ReplaceAttr: scrubAttr}
	var handler slog.Handler
	if format == "text" {
		handler = slog.NewTextHandler(w, options)
	} else {
		handler = slog.NewJSONHandler(w, options)
	}
	return slog.New(handler)
}

// sensitiveKeys are attribute names whose values are replaced wholesale. Keys
// are matched case-insensitively and by substring, so "authorization",
// "Authorization" and "proxy_authorization" are all caught.
var sensitiveKeys = []string{
	"authorization", "password", "secret", "token", "credential",
	"api_key", "apikey", "private_key", "cookie", "set-cookie",
}

// bearerPattern catches a credential embedded in a longer string, such as a URL
// with basic-auth userinfo or an error message quoting a header.
var (
	bearerPattern     = regexp.MustCompile(`(?i)\b(bearer|basic)\s+[A-Za-z0-9._~+/=-]{8,}`)
	userInfoPattern   = regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.-]*://)[^/\s@]+:[^/\s@]+@`)
	mantleTokenRegexp = regexp.MustCompile(`mantle_(pat|dep|rob)_[A-Za-z0-9_-]+`)
)

// Redacted is the placeholder written in place of a secret.
const Redacted = "[REDACTED]"

// scrubAttr is the central log scrubber (SEC-12).
//
// It runs on every attribute of every record, so no call site can forget it.
// That is the only design that works: a scrubber the logging call has to opt
// into is a scrubber that is missing from the one log line that mattered.
func scrubAttr(groups []string, a slog.Attr) slog.Attr {
	lower := strings.ToLower(a.Key)
	for _, key := range sensitiveKeys {
		if strings.Contains(lower, key) {
			return slog.String(a.Key, Redacted)
		}
	}
	if a.Value.Kind() == slog.KindString {
		if scrubbed, changed := ScrubString(a.Value.String()); changed {
			return slog.String(a.Key, scrubbed)
		}
	}
	return a
}

// ScrubString removes credentials from free text. It is exported so that error
// messages and CLI output can be scrubbed on the same rules as logs.
func ScrubString(s string) (string, bool) {
	original := s
	if identity.LooksLikeCredential(s) {
		s = mantleTokenRegexp.ReplaceAllString(s, Redacted)
	}
	if bearerPattern.MatchString(s) {
		s = bearerPattern.ReplaceAllStringFunc(s, func(match string) string {
			scheme := strings.Fields(match)[0]
			return scheme + " " + Redacted
		})
	}
	if userInfoPattern.MatchString(s) {
		s = userInfoPattern.ReplaceAllString(s, "${1}"+Redacted+"@")
	}
	return s, s != original
}

// WithRequestID attaches a correlation id to a context.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey, id)
}

// RequestID returns the correlation id, or "" if none.
func RequestID(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey).(string)
	return id
}

// WithLogger attaches a logger to a context.
func WithLogger(ctx context.Context, logger *slog.Logger) context.Context {
	return context.WithValue(ctx, loggerKey, logger)
}

// Logger returns the context's logger, falling back to the default. It never
// returns nil, so callers need no nil check on a path where a missing logger
// would otherwise panic mid-request.
func Logger(ctx context.Context) *slog.Logger {
	if logger, ok := ctx.Value(loggerKey).(*slog.Logger); ok && logger != nil {
		return logger
	}
	return slog.Default()
}
