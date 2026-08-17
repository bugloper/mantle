// Package integration exercises Mantle end to end over real HTTP, against a
// real PostgreSQL database and a real filesystem blob store.
//
// These tests drive the registry the way a client does — token dance included —
// rather than calling handlers directly. That is deliberate: most of the
// interoperability bugs a registry ships are in headers, status codes, and the
// order of requests, none of which a handler-level test observes.
package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mantle-sh/mantle/internal/auth/authz"
	"github.com/mantle-sh/mantle/internal/auth/identity"
	"github.com/mantle-sh/mantle/internal/config"
	"github.com/mantle-sh/mantle/internal/oci"
	"github.com/mantle-sh/mantle/internal/server"
	"github.com/mantle-sh/mantle/internal/testsupport"
)

// harness is a running registry and a client that speaks to it.
type harness struct {
	t          *testing.T
	server     *server.Server
	httpServer *httptest.Server
	pool       *pgxpool.Pool
	identities *identity.Store

	adminUser     string
	adminPassword string
}

// newHarness starts a registry backed by a throwaway database and temp storage.
func newHarness(t *testing.T, mutate ...func(*config.Config)) *harness {
	t.Helper()
	ctx := context.Background()

	pool := testsupport.NewDB(t)

	// The server needs a URL for its own token realm, but the URL is not known
	// until httptest picks a port. The listener is created first and the realm
	// is patched in before any request is served.
	root := t.TempDir()

	cfg := config.Default()
	cfg.Server.Domain = "registry.test"
	cfg.Server.TLS.Mode = "off"
	cfg.Database.URL = poolURL(t, pool)
	cfg.Storage.Filesystem.Root = root
	cfg.Auth.SigningKeyPath = root + "/token.pem"
	cfg.Auth.Service = "registry.test"
	cfg.Auth.Realm = "http://registry.test/auth/token"
	cfg.Observability.MetricsListen = ""
	cfg.Observability.LogLevel = "error"
	// A one-hour grace period is the enforced minimum; tests that need
	// collection to act immediately override the collector directly.
	cfg.GC.Enabled = false
	// Flush ledger events promptly so assertions do not wait two seconds.
	cfg.Ledger.FlushInterval = config.Duration(50 * time.Millisecond)
	cfg.Ledger.FlushSize = 1

	for _, fn := range mutate {
		fn(&cfg)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("test configuration is invalid: %v", err)
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv, err := server.New(ctx, cfg, logger)
	if err != nil {
		t.Fatalf("starting the registry: %v", err)
	}
	t.Cleanup(srv.Close)

	httpServer := httptest.NewServer(srv.Handler())
	t.Cleanup(httpServer.Close)
	srv.Health.SetReady(true)

	// Run the ledger recorder so pull events and provenance are written.
	if srv.Recorder != nil {
		recorderCtx, cancel := context.WithCancel(ctx)
		go srv.Recorder.Run(recorderCtx)
		t.Cleanup(func() {
			srv.Recorder.Stop()
			cancel()
		})
	}

	bootstrap, err := server.Bootstrap(ctx, pool, "admin", "admin-password", server.DefaultOrganization)
	if err != nil {
		t.Fatalf("bootstrapping the instance: %v", err)
	}
	_ = bootstrap

	h := &harness{
		t:             t,
		server:        srv,
		httpServer:    httpServer,
		pool:          pool,
		identities:    identity.New(pool),
		adminUser:     "admin",
		adminPassword: "admin-password",
	}
	h.createOrg("acme")
	return h
}

// poolURL reconstructs a connection string for the test database.
func poolURL(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	cfg := pool.Config().ConnConfig
	return fmt.Sprintf("postgres://%s@%s:%d/%s?sslmode=disable",
		cfg.User, cfg.Host, cfg.Port, cfg.Database)
}

func (h *harness) URL(path string) string { return h.httpServer.URL + path }

func (h *harness) createOrg(slug string) {
	h.t.Helper()
	if _, err := h.pool.Exec(context.Background(),
		`INSERT INTO organizations (slug, display_name) VALUES ($1, $2) ON CONFLICT DO NOTHING`,
		slug, slug); err != nil {
		h.t.Fatalf("creating organization %q: %v", slug, err)
	}
}

// orgID resolves an organization's id.
func (h *harness) orgID(slug string) int64 {
	h.t.Helper()
	var id int64
	if err := h.pool.QueryRow(context.Background(),
		`SELECT id FROM organizations WHERE slug = $1`, slug).Scan(&id); err != nil {
		h.t.Fatalf("looking up organization %q: %v", slug, err)
	}
	return id
}

// deployToken creates a machine credential with a role over a namespace.
func (h *harness) deployToken(name, orgSlug, namespace string, role authz.Role) string {
	h.t.Helper()
	ctx := context.Background()
	orgID := h.orgID(orgSlug)

	id, secret, err := h.identities.CreateMachine(ctx, identity.CreateMachineParams{
		Kind:           identity.KindDeployToken,
		Name:           name,
		OrganizationID: orgID,
	})
	if err != nil {
		h.t.Fatalf("creating deploy token %q: %v", name, err)
	}
	if err := h.identities.Grant(ctx, identity.GrantParams{
		IdentityID:      &id.ID,
		ScopeType:       "namespace",
		NamespacePrefix: namespace,
		Role:            role,
		Effect:          "allow",
	}); err != nil {
		h.t.Fatalf("granting %s on %s: %v", role, namespace, err)
	}
	return secret
}

// client is an OCI client that performs the token dance the way Docker does.
type client struct {
	t       *testing.T
	harness *harness
	// username and secret are the Basic credentials presented to the token
	// endpoint. An empty pair means anonymous.
	username string
	secret   string
	// tokens caches issued tokens per scope, as a real client does.
	tokens map[string]string
	http   *http.Client
}

// Client returns a client authenticating with the given credentials.
func (h *harness) Client(username, secret string) *client {
	return &client{
		t: h.t, harness: h, username: username, secret: secret,
		tokens: map[string]string{},
		http: &http.Client{
			Timeout: 30 * time.Second,
			// Redirects are not followed automatically: several tests assert on
			// the 307 itself.
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

// Anonymous returns an unauthenticated client.
func (h *harness) Anonymous() *client { return h.Client("", "") }

// TokenClient authenticates with a machine credential, which is presented as
// the password with an arbitrary username, exactly as Docker does.
func (h *harness) TokenClient(secret string) *client { return h.Client("mantle", secret) }

// do performs a request, transparently handling a 401 by fetching a token from
// the realm named in the challenge and retrying once. This is the Docker
// registry v2 token flow (§9.1), and implementing it here is what makes these
// tests exercise the real client contract.
func (c *client) do(method, path string, body io.Reader, headers map[string]string) *http.Response {
	c.t.Helper()

	var bodyBytes []byte
	if body != nil {
		var err error
		bodyBytes, err = io.ReadAll(body)
		if err != nil {
			c.t.Fatalf("reading request body: %v", err)
		}
	}

	send := func(bearer string) *http.Response {
		var reader io.Reader
		if bodyBytes != nil {
			reader = bytes.NewReader(bodyBytes)
		}
		req, err := http.NewRequest(method, c.harness.URL(path), reader)
		if err != nil {
			c.t.Fatalf("building %s %s: %v", method, path, err)
		}
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		if bodyBytes != nil {
			req.ContentLength = int64(len(bodyBytes))
		}
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		resp, err := c.http.Do(req)
		if err != nil {
			c.t.Fatalf("%s %s: %v", method, path, err)
		}
		return resp
	}

	// The admin API authenticates with HTTP Basic rather than the registry
	// token dance, which is what the CLI does too. It has no realm to redirect
	// to, so credentials go on the first request.
	if strings.HasPrefix(path, "/api/v1/") {
		var reader io.Reader
		if bodyBytes != nil {
			reader = bytes.NewReader(bodyBytes)
		}
		req, err := http.NewRequest(method, c.harness.URL(path), reader)
		if err != nil {
			c.t.Fatalf("building %s %s: %v", method, path, err)
		}
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		if c.username != "" || c.secret != "" {
			req.SetBasicAuth(c.username, c.secret)
		}
		resp, err := c.http.Do(req)
		if err != nil {
			c.t.Fatalf("%s %s: %v", method, path, err)
		}
		return resp
	}

	// Reuse a cached token for this path's scope if we have one.
	scope := scopeForPath(method, path)
	resp := send(c.tokens[scope])
	if resp.StatusCode != http.StatusUnauthorized {
		return resp
	}

	challenge := resp.Header.Get("WWW-Authenticate")
	resp.Body.Close()
	if challenge == "" {
		// A 401 with no challenge is itself a conformance failure; return it so
		// the test can assert on it rather than hiding it behind a retry.
		return send("")
	}

	bearer, ok := c.fetchToken(challenge)
	if !ok {
		return send("")
	}
	c.tokens[scope] = bearer
	return send(bearer)
}

// fetchToken performs step 3 of the token flow against the realm in the
// challenge.
func (c *client) fetchToken(challenge string) (string, bool) {
	c.t.Helper()
	params := parseChallenge(challenge)
	realm := params["realm"]
	if realm == "" {
		return "", false
	}

	// The configured realm points at the production hostname; in tests the
	// server is on an ephemeral port, so only the path is used.
	realmURL, err := url.Parse(realm)
	if err != nil {
		return "", false
	}
	target := c.harness.URL(realmURL.Path)

	query := url.Values{}
	if service := params["service"]; service != "" {
		query.Set("service", service)
	}
	if scope := params["scope"]; scope != "" {
		query.Set("scope", scope)
	}

	req, err := http.NewRequest(http.MethodGet, target+"?"+query.Encode(), nil)
	if err != nil {
		return "", false
	}
	if c.username != "" || c.secret != "" {
		req.SetBasicAuth(c.username, c.secret)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		c.t.Fatalf("fetching a token from %s: %v", target, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// Not fatal: several tests legitimately expect the token fetch to fail
		// (an anonymous caller, a disabled credential) and then assert on the
		// 401 that follows. It is logged, because when it is *not* expected the
		// failure otherwise surfaces as a confusing 401 several steps later.
		c.t.Logf("token endpoint returned %d for scope %q: %s",
			resp.StatusCode, params["scope"], readBody(resp))
		return "", false
	}

	var payload struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return "", false
	}
	if payload.Token != "" {
		return payload.Token, true
	}
	return payload.AccessToken, payload.AccessToken != ""
}

// parseChallenge splits a WWW-Authenticate Bearer challenge into its parameters.
func parseChallenge(challenge string) map[string]string {
	params := map[string]string{}
	rest := strings.TrimSpace(strings.TrimPrefix(challenge, "Bearer "))
	for _, part := range splitChallengeParts(rest) {
		key, value, found := strings.Cut(part, "=")
		if !found {
			continue
		}
		params[strings.TrimSpace(key)] = strings.Trim(strings.TrimSpace(value), `"`)
	}
	return params
}

// splitChallengeParts splits on commas that are not inside a quoted string,
// because a scope value legitimately contains commas ("pull,push").
func splitChallengeParts(s string) []string {
	var parts []string
	var current strings.Builder
	inQuotes := false
	for i := 0; i < len(s); i++ {
		switch {
		case s[i] == '"':
			inQuotes = !inQuotes
			current.WriteByte(s[i])
		case s[i] == ',' && !inQuotes:
			parts = append(parts, current.String())
			current.Reset()
		default:
			current.WriteByte(s[i])
		}
	}
	if current.Len() > 0 {
		parts = append(parts, current.String())
	}
	return parts
}

// scopeForPath derives a cache key for the token belonging to a request.
func scopeForPath(method, path string) string {
	return method + " " + path
}

// --- request helpers ---

func (c *client) Get(path string) *http.Response  { return c.do(http.MethodGet, path, nil, nil) }
func (c *client) Head(path string) *http.Response { return c.do(http.MethodHead, path, nil, nil) }
func (c *client) Delete(path string) *http.Response {
	return c.do(http.MethodDelete, path, nil, nil)
}

func (c *client) Post(path string, body io.Reader, headers map[string]string) *http.Response {
	return c.do(http.MethodPost, path, body, headers)
}

func (c *client) Patch(path string, body io.Reader, headers map[string]string) *http.Response {
	return c.do(http.MethodPatch, path, body, headers)
}

func (c *client) Put(path string, body io.Reader, headers map[string]string) *http.Response {
	return c.do(http.MethodPut, path, body, headers)
}

// --- higher-level operations ---

// PushBlob uploads a blob using the chunked flow and returns its digest.
func (c *client) PushBlob(repo string, content []byte) string {
	c.t.Helper()
	digest := oci.FromBytes(content).String()

	resp := c.Post(fmt.Sprintf("/v2/%s/blobs/uploads/", repo), nil, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		c.t.Fatalf("starting an upload to %s: status %d, body %s", repo, resp.StatusCode, readBody(resp))
	}
	location := resp.Header.Get("Location")
	if location == "" {
		c.t.Fatal("upload start returned no Location header")
	}

	putURL := appendQuery(location, "digest", digest)
	putResp := c.Put(putURL, bytes.NewReader(content),
		map[string]string{"Content-Type": "application/octet-stream"})
	defer putResp.Body.Close()
	if putResp.StatusCode != http.StatusCreated {
		c.t.Fatalf("completing an upload to %s: status %d, body %s",
			repo, putResp.StatusCode, readBody(putResp))
	}
	return digest
}

// PushManifest stores a manifest under a reference and returns its digest.
func (c *client) PushManifest(repo, reference string, payload []byte, mediaType string) string {
	c.t.Helper()
	resp := c.Put(fmt.Sprintf("/v2/%s/manifests/%s", repo, reference),
		bytes.NewReader(payload), map[string]string{"Content-Type": mediaType})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		c.t.Fatalf("pushing manifest %s:%s: status %d, body %s",
			repo, reference, resp.StatusCode, readBody(resp))
	}
	return resp.Header.Get("Docker-Content-Digest")
}

// --- fixtures ---

// testImage is a minimal but structurally valid image.
type testImage struct {
	ConfigBlob     []byte
	ConfigDigest   string
	Layers         [][]byte
	LayerDigests   []string
	Manifest       []byte
	ManifestDigest string
}

// buildImage constructs an image whose config carries the given labels, so
// Tier 0 provenance extraction has something real to read.
func buildImage(t *testing.T, layerContents []string, labels map[string]string,
	annotations map[string]string) *testImage {
	t.Helper()

	config := map[string]any{
		"architecture": "amd64",
		"os":           "linux",
		"created":      "2026-08-14T10:00:00Z",
		"config":       map[string]any{"Labels": labels},
		"rootfs":       map[string]any{"type": "layers", "diff_ids": []string{}},
	}
	configBlob, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}

	image := &testImage{
		ConfigBlob:   configBlob,
		ConfigDigest: oci.FromBytes(configBlob).String(),
	}

	layerDescriptors := make([]map[string]any, 0, len(layerContents))
	for _, content := range layerContents {
		blob := []byte(content)
		digest := oci.FromBytes(blob).String()
		image.Layers = append(image.Layers, blob)
		image.LayerDigests = append(image.LayerDigests, digest)
		layerDescriptors = append(layerDescriptors, map[string]any{
			"mediaType": "application/vnd.oci.image.layer.v1.tar+gzip",
			"digest":    digest,
			"size":      len(blob),
		})
	}

	manifest := map[string]any{
		"schemaVersion": 2,
		"mediaType":     oci.MediaTypeOCIManifest,
		"config": map[string]any{
			"mediaType": oci.MediaTypeOCIImageConfig,
			"digest":    image.ConfigDigest,
			"size":      len(configBlob),
		},
		"layers": layerDescriptors,
	}
	if len(annotations) > 0 {
		manifest["annotations"] = annotations
	}

	image.Manifest, err = json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	image.ManifestDigest = oci.FromBytes(image.Manifest).String()
	return image
}

// PushImage uploads an image's blobs and manifest.
func (c *client) PushImage(repo, reference string, image *testImage) {
	c.t.Helper()
	c.PushBlob(repo, image.ConfigBlob)
	for _, layer := range image.Layers {
		c.PushBlob(repo, layer)
	}
	c.PushManifest(repo, reference, image.Manifest, oci.MediaTypeOCIManifest)
}

// --- small helpers ---

func readBody(resp *http.Response) string {
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8192))
	if err != nil {
		return "<unreadable>"
	}
	return string(body)
}

func appendQuery(rawURL, key, value string) string {
	separator := "?"
	if strings.Contains(rawURL, "?") {
		separator = "&"
	}
	return rawURL + separator + key + "=" + url.QueryEscape(value)
}

// decodeOCIError parses an error envelope from a response body.
func decodeOCIError(t *testing.T, resp *http.Response) []struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Detail  any    `json:"detail"`
} {
	t.Helper()
	var envelope struct {
		Errors []struct {
			Code    string `json:"code"`
			Message string `json:"message"`
			Detail  any    `json:"detail"`
		} `json:"errors"`
	}
	body, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("response body is not an OCI error envelope: %s", body)
	}
	return envelope.Errors
}

// requireStatus fails the test unless the response carries the expected status.
func requireStatus(t *testing.T, resp *http.Response, want int, context string) {
	t.Helper()
	if resp.StatusCode != want {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		t.Fatalf("%s: status %d, want %d (body: %s)", context, resp.StatusCode, want, body)
	}
}
