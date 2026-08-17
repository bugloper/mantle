package integration

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/mantle-sh/mantle/internal/auth/authz"
)

// These two tests exist because a real `docker push` failed against a build
// that passed every other test in this suite. Both failures are invisible to a
// client that talks to the registry the way the rest of this package does, and
// both were found only by pointing Docker 29 at a running instance.

// TestTokenEndpointAcceptsOAuth2PostForm covers the token request shape
// containerd — and therefore Docker — uses when pushing.
//
// The endpoint has two forms. The original is a GET with the scope in the
// query string and the credential in a Basic header, which is what the harness
// in this package speaks and what every other test exercises. The OAuth2 form
// is a POST carrying grant_type, service, scope, username and password in a
// form-encoded body.
//
// Reading only the query string does not fail loudly. The request authenticates
// as nobody, is granted nothing, and still returns 200 with a well-formed
// token — so the client retries its push against a token that grants no access
// and loops on 401 until it gives up, never indicating that the token service
// was involved.
func TestTokenEndpointAcceptsOAuth2PostForm(t *testing.T) {
	h := newHarness(t)
	h.createOrg("acme")
	secret := h.deployToken("builder", "acme", "acme/", authz.RoleContributor)

	scope := "repository:acme/web:pull,push"

	form := url.Values{
		"grant_type": {"password"},
		"service":    {"registry.test"},
		"scope":      {scope},
		"client_id":  {"docker"},
		"username":   {"mantle"},
		"password":   {secret},
	}
	resp, err := http.Post(h.URL("/auth/token"),
		"application/x-www-form-urlencoded", strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatalf("posting to the token endpoint: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("token request: got %d, want 200", resp.StatusCode)
	}

	var body struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decoding the token response: %v", err)
	}
	if body.Token == "" {
		t.Fatal("the token response carried no token")
	}

	claims := decodeClaims(t, body.Token)

	// The precise failure being guarded against: a 200 carrying an anonymous
	// token with an empty access list.
	if strings.Contains(claims.Subject, "anonymous") {
		t.Errorf("the form-encoded credential was ignored: subject is %q", claims.Subject)
	}
	if len(claims.Access) == 0 {
		t.Fatal("the token granted no access, so a push against it would 401 forever")
	}

	got := claims.Access[0]
	if got.Name != "acme/web" {
		t.Errorf("access name: got %q, want acme/web", got.Name)
	}
	if !contains(got.Actions, "pull") || !contains(got.Actions, "push") {
		t.Errorf("actions: got %v, want both pull and push", got.Actions)
	}
}

// TestTokenEndpointRejectsBadFormCredentials makes sure the form path is a real
// authentication path rather than one that quietly falls through to anonymous.
func TestTokenEndpointRejectsBadFormCredentials(t *testing.T) {
	h := newHarness(t)
	h.createOrg("acme")

	form := url.Values{
		"grant_type": {"password"},
		"scope":      {"repository:acme/web:pull,push"},
		"username":   {"mantle"},
		"password":   {"mantle_dep_0000000000000000_not-a-real-secret"},
	}
	resp, err := http.Post(h.URL("/auth/token"),
		"application/x-www-form-urlencoded", strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatalf("posting to the token endpoint: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("a bad form credential returned %d, want 401", resp.StatusCode)
	}
}

// TestConcurrentFirstPushCreatesRepositoryOnce covers repository auto-creation
// racing against itself.
//
// Docker uploads a manifest's layers concurrently, so the first push to a new
// repository arrives at EnsureRepository several times at once, every one of
// them having seen "not found". Sequential tests never reach the second path
// because the first call has already created the row.
//
// The original failure was not a lost update but an outright error: the upsert
// named an arbiter that did not match the constraint Postgres raises first, so
// the loser of the race got a raw unique violation surfaced to the client as a
// 500 mid-push.
func TestConcurrentFirstPushCreatesRepositoryOnce(t *testing.T) {
	h := newHarness(t)
	h.createOrg("acme")
	secret := h.deployToken("builder", "acme", "acme/", authz.RoleContributor)

	const parallel = 8
	var wg sync.WaitGroup
	errs := make(chan error, parallel)

	for i := 0; i < parallel; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// Each goroutine pushes a distinct blob to the same not-yet-existing
			// repository, which is what Docker does with a multi-layer image.
			c := h.TokenClient(secret)
			resp := c.Post("/v2/acme/race/blobs/uploads/", nil, nil)
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusAccepted {
				errs <- fmt.Errorf("upload %d: starting an upload returned %d, want 202",
					i, resp.StatusCode)
			}
		}(i)
	}
	wg.Wait()
	close(errs)

	for err := range errs {
		t.Error(err)
	}

	var rows int
	if err := h.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM repositories WHERE name = 'acme/race'`).Scan(&rows); err != nil {
		t.Fatalf("counting repositories: %v", err)
	}
	if rows != 1 {
		t.Errorf("got %d rows for acme/race, want exactly 1", rows)
	}
}

// --- helpers --------------------------------------------------------------

type tokenClaims struct {
	Subject string `json:"sub"`
	Access  []struct {
		Type    string   `json:"type"`
		Name    string   `json:"name"`
		Actions []string `json:"actions"`
	} `json:"access"`
}

// decodeClaims reads a JWT payload without verifying it. The signature is not
// what these tests are about, and verifying it here would only re-test the
// issuer.
func decodeClaims(t *testing.T, token string) tokenClaims {
	t.Helper()
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("expected a three-part JWT, got %d parts", len(parts))
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("decoding the token payload: %v", err)
	}
	var claims tokenClaims
	if err := json.Unmarshal(raw, &claims); err != nil {
		t.Fatalf("parsing the token claims: %v", err)
	}
	return claims
}

func contains(haystack []string, needle string) bool {
	for _, v := range haystack {
		if v == needle {
			return true
		}
	}
	return false
}
