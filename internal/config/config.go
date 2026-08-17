// Package config resolves Mantle's configuration from defaults, a YAML file,
// and the environment, and validates the result (§17).
//
// Two rules govern everything here. Resolution order is flags > environment
// (MANTLE_*) > file > defaults. And every default is chosen so that an operator
// who changes nothing has a safe, working registry — a default that is unsafe
// when unchanged is a bug, not a configuration choice.
package config

import (
	"fmt"
	"os"
	"reflect"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Config is the complete resolved configuration.
type Config struct {
	Server        Server        `yaml:"server"`
	Database      Database      `yaml:"database"`
	Storage       Storage       `yaml:"storage"`
	Auth          Auth          `yaml:"auth"`
	Limits        Limits        `yaml:"limits"`
	GC            GC            `yaml:"gc"`
	Retention     Retention     `yaml:"retention"`
	Ledger        Ledger        `yaml:"ledger"`
	Observability Observability `yaml:"observability"`
}

type Server struct {
	Domain string `yaml:"domain" env:"MANTLE_SERVER_DOMAIN"`
	Listen string `yaml:"listen" env:"MANTLE_SERVER_LISTEN"`
	TLS    TLS    `yaml:"tls"`
	// ReadHeaderTimeout bounds slow-header attacks. It is deliberately not the
	// whole-request timeout: a 10 GiB layer upload is a legitimately long request.
	ReadHeaderTimeout Duration `yaml:"read_header_timeout"`
	ShutdownGrace     Duration `yaml:"shutdown_grace"`
}

type TLS struct {
	// Mode is acme, file, or off. "off" is legitimate behind a terminating
	// proxy and is not treated as a misconfiguration.
	Mode  string `yaml:"mode" env:"MANTLE_SERVER_TLS_MODE"`
	Email string `yaml:"email" env:"MANTLE_SERVER_TLS_EMAIL"`
	Cert  string `yaml:"cert" env:"MANTLE_SERVER_TLS_CERT"`
	Key   string `yaml:"key" env:"MANTLE_SERVER_TLS_KEY"`
	// CacheDir holds ACME state across restarts. Without it, a restart loop
	// will hit Let's Encrypt rate limits within the hour.
	CacheDir string `yaml:"cache_dir" env:"MANTLE_SERVER_TLS_CACHE_DIR"`
}

type Database struct {
	URL            string `yaml:"url" env:"MANTLE_DATABASE_URL"`
	MaxConnections int    `yaml:"max_connections" env:"MANTLE_DATABASE_MAX_CONNECTIONS"`
	MinConnections int    `yaml:"min_connections" env:"MANTLE_DATABASE_MIN_CONNECTIONS"`
}

type Storage struct {
	Driver     string     `yaml:"driver" env:"MANTLE_STORAGE_DRIVER"`
	Filesystem Filesystem `yaml:"filesystem"`
	S3         S3         `yaml:"s3"`
}

type Filesystem struct {
	Root    string `yaml:"root" env:"MANTLE_STORAGE_FILESYSTEM_ROOT"`
	MinFree Bytes  `yaml:"min_free" env:"MANTLE_STORAGE_FILESYSTEM_MIN_FREE"`
}

type S3 struct {
	Bucket     string `yaml:"bucket" env:"MANTLE_STORAGE_S3_BUCKET"`
	Region     string `yaml:"region" env:"MANTLE_STORAGE_S3_REGION"`
	Endpoint   string `yaml:"endpoint" env:"MANTLE_STORAGE_S3_ENDPOINT"`
	AccessKey  string `yaml:"access_key" env:"MANTLE_STORAGE_S3_ACCESS_KEY"`
	SecretKey  string `yaml:"secret_key" env:"MANTLE_STORAGE_S3_SECRET_KEY"`
	PartSize   Bytes  `yaml:"part_size" env:"MANTLE_STORAGE_S3_PART_SIZE"`
	Redirect   bool   `yaml:"redirect" env:"MANTLE_STORAGE_S3_REDIRECT"`
	ScratchDir string `yaml:"scratch_dir" env:"MANTLE_STORAGE_S3_SCRATCH_DIR"`
}

type Auth struct {
	TokenTTL      Duration `yaml:"token_ttl" env:"MANTLE_AUTH_TOKEN_TTL"`
	AnonymousPull bool     `yaml:"anonymous_pull" env:"MANTLE_AUTH_ANONYMOUS_PULL"`
	// Realm is the token endpoint advertised in the WWW-Authenticate challenge.
	// Derived from the domain when empty.
	Realm   string `yaml:"realm" env:"MANTLE_AUTH_REALM"`
	Service string `yaml:"service" env:"MANTLE_AUTH_SERVICE"`
	// SigningKeyPath holds the RS256 private key used to sign tokens. Generated
	// at install if absent.
	SigningKeyPath string `yaml:"signing_key_path" env:"MANTLE_AUTH_SIGNING_KEY_PATH"`
	OIDC           OIDC   `yaml:"oidc"`
}

type OIDC struct {
	Enabled bool `yaml:"enabled" env:"MANTLE_AUTH_OIDC_ENABLED"`
}

type Limits struct {
	MaxBlobSize      Bytes    `yaml:"max_blob_size" env:"MANTLE_LIMITS_MAX_BLOB_SIZE"`
	MaxManifestSize  Bytes    `yaml:"max_manifest_size" env:"MANTLE_LIMITS_MAX_MANIFEST_SIZE"`
	MaxLayers        int      `yaml:"max_layers" env:"MANTLE_LIMITS_MAX_LAYERS"`
	MaxIndexDepth    int      `yaml:"max_index_depth" env:"MANTLE_LIMITS_MAX_INDEX_DEPTH"`
	MaxIndexEntries  int      `yaml:"max_index_entries" env:"MANTLE_LIMITS_MAX_INDEX_ENTRIES"`
	UploadSessionTTL Duration `yaml:"upload_session_ttl" env:"MANTLE_LIMITS_UPLOAD_SESSION_TTL"`
	// PaginationDefault and PaginationMax bound tags/list and _catalog. A client
	// asking for everything on a large registry is a memory event.
	PaginationDefault int `yaml:"pagination_default" env:"MANTLE_LIMITS_PAGINATION_DEFAULT"`
	PaginationMax     int `yaml:"pagination_max" env:"MANTLE_LIMITS_PAGINATION_MAX"`
}

type GC struct {
	Enabled          bool     `yaml:"enabled" env:"MANTLE_GC_ENABLED"`
	Schedule         string   `yaml:"schedule" env:"MANTLE_GC_SCHEDULE"`
	GracePeriod      Duration `yaml:"grace_period" env:"MANTLE_GC_GRACE_PERIOD"`
	QuarantinePeriod Duration `yaml:"quarantine_period" env:"MANTLE_GC_QUARANTINE_PERIOD"`
	BatchSize        int      `yaml:"batch_size" env:"MANTLE_GC_BATCH_SIZE"`
}

type Retention struct {
	RollbackDepth    int     `yaml:"rollback_depth" env:"MANTLE_RETENTION_ROLLBACK_DEPTH"`
	MaxBatchFraction float64 `yaml:"max_batch_fraction" env:"MANTLE_RETENTION_MAX_BATCH_FRACTION"`
	Enabled          bool    `yaml:"enabled" env:"MANTLE_RETENTION_ENABLED"`
}

type Ledger struct {
	Enabled            bool     `yaml:"enabled" env:"MANTLE_LEDGER_ENABLED"`
	InferDeployments   bool     `yaml:"infer_deployments" env:"MANTLE_LEDGER_INFER_DEPLOYMENTS"`
	PullEventRetention Duration `yaml:"pull_event_retention" env:"MANTLE_LEDGER_PULL_EVENT_RETENTION"`
	// QueueSize bounds the in-process pull-event buffer. When it fills, events
	// are dropped rather than slowing a pull (REQ-LEDGER-01/02).
	QueueSize     int      `yaml:"queue_size" env:"MANTLE_LEDGER_QUEUE_SIZE"`
	FlushInterval Duration `yaml:"flush_interval" env:"MANTLE_LEDGER_FLUSH_INTERVAL"`
	FlushSize     int      `yaml:"flush_size" env:"MANTLE_LEDGER_FLUSH_SIZE"`
}

type Observability struct {
	MetricsListen string `yaml:"metrics_listen" env:"MANTLE_OBSERVABILITY_METRICS_LISTEN"`
	LogFormat     string `yaml:"log_format" env:"MANTLE_OBSERVABILITY_LOG_FORMAT"`
	LogLevel      string `yaml:"log_level" env:"MANTLE_OBSERVABILITY_LOG_LEVEL"`
}

// Default returns the shipped defaults. Every value here is safe to run with.
func Default() Config {
	return Config{
		Server: Server{
			Listen:            "0.0.0.0:443",
			TLS:               TLS{Mode: "acme", CacheDir: "/var/lib/mantle/acme"},
			ReadHeaderTimeout: Duration(30e9), // 30s
			ShutdownGrace:     Duration(30e9), // 30s
		},
		Database: Database{
			URL:            "postgres://mantle@localhost/mantle?sslmode=require",
			MaxConnections: 25,
			MinConnections: 2,
		},
		Storage: Storage{
			Driver:     "filesystem",
			Filesystem: Filesystem{Root: "/var/lib/mantle", MinFree: 10 << 30},
			S3: S3{
				PartSize:   16 << 20,
				Redirect:   true,
				ScratchDir: "/var/lib/mantle/scratch",
			},
		},
		Auth: Auth{
			TokenTTL:       Duration(300e9), // 300s — D-06
			AnonymousPull:  false,
			SigningKeyPath: "/var/lib/mantle/keys/token.pem",
		},
		Limits: Limits{
			MaxBlobSize:       10 << 30,
			MaxManifestSize:   4 << 20,
			MaxLayers:         200,
			MaxIndexDepth:     3,
			MaxIndexEntries:   256,
			UploadSessionTTL:  Duration(24 * 3600e9),
			PaginationDefault: 100,
			PaginationMax:     1000,
		},
		GC: GC{
			Enabled:          true,
			Schedule:         "0 3 * * *",
			GracePeriod:      Duration(24 * 3600e9),
			QuarantinePeriod: Duration(168 * 3600e9),
			BatchSize:        1000,
		},
		Retention: Retention{
			Enabled:          true,
			RollbackDepth:    3,
			MaxBatchFraction: 0.25,
		},
		Ledger: Ledger{
			Enabled:            true,
			InferDeployments:   true,
			PullEventRetention: Duration(90 * 24 * 3600e9),
			QueueSize:          8192,
			FlushInterval:      Duration(2e9),
			FlushSize:          256,
		},
		Observability: Observability{
			MetricsListen: "127.0.0.1:9090",
			LogFormat:     "json",
			LogLevel:      "info",
		},
	}
}

// Load resolves configuration from the given file (which may be empty, meaning
// defaults only) and then the environment.
func Load(path string) (Config, error) {
	cfg := Default()
	if path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return cfg, fmt.Errorf("reading config %s: %w", path, err)
		}
		// KnownFields catches typos. A misspelled key that is silently ignored
		// means an operator believes they set a limit they did not set.
		dec := yaml.NewDecoder(strings.NewReader(string(data)))
		dec.KnownFields(true)
		if err := dec.Decode(&cfg); err != nil {
			return cfg, fmt.Errorf("parsing config %s: %w", path, describeYAMLError(err, path))
		}
	}
	if err := applyEnv(&cfg); err != nil {
		return cfg, err
	}
	cfg.applyDerived()
	return cfg, cfg.Validate()
}

