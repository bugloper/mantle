package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The shipped defaults must be safe to run with, unchanged. §17 states that a
// default which is unsafe when unchanged is a bug, so it is checked rather than
// assumed — this is the configuration an operator who reads nothing gets.
func TestDefaultsAreSafe(t *testing.T) {
	cfg := Default()

	if cfg.Auth.AnonymousPull {
		t.Error("anonymous pull is on by default")
	}
	if cfg.Storage.Filesystem.MinFree <= 0 {
		t.Error("no free-space floor by default; a full disk would be discovered by failing")
	}
	if cfg.GC.GracePeriod.Std() < MinGracePeriod {
		t.Errorf("default grace period %s is below the enforced minimum %s",
			cfg.GC.GracePeriod, MinGracePeriod)
	}
	if cfg.GC.QuarantinePeriod.Std() < 24*time.Hour {
		t.Error("the default quarantine window is under a day; a mistaken deletion " +
			"would stop being recoverable too quickly")
	}
	if cfg.Auth.TokenTTL.Std() > time.Hour {
		t.Error("the default token TTL exceeds an hour, so revocation would lag too long")
	}
	if cfg.Retention.MaxBatchFraction <= 0 || cfg.Retention.MaxBatchFraction > 1 {
		t.Error("the retention batch fraction default is out of range")
	}
	if cfg.Limits.MaxBlobSize <= 0 || cfg.Limits.MaxManifestSize <= 0 {
		t.Error("size limits are unbounded by default")
	}
	if !strings.HasPrefix(cfg.Observability.MetricsListen, "127.0.0.1") {
		t.Errorf("metrics listen defaults to %q; it should be loopback so metrics "+
			"are not exposed alongside the registry", cfg.Observability.MetricsListen)
	}

	// Defaults alone are not a complete configuration — a domain is required —
	// but everything that *is* set must be valid.
	cfg.Server.Domain = "registry.example.com"
	cfg.Server.TLS.Email = "ops@example.com"
	if err := cfg.Validate(); err != nil {
		t.Errorf("the defaults do not validate: %v", err)
	}
}

// Validation collects every problem in one pass, because fixing a config file
// one restart at a time is a bad afternoon.
func TestValidateReportsEveryProblemAtOnce(t *testing.T) {
	cfg := Default()
	cfg.Server.Domain = ""
	cfg.Server.TLS.Mode = "nonsense"
	cfg.Database.MaxConnections = 0
	cfg.Storage.Driver = "gopher"
	cfg.GC.GracePeriod = Duration(time.Minute)
	cfg.Observability.LogLevel = "chatty"

	err := cfg.Validate()
	if err == nil {
		t.Fatal("an invalid configuration validated")
	}
	errs, ok := err.(ValidationErrors)
	if !ok {
		t.Fatalf("error is %T, want ValidationErrors", err)
	}
	if len(errs) < 5 {
		t.Errorf("reported %d problems, want at least 5: %v", len(errs), err)
	}

	// Each message must name the key, so an operator can find it in the file.
	message := err.Error()
	for _, key := range []string{
		"server.tls.mode", "database.max_connections",
		"storage.driver", "gc.grace_period", "observability.log_level",
	} {
		if !strings.Contains(message, key) {
			t.Errorf("the error does not name %s:\n%s", key, message)
		}
	}
}

// The grace period floor is the mechanism preventing collection from racing an
// in-flight push, so configuration must not be able to disable it.
func TestGracePeriodFloorIsEnforced(t *testing.T) {
	cfg := Default()
	cfg.Server.Domain = "registry.example.com"
	cfg.Server.TLS.Email = "ops@example.com"
	cfg.GC.GracePeriod = Duration(time.Second)

	err := cfg.Validate()
	if err == nil {
		t.Fatal("a one-second grace period was accepted")
	}
	if !strings.Contains(err.Error(), "gc.grace_period") {
		t.Errorf("the error does not name the offending key: %v", err)
	}
}

