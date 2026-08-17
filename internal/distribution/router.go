package distribution

import (
	"net/http"
	"regexp"
	"strings"

	ocierrors "github.com/mantle-sh/mantle/internal/distribution/errors"
	"github.com/mantle-sh/mantle/internal/oci"
)

// route binds a URL shape to a handler.
//
// The /v2 namespace cannot be served by a path-segment router, because a
// repository name contains slashes and has no fixed segment count:
// /v2/acme/web/team/api/manifests/v1 is one repository and one reference. Every
// conformant registry resolves this with regular expressions anchored on the
// distinctive suffix, and so does Mantle.
type route struct {
	// name identifies the endpoint class in metrics and logs. It is a fixed
	// string, never a path, so metric cardinality stays bounded.
	name    string
	pattern *regexp.Regexp
	methods map[string]handlerFunc
}

// handlerFunc handles a matched request. params carries the captured groups.
type handlerFunc func(w http.ResponseWriter, r *http.Request, p params)

// params are the captured path components.
type params struct {
	Name      string
	Reference string
	Digest    string
	Session   string
}

// The name group is deliberately permissive. Matching the specification grammar
// in the router would turn an invalid name into a 404 from the router's
// perspective, when the specification requires NAME_INVALID with an explanation
// (REQ-OCI-06). So the router captures loosely and validation happens in one
// place, where it can produce a useful message.
const namePattern = `(.+?)`

// Digests and session ids are matched loosely for the same reason: a malformed
// digest should be DIGEST_INVALID, not "no such route".
const (
	digestPattern    = `([^/]+)`
	referencePattern = `([^/]+)`
	sessionPattern   = `([^/]+)`
)

func (s *Service) buildRoutes() []route {
	return []route{
		{
			// end-1
			name:    "base",
			pattern: regexp.MustCompile(`^/v2/?$`),
			methods: map[string]handlerFunc{
				http.MethodGet:  s.handleBase,
				http.MethodHead: s.handleBase,
			},
		},
		{
			// Not in the specification, but universally expected by tooling.
			name:    "catalog",
			pattern: regexp.MustCompile(`^/v2/_catalog/?$`),
			methods: map[string]handlerFunc{
				http.MethodGet: s.handleCatalog,
			},
		},
		{
			// end-13, end-5, end-6, and upload cancellation. Registered before
			// the blob route so that "uploads" is never mistaken for a digest.
			name:    "upload_session",
			pattern: regexp.MustCompile(`^/v2/` + namePattern + `/blobs/uploads/` + sessionPattern + `/?$`),
			methods: map[string]handlerFunc{
				http.MethodGet:    s.handleUploadStatus,
				http.MethodPatch:  s.handleUploadChunk,
				http.MethodPut:    s.handleUploadComplete,
				http.MethodDelete: s.handleUploadCancel,
			},
		},
		{
			// end-4a, end-4b, end-11
			name:    "upload_start",
			pattern: regexp.MustCompile(`^/v2/` + namePattern + `/blobs/uploads/?$`),
			methods: map[string]handlerFunc{
				http.MethodPost: s.handleUploadStart,
			},
		},
		{
			// end-2, end-10
			name:    "blob",
			pattern: regexp.MustCompile(`^/v2/` + namePattern + `/blobs/` + digestPattern + `$`),
			methods: map[string]handlerFunc{
				http.MethodGet:    s.handleBlobGet,
				http.MethodHead:   s.handleBlobGet,
				http.MethodDelete: s.handleBlobDelete,
			},
		},
		{
			// end-3, end-7, end-9
			name:    "manifest",
			pattern: regexp.MustCompile(`^/v2/` + namePattern + `/manifests/` + referencePattern + `$`),
			methods: map[string]handlerFunc{
				http.MethodGet:    s.handleManifestGet,
				http.MethodHead:   s.handleManifestGet,
				http.MethodPut:    s.handleManifestPut,
				http.MethodDelete: s.handleManifestDelete,
			},
		},
		{
			// end-8a, end-8b
			name:    "tags_list",
			pattern: regexp.MustCompile(`^/v2/` + namePattern + `/tags/list/?$`),
			methods: map[string]handlerFunc{
				http.MethodGet: s.handleTagsList,
			},
		},
		{
			// end-12a, end-12b
			name:    "referrers",
			pattern: regexp.MustCompile(`^/v2/` + namePattern + `/referrers/` + digestPattern + `$`),
			methods: map[string]handlerFunc{
				http.MethodGet: s.handleReferrers,
			},
		},
	}
}

