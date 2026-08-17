package distribution

import (
	"net/http"
	"testing"
)

// The router and the challenge builder both decide which endpoint a path
// belongs to. The router does it by matching routes; the challenge builder has
// to do it before the router has run, because a 401 is produced by the
// authentication middleware above it.
//
// Two implementations of one decision will drift, so this test pins them
// together. When they disagree, a client is handed a token whose scope does not
// cover the request it is about to retry — which surfaces as a confusing 404,
// as it did twice during development.
func TestChallengeClassMatchesRouterClass(t *testing.T) {
	service := &Service{}
	service.routes = service.buildRoutes()

	paths := []string{
		"/v2/acme/web/manifests/v1.2.3",
		"/v2/acme/web/manifests/sha256:" + hex64,
		"/v2/acme/web/blobs/sha256:" + hex64,
		"/v2/acme/web/blobs/uploads/",
		"/v2/acme/web/blobs/uploads/6ba7b810-9dad-11d1-80b4-00c04fd430c8",
		"/v2/acme/web/tags/list",
		"/v2/acme/web/referrers/sha256:" + hex64,
		"/v2/acme/team/nested/web/manifests/v1",
	}

	for _, path := range paths {
		routerClass := ""
		for _, route := range service.routes {
			if route.pattern.MatchString(path) {
				routerClass = route.name
				break
			}
		}
		if routerClass == "" {
			t.Errorf("%s matched no route", path)
			continue
		}

		challengeClass := endpointClassForPath(path)
		if challengeClass != routerClass {
			t.Errorf("%s: the router calls this %q but the challenge builder calls it %q",
				path, routerClass, challengeClass)
		}
	}
}

// Every method the router accepts on a route must map to a permission. A route
// added without one would be authorised as a read by default, which is the
// wrong way for that mistake to fail.
func TestEveryRouteMethodHasARequiredAction(t *testing.T) {
	service := &Service{}
	service.routes = service.buildRoutes()

	for _, route := range service.routes {
		if route.name == "base" || route.name == "catalog" {
			continue // Handled directly, with their own anonymous-access rules.
		}
		for method := range route.methods {
			action := requiredAction(method, route.name)
			if action == "" {
				t.Errorf("%s %s maps to no permission", method, route.name)
			}
			// A write must never resolve to a read permission.
			switch method {
			case http.MethodPost, http.MethodPatch, http.MethodPut:
				if action == "pull" {
					t.Errorf("%s on %s requires only pull permission", method, route.name)
				}
			}
		}
	}
}

// Repository names contain slashes and have no fixed segment count, so the
// router has to resolve them by suffix. These are the shapes that break naive
// splitting.
func TestRouterResolvesNamesWithSlashes(t *testing.T) {
	service := &Service{}
	service.routes = service.buildRoutes()

	cases := []struct {
		path      string
		wantRoute string
		wantName  string
		wantExtra string
	}{
		{"/v2/acme/web/manifests/v1", "manifest", "acme/web", "v1"},
		{"/v2/a/b/c/d/manifests/latest", "manifest", "a/b/c/d", "latest"},
		{"/v2/acme/web/blobs/sha256:" + hex64, "blob", "acme/web", "sha256:" + hex64},
		{"/v2/acme/web/tags/list", "tags_list", "acme/web", ""},
		{"/v2/acme/web/blobs/uploads/", "upload_start", "acme/web", ""},
		{"/v2/acme/web/blobs/uploads/session-id", "upload_session", "acme/web", "session-id"},
		// A repository whose own name contains "blobs" must still resolve.
		{"/v2/acme/blobs/manifests/v1", "manifest", "acme/blobs", "v1"},
	}

	for _, tc := range cases {
		var matched *route
		var groups []string
		for i := range service.routes {
			if g := service.routes[i].pattern.FindStringSubmatch(tc.path); g != nil {
				matched = &service.routes[i]
				groups = g
				break
			}
		}
		if matched == nil {
			t.Errorf("%s matched no route", tc.path)
			continue
		}
		if matched.name != tc.wantRoute {
			t.Errorf("%s matched route %q, want %q", tc.path, matched.name, tc.wantRoute)
			continue
		}

		p, errs := decodeParams(*matched, groups)
		if errs != nil {
			t.Errorf("%s: decoding params failed: %v", tc.path, errs)
			continue
		}
		if p.Name != tc.wantName {
			t.Errorf("%s: name = %q, want %q", tc.path, p.Name, tc.wantName)
		}
		extra := p.Reference + p.Digest + p.Session
		if tc.wantExtra != "" && extra != tc.wantExtra {
			t.Errorf("%s: captured %q, want %q", tc.path, extra, tc.wantExtra)
		}
	}
}

const hex64 = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