// A database password must never appear in an error message or a log line
// (SEC-12), and configuration errors are among the most widely pasted strings
// in any support channel.
func TestDatabaseURLIsRedacted(t *testing.T) {
	cfg := Default()
	cfg.Database.URL = "postgres://mantle:swordfish@db.example.com:5432/mantle?sslmode=require"

	redacted := cfg.RedactedDatabaseURL()
	if strings.Contains(redacted, "swordfish") {
		t.Errorf("the password survived redaction: %s", redacted)
	}
	// The rest must survive, or the message stops being diagnostic.
	for _, part := range []string{"mantle", "db.example.com", "5432"} {
		if !strings.Contains(redacted, part) {
			t.Errorf("redaction removed %q, leaving an unhelpful message: %s", part, redacted)
		}
	}

	// An unparseable URL must still be redacted rather than passed through.
	cfg.Database.URL = "postgres://mantle:swordfish@ho st/mantle"
	if strings.Contains(cfg.RedactedDatabaseURL(), "swordfish") {
		t.Errorf("the password survived redaction of a malformed URL: %s", cfg.RedactedDatabaseURL())
	}
}

func TestLoadAppliesFileThenEnvironment(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mantle.yaml")
	content := `
server:
  domain: from-file.example.com
  tls:
    mode: "off"
limits:
  max_blob_size: 2GiB
gc:
  grace_period: 48h
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("loading config: %v", err)
	}
	if cfg.Server.Domain != "from-file.example.com" {
		t.Errorf("domain = %q, want the value from the file", cfg.Server.Domain)
	}
	if cfg.Limits.MaxBlobSize != 2<<30 {
		t.Errorf("max_blob_size = %d, want 2GiB parsed from the file", cfg.Limits.MaxBlobSize)
	}
	if cfg.GC.GracePeriod.Std() != 48*time.Hour {
		t.Errorf("grace_period = %s, want 48h", cfg.GC.GracePeriod)
	}
	// Untouched keys keep their defaults.
	if cfg.Limits.MaxLayers != Default().Limits.MaxLayers {
		t.Error("a key absent from the file lost its default")
	}

	// The environment overrides the file.
	t.Setenv("MANTLE_SERVER_DOMAIN", "from-env.example.com")
	t.Setenv("MANTLE_LIMITS_MAX_BLOB_SIZE", "5GiB")
	cfg, err = Load(path)
	if err != nil {
		t.Fatalf("loading config with environment overrides: %v", err)
	}
	if cfg.Server.Domain != "from-env.example.com" {
		t.Errorf("domain = %q, want the environment to win over the file", cfg.Server.Domain)
	}
	if cfg.Limits.MaxBlobSize != 5<<30 {
		t.Errorf("max_blob_size = %d, want 5GiB from the environment", cfg.Limits.MaxBlobSize)
	}
}

// A misspelled key that is silently ignored means an operator believes they set
// a limit they did not set.
func TestUnknownConfigKeyIsRejected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mantle.yaml")
	if err := os.WriteFile(path, []byte("server:\n  domian: typo.example.com\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("a misspelled configuration key was accepted")
	}
}

// The realm and service are derived from the domain when unset, and they must
// match what the token endpoint expects or clients reject the token they were
// just handed.
func TestDerivedAuthValues(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mantle.yaml")
	if err := os.WriteFile(path, []byte(
		"server:\n  domain: registry.example.com\n  tls:\n    mode: \"off\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Auth.Service != "registry.example.com" {
		t.Errorf("service = %q, want the domain", cfg.Auth.Service)
	}
	// With TLS off the realm must be http, or the client cannot reach it.
	if cfg.Auth.Realm != "http://registry.example.com/auth/token" {
		t.Errorf("realm = %q, want an http realm when TLS is off", cfg.Auth.Realm)
	}
}

func TestParseBytes(t *testing.T) {
	cases := map[string]Bytes{
		"1024": 1024, "1KiB": 1 << 10, "10GiB": 10 << 30,
		"4MiB": 4 << 20, "1MB": 1_000_000, "2GB": 2_000_000_000,
		"1.5GiB": Bytes(1.5 * (1 << 30)), "512B": 512,
	}
	for input, want := range cases {
		got, err := ParseBytes(input)
		if err != nil {
			t.Errorf("ParseBytes(%q) = %v", input, err)
			continue
		}
		if got != want {
			t.Errorf("ParseBytes(%q) = %d, want %d", input, got, want)
		}
	}
	for _, input := range []string{"", "abc", "-5GiB", "GiB", "5 potatoes"} {
		if _, err := ParseBytes(input); err == nil {
			t.Errorf("ParseBytes(%q) succeeded, want an error", input)
		}
	}
}

func TestParseDuration(t *testing.T) {
	cases := map[string]time.Duration{
		"30s": 30 * time.Second, "5m": 5 * time.Minute, "24h": 24 * time.Hour,
		"7d": 7 * 24 * time.Hour, "2w": 14 * 24 * time.Hour, "90d": 90 * 24 * time.Hour,
	}
	for input, want := range cases {
		got, err := ParseDuration(input)
		if err != nil {
			t.Errorf("ParseDuration(%q) = %v", input, err)
			continue
		}
		if got.Std() != want {
			t.Errorf("ParseDuration(%q) = %s, want %s", input, got, want)
		}
	}
	for _, input := range []string{"", "later", "-5h", "5 days"} {
		if _, err := ParseDuration(input); err == nil {
			t.Errorf("ParseDuration(%q) succeeded, want an error", input)
		}
	}
}

func TestParseSchedule(t *testing.T) {
	// A fixed reference time; the schedule parser must not consult the clock.
	base := time.Date(2026, 8, 17, 10, 30, 0, 0, time.UTC)

	cases := []struct {
		expr string
		want time.Time
	}{
		{"0 3 * * *", time.Date(2026, 8, 18, 3, 0, 0, 0, time.UTC)},
		{"*/15 * * * *", time.Date(2026, 8, 17, 10, 45, 0, 0, time.UTC)},
		{"0 * * * *", time.Date(2026, 8, 17, 11, 0, 0, 0, time.UTC)},
		{"@daily", time.Date(2026, 8, 18, 0, 0, 0, 0, time.UTC)},
		{"30 10 * * *", time.Date(2026, 8, 18, 10, 30, 0, 0, time.UTC)},
		{"0 0 1 * *", time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)},
	}
	for _, tc := range cases {
		schedule, err := ParseSchedule(tc.expr)
		if err != nil {
			t.Errorf("ParseSchedule(%q) = %v", tc.expr, err)
			continue
		}
		if got := schedule.Next(base); !got.Equal(tc.want) {
			t.Errorf("ParseSchedule(%q).Next = %s, want %s", tc.expr, got, tc.want)
		}
	}

	for _, expr := range []string{"", "0 3 * *", "99 3 * * *", "0 3 * * funday", "a b c d e"} {
		if _, err := ParseSchedule(expr); err == nil {
			t.Errorf("ParseSchedule(%q) succeeded, want an error", expr)
		}
	}
}

// Cron's day rule: when both day fields are restricted, a day matching either
// one matches. It is an inconsistency inherited from Vixie cron that every
// implementation must reproduce or surprise people.
func TestScheduleDayOfMonthAndWeekAreUnionWhenBothRestricted(t *testing.T) {
	schedule, err := ParseSchedule("0 0 13 * 5") // the 13th, or any Friday
	if err != nil {
		t.Fatal(err)
	}
	// 2026-08-17 is a Monday. The next Friday is the 21st; the 13th has passed.
	base := time.Date(2026, 8, 17, 10, 0, 0, 0, time.UTC)
	got := schedule.Next(base)
	want := time.Date(2026, 8, 21, 0, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("Next = %s, want %s (either the 13th or a Friday)", got, want)
	}
}
