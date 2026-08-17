package distribution

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/mantle-sh/mantle/internal/auth/authz"
	"github.com/mantle-sh/mantle/internal/catalog"
	ocierrors "github.com/mantle-sh/mantle/internal/distribution/errors"
	"github.com/mantle-sh/mantle/internal/observability"
)

// Authenticate resolves the credential on a request, if any.
//
// This runs as middleware ahead of the router so that every /v2 request carries
// an auth context, but it makes no authorization decision: an unauthenticated
// request is perfectly valid until a handler asks for a permission it does not
// have. Separating the two is what lets an anonymous pull of a public image
// succeed while an anonymous pull of a private one produces a challenge.
func (s *Service) Authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := &authContext{Anonymous: true}

		header := r.Header.Get("Authorization")
		switch {
		case header == "":
			// Anonymous.

		case strings.HasPrefix(header, "Bearer "):
			raw := strings.TrimSpace(strings.TrimPrefix(header, "Bearer "))
			claims, err := s.opts.Verifier.Verify(raw)
			if err != nil {
				s.countAuthFailure("invalid_token")
				s.logger(r.Context()).Debug("rejected bearer token", "error", err)
				// An expired or malformed token gets the same challenge as no
				// token at all, so a client whose token aged out simply
				// re-authenticates rather than failing the deploy.
				s.challenge(w, r, ocierrors.New(ocierrors.CodeUnauthorized,
					"authentication required"))
				return
			}
			auth.Claims = claims

			// Resolve the identity behind the token. The claims are
			// authoritative for scope, but a disabled identity must stop
			// working immediately rather than at token expiry.
			//
			// A token whose subject resolves to no identity is an anonymous
			// token — the token service issues one to an unauthenticated caller
			// so that public pulls work. Holding such a token must not make a
			// request count as authenticated, or presenting one would bypass
			// every check that distinguishes an anonymous caller.
			if uuid, ok := tokenSubjectUUID(claims.Subject); ok {
				id, err := s.opts.Identities.ByUUID(r.Context(), uuid)
				if err == nil {
					if usableErr := id.Usable(); usableErr != nil {
						s.countAuthFailure("identity_unusable")
						s.challenge(w, r, ocierrors.New(ocierrors.CodeUnauthorized,
							"authentication required"))
						return
					}
					auth.Identity = id
				}
			}
			auth.Anonymous = auth.Identity == nil

		case strings.HasPrefix(header, "Basic "):
			// Basic credentials directly on /v2 are accepted as a convenience:
			// curl, some CI images, and several deploy tools do this rather
			// than performing the token dance. The token flow remains the
			// advertised mechanism.
			username, password, ok := r.BasicAuth()
			if !ok {
				s.countAuthFailure("malformed_basic")
				s.challenge(w, r, ocierrors.New(ocierrors.CodeUnauthorized,
					"authentication required"))
				return
			}
			id, err := s.opts.Identities.Authenticate(r.Context(), username, password)
			if err != nil {
				s.countAuthFailure("bad_credentials")
				s.logger(r.Context()).Info("failed basic authentication",
					"username", username, "reason", err)
				s.challenge(w, r, ocierrors.New(ocierrors.CodeUnauthorized,
					"authentication required"))
				return
			}
			auth.Identity = id
			auth.Anonymous = false

		default:
			s.countAuthFailure("unsupported_scheme")
			s.challenge(w, r, ocierrors.New(ocierrors.CodeUnauthorized,
				"authentication required"))
			return
		}

		next.ServeHTTP(w, withAuth(r, auth))
	})
}

// challenge writes a 401 with the WWW-Authenticate header the specification
// requires (REQ-OCI-10). A 401 without this header leaves a client with no way
// to discover where to authenticate, which is a hang rather than an error.
func (s *Service) challenge(w http.ResponseWriter, r *http.Request, errs *ocierrors.Errors) {
	header := http.Header{}
	header.Set("WWW-Authenticate", s.challengeValue(r))
	ocierrors.ServeJSON(w, errs, header)
}

// challengeValue builds the Bearer challenge, including the scope the client
// would need. Supplying the scope lets a client request exactly the right token
// in one round trip instead of guessing.
func (s *Service) challengeValue(r *http.Request) string {
	value := fmt.Sprintf("Bearer realm=%q,service=%q", s.opts.Realm, s.opts.Service)
	if scope := scopeForRequest(r); scope != "" {
		value += fmt.Sprintf(",scope=%q", scope)
	}
	return value
}