// describeYAMLError makes yaml.v3's error mention the file, since an operator
// reading a startup failure needs to know which file to open.
func describeYAMLError(err error, path string) error {
	msg := err.Error()
	if strings.Contains(msg, "yaml:") {
		return fmt.Errorf("%s", strings.ReplaceAll(msg, "yaml:", path+":"))
	}
	return err
}

// applyEnv overlays MANTLE_* environment variables onto the config, walking the
// struct tags reflectively so that adding a field to the struct is enough to
// make it settable from the environment — a field that is silently not
// env-settable is the kind of gap containers discover in production.
func applyEnv(cfg *Config) error {
	return walkEnv(reflect.ValueOf(cfg).Elem())
}

func walkEnv(v reflect.Value) error {
	t := v.Type()
	for i := 0; i < t.NumField(); i++ {
		field, value := t.Field(i), v.Field(i)
		if field.Type.Kind() == reflect.Struct && field.Tag.Get("env") == "" {
			if err := walkEnv(value); err != nil {
				return err
			}
			continue
		}
		name := field.Tag.Get("env")
		if name == "" {
			continue
		}
		raw, ok := os.LookupEnv(name)
		if !ok {
			continue
		}
		if err := setFromString(value, raw); err != nil {
			return fmt.Errorf("environment variable %s: %w", name, err)
		}
	}
	return nil
}

