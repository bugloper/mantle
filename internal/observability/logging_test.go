package observability

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/mantle-sh/mantle/internal/auth/identity"
)

// SEC-12 names this test specifically: no known secret prefix may appear in any
// emitted log line. The scrubber runs on every attribute of every record rather
// than at call sites, because a scrubber a caller has to remember is a scrubber
// missing from the one line that mattered.
func TestNoSecretReachesTheLog(t *testing.T) {
	var buffer bytes.Buffer
	logger := NewLogger(&buffer, "json", "debug")

	// Real credentials, generated the way the system generates them.
	var secrets []string
	for _, kind := range []identity.Kind{
		identity.KindPAT, identity.KindDeployToken, identity.KindRobot,
	} {
		credential, err := identity.GenerateCredential(kind)
		if err != nil {
			t.Fatal(err)
		}
		secrets = append(secrets, credential.Plaintext)
	}

	// Every shape a secret plausibly reaches a log line in: as a value under a
	// sensitive key, as a value under an innocuous key, embedded in a message,
	// inside a bearer header, and inside a URL's userinfo.
	for _, secret := range secrets {
		logger.Info("authenticating", "authorization", "Bearer "+secret)
		logger.Info("authenticating", "token", secret)
		logger.Info("authenticating", "password", "hunter2")
		logger.Info("authenticating", "detail", "presented credential "+secret)
		logger.Info("authenticating", "header", "Bearer "+secret)
		logger.Info("authenticating", "url", "postgres://mantle:s3cr3t@localhost/mantle")
		logger.Error("push failed", "error", "upstream rejected "+secret)
	}

	output := buffer.String()
	for _, secret := range secrets {
		if strings.Contains(output, secret) {
			t.Errorf("a credential reached the log:\n  secret: %s\n  output: %s", secret, output)
		}
	}
	for _, prefix := range identity.AllPrefixes {
		// The prefix may survive inside the redaction marker's neighbours, but a
		// prefix followed by credential-shaped characters must not.
		for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
			idx := strings.Index(line, prefix)
			if idx < 0 {
				continue
			}
			remainder := line[idx+len(prefix):]
			if len(remainder) > 4 && remainder[0] != '"' {
				t.Errorf("a value with the %s prefix survived scrubbing: %s", prefix, line)
			}
		}
	}
	if strings.Contains(output, "hunter2") {
		t.Errorf("a password reached the log: %s", output)
	}
	if strings.Contains(output, "s3cr3t") {
		t.Errorf("a URL password reached the log: %s", output)
	}
}

// The scrubber must not be so aggressive that logs stop being useful.
func TestScrubberPreservesOrdinaryFields(t *testing.T) {
	var buffer bytes.Buffer
	logger := NewLogger(&buffer, "json", "info")

	logger.Info("stored manifest",
		"repository", "acme/web",
		"digest", "sha256:0123456789abcdef",
		"tag", "v1.2.3",
		"actor", "builder",
		"duration_ms", 42)

	var record map[string]any
	if err := json.Unmarshal(buffer.Bytes(), &record); err != nil {
		t.Fatalf("log line is not valid JSON: %v", err)
	}
	for key, want := range map[string]any{
		"repository": "acme/web",
		"digest":     "sha256:0123456789abcdef",
		"tag":        "v1.2.3",
		"actor":      "builder",
	} {
		if got := record[key]; got != want {
			t.Errorf("%s = %v, want %v — the scrubber is redacting ordinary fields", key, got, want)
		}
	}
}

// Sensitive keys are matched case-insensitively and by substring, since header
// names arrive in whatever case the client sent.
func TestSensitiveKeyMatchingIsCaseInsensitive(t *testing.T) {
	var buffer bytes.Buffer
	logger := NewLogger(&buffer, "json", "info")

	for _, key := range []string{
		"Authorization", "AUTHORIZATION", "proxy_authorization",
		"Secret", "api_key", "Set-Cookie", "user_password",
	} {
		buffer.Reset()
		logger.Info("probe", key, "extremely-sensitive-value")
		if strings.Contains(buffer.String(), "extremely-sensitive-value") {
			t.Errorf("the value under key %q was not redacted: %s", key, buffer.String())
		}
	}
}

func TestScrubString(t *testing.T) {
	cases := map[string]struct {
		input       string
		mustNotHave string
	}{
		"bearer token":  {"Authorization: Bearer abcdef0123456789xyz", "abcdef0123456789xyz"},
		"basic header":  {"got Basic YWRtaW46aHVudGVyMg==", "YWRtaW46aHVudGVyMg=="},
		"url userinfo":  {"dial postgres://mantle:swordfish@db:5432/mantle", "swordfish"},
		"mantle deploy": {"token mantle_dep_0123456789abcdef_secretvalue rejected", "secretvalue"},
	}
	for name, tc := range cases {
		got, changed := ScrubString(tc.input)
		if !changed {
			t.Errorf("%s: nothing was scrubbed in %q", name, tc.input)
		}
		if strings.Contains(got, tc.mustNotHave) {
			t.Errorf("%s: %q survived scrubbing, result was %q", name, tc.mustNotHave, got)
		}
	}

	// Ordinary text must pass through untouched, or every log line pays for it.
	if _, changed := ScrubString("pushed acme/web:v1.2.3 (sha256:abc123)"); changed {
		t.Error("ordinary text was modified by the scrubber")
	}
}