// scopeForRequest derives the token scope a request needs.
func scopeForRequest(r *http.Request) string {
	path := r.URL.EscapedPath()
	if path == "/v2/" || path == "/v2" {
		return ""
	}
	if strings.HasPrefix(path, "/v2/_catalog") {
		return "registry:catalog:*"
	}
	name, ok := repositoryNameFromPath(path)
	if !ok {
		return ""
	}

	// The endpoint class must be derived here, not left blank. requiredAction
	// resolves DELETE differently per class, and advertising the wrong scope
	// hands the client a token that cannot do what it asked for — which it then
	// discovers as a 404, with no way to recover.
	action := scopeActionFor(requiredAction(r.Method, endpointClassForPath(path)))

	if action == "pull" {
		return fmt.Sprintf("repository:%s:pull", name)
	}
	// A client that needs to write also needs to read, and requesting both
	// avoids a second challenge on the HEAD that precedes every push.
	return fmt.Sprintf("repository:%s:pull,%s", name, action)
}

// endpointClassForPath infers which endpoint a path belongs to, for computing
// the challenge scope before the router has run.
//
// This duplicates a decision the router also makes, which is worth being
// uneasy about — but the challenge has to be built while handling a 401 from
// the authentication middleware, which sits above the router. The two are kept
// consistent by a test that walks every route and checks the classes agree.
func endpointClassForPath(path string) string {
	switch {
	case strings.HasSuffix(path, "/blobs/uploads"), strings.HasSuffix(path, "/blobs/uploads/"):
		return "upload_start"
	case strings.Contains(path, "/blobs/uploads/"):
		return "upload_session"
	case strings.Contains(path, "/manifests/"):
		return "manifest"
	case strings.Contains(path, "/blobs/"):
		return "blob"
	case strings.HasSuffix(path, "/tags/list"):
		return "tags_list"
	case strings.Contains(path, "/referrers/"):
		return "referrers"
	default:
		return "unknown"
	}
}

// repositoryNameFromPath extracts the repository name from a /v2 path, for the
// challenge only. The router remains the authority for dispatch.
func repositoryNameFromPath(path string) (string, bool) {
	trimmed := strings.TrimPrefix(path, "/v2/")
	if trimmed == path {
		return "", false
	}
	for _, suffix := range []string{"/blobs/", "/manifests/", "/tags/list", "/referrers/"} {
		if i := strings.LastIndex(trimmed, suffix); i > 0 {
			return trimmed[:i], true
		}
	}
	return "", false
}

func tokenSubjectUUID(subject string) (string, bool) {
	parts := strings.Split(subject, ":")
	if len(parts) != 3 || parts[0] != "mantle" {
		return "", false
	}
	return parts[2], true
}

// authorizeRepository is the single authorization gate for the /v2 surface.
//
// It returns the repository when access is permitted. When it is not, it has
// already written the response, and the handler must return immediately.
//
// The response shape implements REQ-OCI-11, which is subtle and load-bearing:
//
//   - An unauthenticated caller always gets 401 with a challenge, whether the
//     repository is private or does not exist. The two must be
//     indistinguishable, or an anonymous prober can enumerate the private
//     namespace — and namespaces leak customer names.
//   - An authenticated caller without permission gets 404 NAME_UNKNOWN, not
//     403. A 403 would confirm the repository exists.
//
// The only case that yields 403 DENIED is one where the caller demonstrably
// knows the repository exists and is being refused on policy grounds.
func (s *Service) authorizeRepository(w http.ResponseWriter, r *http.Request, name string, action authz.Action) (*catalog.Repository, bool) {
	if errs := validateName(name); errs != nil {
		ocierrors.ServeJSON(w, errs, nil)
		return nil, false
	}

	auth := authFrom(r.Context())
	ctx := r.Context()

	repo, err := s.opts.Catalog.RepositoryByName(ctx, name)
	switch {
	case errors.Is(err, catalog.ErrNotFound):
		repo = nil
	case err != nil:
		s.serveInternal(w, r, "looking up repository", err)
		return nil, false
	}

	// Public repositories are readable without authentication when anonymous
	// pull is enabled.
	if action == authz.ActionPull && repo != nil && repo.IsPublic() {
		if !auth.Anonymous || s.opts.AnonymousPull {
			return repo, true
		}
	}

	if auth.Anonymous {
		s.countAuthFailure("anonymous_denied")
		s.challenge(w, r, ocierrors.New(ocierrors.CodeUnauthorized, "authentication required"))
		return nil, false
	}

	// The token's own access claim is checked first and is authoritative
	// (SEC-09). A token that does not carry the scope cannot be widened by a
	// database grant, because the grant may have been added after the token was
	// issued and the token is the statement of what was authorised.
	if auth.Claims != nil && !auth.Identity.InstanceAdmin {
		if !auth.Claims.Allows("repository", name, scopeActionFor(action)) {
			s.countAuthFailure("scope_insufficient")
			s.notFoundOrDenied(w, r, name, repo, "token scope does not permit this action")
			return nil, false
		}
	}

	// Then the live permission set, re-evaluated per request (REQ-AUTHZ-02), so
	// a revoked grant stops working without waiting for token expiry.
	permitted, err := s.hasPermission(r, name, repo, action)
	if err != nil {
		s.serveInternal(w, r, "evaluating permissions", err)
		return nil, false
	}
	if !permitted {
		s.countAuthFailure("permission_denied")
		s.notFoundOrDenied(w, r, name, repo, "insufficient permission")
		return nil, false
	}

	return repo, true
}