func setFromString(v reflect.Value, raw string) error {
	switch v.Interface().(type) {
	case Bytes:
		b, err := ParseBytes(raw)
		if err != nil {
			return err
		}
		v.Set(reflect.ValueOf(b))
		return nil
	case Duration:
		d, err := ParseDuration(raw)
		if err != nil {
			return err
		}
		v.Set(reflect.ValueOf(d))
		return nil
	}
	switch v.Kind() {
	case reflect.String:
		v.SetString(raw)
	case reflect.Bool:
		b, err := strconv.ParseBool(raw)
		if err != nil {
			return fmt.Errorf("%q is not a boolean (use true or false)", raw)
		}
		v.SetBool(b)
	case reflect.Int, reflect.Int64:
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return fmt.Errorf("%q is not an integer", raw)
		}
		v.SetInt(n)
	case reflect.Float64:
		f, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			return fmt.Errorf("%q is not a number", raw)
		}
		v.SetFloat(f)
	default:
		return fmt.Errorf("unsupported config field kind %s", v.Kind())
	}
	return nil
}

// applyDerived fills values that default to a function of another value, after
// the file and environment have had their say.
func (c *Config) applyDerived() {
	if c.Auth.Service == "" {
		c.Auth.Service = c.Server.Domain
	}
	if c.Auth.Realm == "" && c.Server.Domain != "" {
		scheme := "https"
		if c.Server.TLS.Mode == "off" {
			scheme = "http"
		}
		c.Auth.Realm = fmt.Sprintf("%s://%s/auth/token", scheme, c.Server.Domain)
	}
}

// OCILimits projects the configured limits onto the parser's limit struct.
func (c *Config) OCILimits() (int64, int, int, int) {
	return c.Limits.MaxManifestSize.Int64(), c.Limits.MaxLayers, c.Limits.MaxIndexDepth, c.Limits.MaxIndexEntries
}
