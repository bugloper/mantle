package main

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func newTestHandler(t *testing.T, upstream *httptest.Server) http.Handler {
	return newTestHandlerMode(t, upstream, false)
}

func newTestHandlerMode(t *testing.T, upstream *httptest.Server, readOnly bool) http.Handler {
	t.Helper()
	target, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	assets, err := resolveAssets("")
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return newHandler(target, assets, logger, readOnly)
}

// --read-only restores the posture §14.3 recommends: the interface becomes a
// dashboard and every change goes through the CLI. It is enforced by the proxy
// refusing to carry a mutating method, not by the interface declining to offer
// one — hiding a button is a convenience, this is the boundary.
func TestReadOnlyModeRefusesMutatingMethods(t *testing.T) {
	var reached bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	handler := newTestHandlerMode(t, upstream, true)

	for _, method := range []string{
		http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete,
	} {
		reached = false
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(method, "/api/v1/gc/run", nil))

		if recorder.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s returned %d, want 405", method, recorder.Code)
		}
		if reached {
			t.Errorf("%s reached the registry; the read-only guard did not hold", method)
		}
		if !strings.Contains(recorder.Body.String(), "read_only") {
			t.Errorf("%s: response does not explain why it was refused: %s", method, recorder.Body)
		}
	}

	// Reads must still work, or read-only mode would be no mode at all.
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/version", nil))
	if !reached {
		t.Error("a GET was blocked in read-only mode")
	}
}

// By default the interface can write, and the proxy must not second-guess which
// writes are allowed — that is the registry's decision, reached with the
// caller's own credential. A proxy with its own opinion would let the two
// surfaces disagree about permissions.
func TestWritesReachTheRegistryByDefault(t *testing.T) {
	var seen []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Method+" "+r.URL.Path)
		w.WriteHeader(http.StatusCreated)
	}))
	defer upstream.Close()

	handler := newTestHandlerMode(t, upstream, false)

	cases := map[string]string{
		http.MethodPost:   "/api/v1/repositories",
		http.MethodPatch:  "/api/v1/repositories/acme/web",
		http.MethodDelete: "/api/v1/tokens/6ba7b810-9dad-11d1-80b4-00c04fd430c8",
	}
	for method, path := range cases {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(method, path, nil))
		if recorder.Code == http.StatusMethodNotAllowed {
			t.Errorf("%s %s was refused by the proxy; only --read-only should do that", method, path)
		}
	}
	if len(seen) != len(cases) {
		t.Errorf("the registry saw %d of %d writes: %v", len(seen), len(cases), seen)
	}
}

// The interface asks what it may do rather than assuming, so it can omit
// controls that would only 405.
func TestUIConfigReportsCapabilities(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer upstream.Close()

	for _, readOnly := range []bool{false, true} {
		handler := newTestHandlerMode(t, upstream, readOnly)
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/ui-config.json", nil))

		if recorder.Code != http.StatusOK {
			t.Fatalf("read_only=%t: status %d, want 200", readOnly, recorder.Code)
		}
		var config struct {
			ReadOnly bool   `json:"read_only"`
			Version  string `json:"version"`
		}
		if err := json.Unmarshal(recorder.Body.Bytes(), &config); err != nil {
			t.Fatalf("read_only=%t: body is not JSON: %s", readOnly, recorder.Body)
		}
		if config.ReadOnly != readOnly {
			t.Errorf("read_only reported as %t, want %t", config.ReadOnly, readOnly)
		}
		if config.Version == "" {
			t.Error("no version reported")
		}
	}
}

// The proxy holds no credential of its own: it forwards what the browser sent,
// unchanged. If it ever added one, this process would become a privileged
// component, which §14.3 forbids.
func TestProxyForwardsTheBrowsersCredentialAndNothingElse(t *testing.T) {
	var gotAuth, gotCookie string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotCookie = r.Header.Get("Cookie")
		w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	handler := newTestHandler(t, upstream)

	request := httptest.NewRequest(http.MethodGet, "/api/v1/version", nil)
	request.Header.Set("Authorization", "Basic dXNlcjpwYXNz")
	request.Header.Set("Cookie", "session=should-not-travel")
	handler.ServeHTTP(httptest.NewRecorder(), request)

	if gotAuth != "Basic dXNlcjpwYXNz" {
		t.Errorf("Authorization forwarded as %q, want it passed through unchanged", gotAuth)
	}
	if gotCookie != "" {
		t.Errorf("a cookie reached the registry (%q); the proxy should strip it", gotCookie)
	}

	// With no credential, nothing is invented on the caller's behalf.
	gotAuth = "sentinel"
	handler.ServeHTTP(httptest.NewRecorder(),
		httptest.NewRequest(http.MethodGet, "/api/v1/version", nil))
	if gotAuth != "" {
		t.Errorf("the proxy supplied its own Authorization header (%q)", gotAuth)
	}
}