// hasPermission evaluates the caller's live permissions.
func (s *Service) hasPermission(r *http.Request, name string, repo *catalog.Repository, action authz.Action) (bool, error) {
	auth := authFrom(r.Context())
	if auth.Identity == nil {
		return false, nil
	}
	if auth.Identity.InstanceAdmin {
		return true, nil
	}

	// A repository that does not exist yet is authorised against the name, so
	// that a namespace grant permits creating it on first push.
	if repo == nil {
		permissions, err := s.opts.Identities.PermissionsForName(r.Context(), auth.Identity.ID, name)
		if err != nil {
			return false, err
		}
		return permissions.Has(action), nil
	}

	permissions, err := s.opts.Identities.PermissionsForRepository(r.Context(), auth.Identity.ID, repo.ID)
	if err != nil {
		return false, err
	}
	return permissions.Has(action), nil
}

// notFoundOrDenied implements the authenticated half of REQ-OCI-11.
func (s *Service) notFoundOrDenied(w http.ResponseWriter, r *http.Request, name string, repo *catalog.Repository, reason string) {
	s.logger(r.Context()).Info("denied registry request",
		"repository", name,
		"actor", authFrom(r.Context()).Name(),
		"method", r.Method,
		"reason", reason)
	// 404, always — including when the repository exists. Answering 403 here
	// would tell the caller that a repository they cannot read is nonetheless
	// there.
	ocierrors.ServeJSON(w, ocierrors.NameUnknown(name), nil)
}

// requireRepository is authorizeRepository for handlers that cannot operate on
// a repository that does not exist yet.
func (s *Service) requireRepository(w http.ResponseWriter, r *http.Request, name string, action authz.Action) (*catalog.Repository, bool) {
	repo, ok := s.authorizeRepository(w, r, name, action)
	if !ok {
		return nil, false
	}
	if repo == nil {
		ocierrors.ServeJSON(w, ocierrors.NameUnknown(name), nil)
		return nil, false
	}
	return repo, true
}

func (s *Service) countAuthFailure(reason string) {
	if s.opts.Metrics != nil {
		s.opts.Metrics.AuthFailures.WithLabelValues(reason).Inc()
	}
}

// serveInternal logs an unexpected failure and returns a generic error.
//
// The message the client sees never contains the underlying error. Internal
// errors carry connection strings, file paths, and occasionally credentials,
// and a registry error body is one of the most widely pasted strings in any
// support channel (SEC-12).
func (s *Service) serveInternal(w http.ResponseWriter, r *http.Request, what string, err error) {
	s.logger(r.Context()).Error("registry request failed", "operation", what, "error", err)

	// The client is given the request id and nothing else. Correlating it with
	// the log line costs an operator one grep and costs an attacker the
	// connection strings, paths, and occasional credentials that internal
	// errors carry.
	message := "the registry encountered an internal error"
	if id := observability.RequestID(r.Context()); id != "" {
		message += " (request " + id + ")"
	}
	ocierrors.ServeJSON(w, ocierrors.New(ocierrors.CodeUnknown, message), nil)
}