// dispatch resolves a request to a handler.
func (s *Service) dispatch(w http.ResponseWriter, r *http.Request) {
	path := r.URL.EscapedPath()

	for _, rt := range s.routes {
		matches := rt.pattern.FindStringSubmatch(path)
		if matches == nil {
			continue
		}

		handler, ok := rt.methods[r.Method]
		if !ok {
			// A known resource with an unsupported method. Advertising Allow
			// is what lets a client tell "not implemented here" apart from
			// "wrong URL", and it is required for a correct 405.
			w.Header().Set("Allow", allowHeader(rt.methods))
			ocierrors.ServeJSON(w, ocierrors.Unsupported(
				r.Method+" is not supported on this resource"), nil)
			return
		}

		p, err := decodeParams(rt, matches)
		if err != nil {
			ocierrors.ServeJSON(w, err, nil)
			return
		}
		setEndpointClass(r, rt.name)
		handler(w, r, p)
		return
	}

	// Nothing matched. Everything under /v2 that is not a known route is a
	// 404 in the OCI envelope rather than Go's plain-text default, because a
	// client parsing an error body must not receive HTML or bare text.
	setEndpointClass(r, "unknown")
	ocierrors.ServeJSON(w, ocierrors.New(ocierrors.CodeUnsupported,
		"the requested path is not part of the registry API"), nil)
}

// decodeParams extracts and URL-decodes the captured groups.
func decodeParams(rt route, matches []string) (params, *ocierrors.Errors) {
	var p params
	switch rt.name {
	case "base", "catalog":
		return p, nil
	case "upload_session":
		p.Name, p.Session = matches[1], matches[2]
	case "upload_start":
		p.Name = matches[1]
	case "blob", "referrers":
		p.Name, p.Digest = matches[1], matches[2]
	case "manifest":
		p.Name, p.Reference = matches[1], matches[2]
	case "tags_list":
		p.Name = matches[1]
	}

	// Percent-decode the name. Docker does not escape repository names, but
	// other clients and proxies do, and a name that round-trips differently
	// would resolve to a different repository than the one pushed to.
	if p.Name != "" {
		decoded, err := decodePathComponent(p.Name)
		if err != nil {
			return p, ocierrors.NameInvalid(p.Name, "name is not valid percent-encoding")
		}
		p.Name = decoded
	}
	if p.Reference != "" {
		decoded, err := decodePathComponent(p.Reference)
		if err != nil {
			return p, ocierrors.New(ocierrors.CodeManifestUnknown, "reference is not valid percent-encoding")
		}
		p.Reference = decoded
	}
	if p.Digest != "" {
		decoded, err := decodePathComponent(p.Digest)
		if err != nil {
			return p, ocierrors.DigestInvalid("digest is not valid percent-encoding")
		}
		p.Digest = decoded
	}
	return p, nil
}

// decodePathComponent percent-decodes a single path component, rejecting an
// encoded slash. An encoded separator would let a crafted name change how the
// path is interpreted after the router has already made its decision.
func decodePathComponent(s string) (string, error) {
	if !strings.Contains(s, "%") {
		return s, nil
	}
	decoded, err := urlPathUnescape(s)
	if err != nil {
		return "", err
	}
	return decoded, nil
}

func allowHeader(methods map[string]handlerFunc) string {
	// A fixed order so the header is stable across requests and testable.
	ordered := []string{
		http.MethodGet, http.MethodHead, http.MethodPost,
		http.MethodPatch, http.MethodPut, http.MethodDelete,
	}
	var allowed []string
	for _, m := range ordered {
		if _, ok := methods[m]; ok {
			allowed = append(allowed, m)
		}
	}
	return strings.Join(allowed, ", ")
}

// validateName checks a captured repository name and returns the OCI error to
// serve if it is invalid (REQ-OCI-06).
func validateName(name string) *ocierrors.Errors {
	if err := oci.ValidateName(name); err != nil {
		return ocierrors.NameInvalid(name, err.Error())
	}
	return nil
}
