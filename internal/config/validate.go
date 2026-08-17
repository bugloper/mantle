package config

import (
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"
)

// ValidationError names one bad configuration key. Errors are UI (principle 4):
// the key, what was supplied, and what is acceptable, in one line an operator
// can act on without opening the source.
type ValidationError struct {
	Key      string
	Value    any
	Expected string
}

func (e *ValidationError) Error() string {
	if e.Value == nil || e.Value == "" {
		return fmt.Sprintf("%s is not set: %s", e.Key, e.Expected)
	}
	return fmt.Sprintf("%s is %v: %s", e.Key, e.Value, e.Expected)
}

// ValidationErrors collects every problem in one pass, because fixing a config
// file one restart at a time is a bad afternoon.
type ValidationErrors []*ValidationError

func (v ValidationErrors) Error() string {
	if len(v) == 1 {
		return "invalid configuration: " + v[0].Error()
	}
	var b strings.Builder
	fmt.Fprintf(&b, "invalid configuration (%d problems):", len(v))
	for _, e := range v {
		b.WriteString("\n  - ")
		b.WriteString(e.Error())
	}
	return b.String()
}

// MinGracePeriod is the floor on gc.grace_period (§12.1). A grace period
// shorter than the longest plausible push is the classic GC race, so the
// minimum is enforced rather than merely recommended.
const MinGracePeriod = time.Hour