// A registry that is down must produce an explanation, not a blank page.
func TestProxyReportsAnUnreachableRegistry(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	upstream.Close() // Closed on purpose: nothing is listening.

	handler := newTestHandler(t, upstream)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/version", nil))

	if recorder.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", recorder.Code)
	}
	body := recorder.Body.String()
	if !strings.Contains(body, "registry_unreachable") {
		t.Errorf("body does not name the failure: %s", body)
	}
	if !strings.Contains(body, "remedy") {
		t.Errorf("body offers no next action: %s", body)
	}
}

// The root must serve the page, not redirect. http.FileServer canonicalises
// /index.html to / with a 301, which loops; this pins the fix.
func TestRootServesTheInterface(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer upstream.Close()
	handler := newTestHandler(t, upstream)

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("GET / returned %d, want 200", recorder.Code)
	}
	if !strings.Contains(recorder.Body.String(), "<title>Mantle</title>") {
		t.Error("GET / did not serve the interface")
	}
	if contentType := recorder.Header().Get("Content-Type"); !strings.HasPrefix(contentType, "text/html") {
		t.Errorf("Content-Type = %q, want text/html", contentType)
	}
}

func TestAssetsAreServedAndUnknownPathsAre404(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer upstream.Close()
	handler := newTestHandler(t, upstream)

	for path, wantType := range map[string]string{
		"/app.js":    "text/javascript",
		"/style.css": "text/css",
	} {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		if recorder.Code != http.StatusOK {
			t.Errorf("GET %s returned %d, want 200", path, recorder.Code)
		}
		if got := recorder.Header().Get("Content-Type"); !strings.HasPrefix(got, wantType) {
			t.Errorf("GET %s Content-Type = %q, want %s", path, got, wantType)
		}
	}

	// A missing asset must 404 rather than fall back to index.html, or a
	// mistyped script path returns HTML that then fails to parse as JavaScript.
	//
	// "/assets/app.js" is included because the embedded tree is rooted at
	// assets/, so the directory name must not also be reachable as a path
	// component — otherwise every asset has two URLs.
	for _, path := range []string{"/missing.js", "/assets/app.js", "/style.css/"} {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		if recorder.Code != http.StatusNotFound {
			t.Errorf("GET %s returned %d, want 404", path, recorder.Code)
		}
	}
}

// Traversal must not reach anything outside the embedded asset tree. ServeMux
// cleans the path before the handler sees it, so the request is redirected to
// its cleaned form rather than refused — what matters is that no file outside
// the tree is ever served, by either route.
func TestTraversalReachesNothing(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer upstream.Close()
	handler := newTestHandler(t, upstream)

	for _, path := range []string{
		"/../main.go", "/../../go.mod", "/..%2fmain.go", "/./../../LICENSE",
	} {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))

		if recorder.Code == http.StatusOK {
			t.Errorf("GET %s returned 200; something outside the asset tree was served", path)
		}
		body := recorder.Body.String()
		for _, leak := range []string{"package main", "module github.com", "Apache License"} {
			if strings.Contains(body, leak) {
				t.Errorf("GET %s leaked file content containing %q", path, leak)
			}
		}
	}
}

// The policy is strict because everything is same-origin. If a future change
// needs a CDN or an inline handler, this test should be the thing that objects.
func TestSecurityHeaders(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer upstream.Close()
	handler := newTestHandler(t, upstream)

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/", nil))

	policy := recorder.Header().Get("Content-Security-Policy")
	for _, directive := range []string{
		"default-src 'self'", "script-src 'self'", "connect-src 'self'",
		"frame-ancestors 'none'",
	} {
		if !strings.Contains(policy, directive) {
			t.Errorf("Content-Security-Policy lacks %q: %s", directive, policy)
		}
	}
	if strings.Contains(policy, "unsafe-inline") || strings.Contains(policy, "unsafe-eval") {
		t.Errorf("the policy permits unsafe script execution: %s", policy)
	}
	if recorder.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Error("X-Content-Type-Options is not nosniff")
	}
	if recorder.Header().Get("X-Frame-Options") != "DENY" {
		t.Error("X-Frame-Options is not DENY")
	}
}

func TestNormaliseURL(t *testing.T) {
	for input, want := range map[string]string{
		"registry.example.com":          "https://registry.example.com",
		"https://registry.example.com":  "https://registry.example.com",
		"http://127.0.0.1:5100":         "http://127.0.0.1:5100",
		"https://registry.example.com/": "https://registry.example.com",
	} {
		if got := normaliseURL(input); got != want {
			t.Errorf("normaliseURL(%q) = %q, want %q", input, got, want)
		}
	}
}