// Validate checks the resolved configuration.
func (c *Config) Validate() error {
	var errs ValidationErrors
	add := func(key string, value any, expected string) {
		errs = append(errs, &ValidationError{Key: key, Value: value, Expected: expected})
	}

	// --- server ---
	if c.Server.Listen == "" {
		add("server.listen", "", "a host:port such as 0.0.0.0:443")
	} else if _, _, err := net.SplitHostPort(c.Server.Listen); err != nil {
		add("server.listen", c.Server.Listen, "a host:port such as 0.0.0.0:443")
	}
	switch c.Server.TLS.Mode {
	case "acme":
		if c.Server.Domain == "" {
			add("server.domain", "", "required when server.tls.mode is acme, since the certificate is issued for it")
		}
		if c.Server.TLS.Email == "" {
			add("server.tls.email", "", "required when server.tls.mode is acme, for expiry notices from the CA")
		}
		if c.Server.TLS.CacheDir == "" {
			add("server.tls.cache_dir", "", "required when server.tls.mode is acme, or a restart loop will exhaust the CA rate limit")
		}
	case "file":
		if c.Server.TLS.Cert == "" {
			add("server.tls.cert", "", "a path to a PEM certificate chain, required when server.tls.mode is file")
		}
		if c.Server.TLS.Key == "" {
			add("server.tls.key", "", "a path to a PEM private key, required when server.tls.mode is file")
		}
	case "off":
		// Legitimate behind a terminating proxy.
	default:
		add("server.tls.mode", c.Server.TLS.Mode, "one of acme, file, off")
	}
	if c.Server.Domain == "" && c.Server.TLS.Mode != "off" {
		add("server.domain", "", "the hostname clients will use, such as registry.example.com")
	}

	// --- database ---
	if c.Database.URL == "" {
		add("database.url", "", "a PostgreSQL connection URL")
	} else if u, err := url.Parse(c.Database.URL); err != nil {
		add("database.url", redactURL(c.Database.URL), "a parseable PostgreSQL connection URL")
	} else if u.Scheme != "postgres" && u.Scheme != "postgresql" {
		add("database.url", redactURL(c.Database.URL), "a URL with the postgres:// or postgresql:// scheme")
	}
	if c.Database.MaxConnections < 2 {
		add("database.max_connections", c.Database.MaxConnections,
			"at least 2, since the worker leader holds one connection continuously")
	}
	if c.Database.MinConnections > c.Database.MaxConnections {
		add("database.min_connections", c.Database.MinConnections,
			fmt.Sprintf("no greater than database.max_connections (%d)", c.Database.MaxConnections))
	}

	// --- storage ---
	switch c.Storage.Driver {
	case "filesystem":
		if c.Storage.Filesystem.Root == "" {
			add("storage.filesystem.root", "", "a writable directory path")
		}
	case "s3":
		if c.Storage.S3.Bucket == "" {
			add("storage.s3.bucket", "", "the bucket name, required when storage.driver is s3")
		}
		if c.Storage.S3.Region == "" && c.Storage.S3.Endpoint == "" {
			add("storage.s3.region", "", "a region, or storage.s3.endpoint for a non-AWS provider")
		}
		// 5 MiB is the S3 multipart minimum for every part but the last
		// (§10.5). A smaller configured part size cannot be uploaded at all.
		if c.Storage.S3.PartSize < 5<<20 {
			add("storage.s3.part_size", c.Storage.S3.PartSize,
				"at least 5MiB, the S3 multipart minimum part size")
		}
		if c.Storage.S3.ScratchDir == "" {
			add("storage.s3.scratch_dir", "",
				"a local directory for multipart spill buffering, required when storage.driver is s3")
		}
	default:
		add("storage.driver", c.Storage.Driver, "one of filesystem, s3")
	}

	// --- auth ---
	if c.Auth.TokenTTL.Std() < 30*time.Second {
		add("auth.token_ttl", c.Auth.TokenTTL,
			"at least 30s, or clients will spend more time authenticating than transferring")
	}
	if c.Auth.TokenTTL.Std() > time.Hour {
		add("auth.token_ttl", c.Auth.TokenTTL,
			"no more than 1h, since a revoked permission remains usable until the token expires")
	}
	if c.Auth.SigningKeyPath == "" {
		add("auth.signing_key_path", "", "a path to the RS256 token signing key")
	}

	// --- limits ---
	if c.Limits.MaxManifestSize <= 0 {
		add("limits.max_manifest_size", c.Limits.MaxManifestSize, "a positive byte quantity such as 4MiB")
	}
	if c.Limits.MaxBlobSize <= 0 {
		add("limits.max_blob_size", c.Limits.MaxBlobSize, "a positive byte quantity such as 10GiB")
	}
	if c.Limits.MaxLayers < 1 {
		add("limits.max_layers", c.Limits.MaxLayers, "at least 1")
	}
	if c.Limits.MaxIndexDepth < 1 {
		add("limits.max_index_depth", c.Limits.MaxIndexDepth, "at least 1")
	}
	if c.Limits.MaxIndexEntries < 1 {
		add("limits.max_index_entries", c.Limits.MaxIndexEntries, "at least 1")
	}
	if c.Limits.PaginationDefault < 1 {
		add("limits.pagination_default", c.Limits.PaginationDefault, "at least 1")
	}
	if c.Limits.PaginationMax < c.Limits.PaginationDefault {
		add("limits.pagination_max", c.Limits.PaginationMax,
			fmt.Sprintf("at least limits.pagination_default (%d)", c.Limits.PaginationDefault))
	}
	if c.Limits.UploadSessionTTL.Std() < time.Minute {
		add("limits.upload_session_ttl", c.Limits.UploadSessionTTL,
			"at least 1m, or resumable uploads will expire mid-push")
	}

	// --- gc ---
	if c.GC.GracePeriod.Std() < MinGracePeriod {
		add("gc.grace_period", c.GC.GracePeriod,
			fmt.Sprintf("at least %s: a shorter window lets collection race an in-flight push", MinGracePeriod))
	}
	if c.GC.QuarantinePeriod.Std() < time.Hour {
		add("gc.quarantine_period", c.GC.QuarantinePeriod,
			"at least 1h: quarantine is the window in which a mistaken deletion is recoverable")
	}
	if c.GC.BatchSize < 1 {
		add("gc.batch_size", c.GC.BatchSize, "at least 1")
	}
	if c.GC.Enabled {
		if _, err := ParseSchedule(c.GC.Schedule); err != nil {
			add("gc.schedule", c.GC.Schedule, err.Error())
		}
	}

	// --- retention ---
	if c.Retention.RollbackDepth < 0 {
		add("retention.rollback_depth", c.Retention.RollbackDepth,
			"zero or more: this many generations behind the live image stay pinned")
	}
	if c.Retention.MaxBatchFraction <= 0 || c.Retention.MaxBatchFraction > 1 {
		add("retention.max_batch_fraction", c.Retention.MaxBatchFraction,
			"greater than 0 and no more than 1: the fraction of a repository one run may affect before halting")
	}

	// --- ledger ---
	if c.Ledger.QueueSize < 1 {
		add("ledger.queue_size", c.Ledger.QueueSize, "at least 1")
	}
	if c.Ledger.FlushSize < 1 {
		add("ledger.flush_size", c.Ledger.FlushSize, "at least 1")
	}
	if c.Ledger.FlushInterval.Std() <= 0 {
		add("ledger.flush_interval", c.Ledger.FlushInterval, "a positive duration such as 2s")
	}

	// --- observability ---
	switch c.Observability.LogFormat {
	case "json", "text":
	default:
		add("observability.log_format", c.Observability.LogFormat, "one of json, text")
	}
	switch c.Observability.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		add("observability.log_level", c.Observability.LogLevel, "one of debug, info, warn, error")
	}
	if c.Observability.MetricsListen != "" {
		if _, _, err := net.SplitHostPort(c.Observability.MetricsListen); err != nil {
			add("observability.metrics_listen", c.Observability.MetricsListen,
				"a host:port such as 127.0.0.1:9090, or empty to disable")
		}
	}

	if len(errs) > 0 {
		return errs
	}
	return nil
}

// redactURL strips the password from a connection URL so it can appear in an
// error message (SEC-12). A config error that prints the database password into
// a log aggregator is worse than the config error.
func redactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		// Unparseable: fall back to removing anything between "//" and "@".
		if i := strings.Index(raw, "//"); i >= 0 {
			if j := strings.Index(raw[i:], "@"); j >= 0 {
				return raw[:i+2] + "***@" + raw[i+j+1:]
			}
		}
		return raw
	}
	if u.User != nil {
		if _, hasPassword := u.User.Password(); hasPassword {
			u.User = url.UserPassword(u.User.Username(), "***")
		}
	}
	return u.String()
}

// RedactedDatabaseURL is the connection string as it may safely be logged.
func (c *Config) RedactedDatabaseURL() string { return redactURL(c.Database.URL) }
