// Package admin implements the versioned administrative REST API (§2.2).
//
// This is the product surface, not an afterthought. The CLI is its only
// consumer today, which makes it tempting to shape endpoints around exactly
// that one caller — the temptation is the thing to resist. A later `mantle-ui`
// will need list endpoints with pagination, partial updates, and stable
// resource shapes, and it will be an ordinary client of this same API with no
// privileged access of its own.
//
// Two rules follow, and both are cheaper to honour now than to retrofit:
// the path is versioned /api/v1/ from the first commit and breaking changes are
// treated as breaking; and no CLI command may reach past this API into the
// database or the filesystem, so that the two surfaces stay honest and the CLI
// works against a remote instance.
package admin

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mantle-sh/mantle/internal/auth/identity"
	"github.com/mantle-sh/mantle/internal/auth/token"
	"github.com/mantle-sh/mantle/internal/catalog"
	"github.com/mantle-sh/mantle/internal/gc"
	"github.com/mantle-sh/mantle/internal/ledger"
	"github.com/mantle-sh/mantle/internal/observability"
)

// Options wires the admin API.
type Options struct {
	Pool       *pgxpool.Pool
	Catalog    *catalog.Store
	Identities *identity.Store
	Ledger     *ledger.Store
	Collector  *gc.Collector
	Verifier   *token.Verifier
	Logger     *slog.Logger
	Version    string
	// RollbackDepth is how many generations behind the live image stay pinned,
	// reported by the ledger view so the number is never a mystery.
	RollbackDepth int
}

// API serves /api/v1/.
type API struct {
	opts   Options
	routes []adminRoute
}

type adminRoute struct {
	pattern *regexp.Regexp
	methods map[string]adminHandler
	// adminOnly restricts the route to instance administrators. Routes without
	// it perform their own, finer-grained check.
	adminOnly bool
}

type adminHandler func(w http.ResponseWriter, r *http.Request, m []string, actor *identity.Identity)

// New builds the admin API.
func New(opts Options) *API {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	a := &API{opts: opts}
	a.routes = a.buildRoutes()
	return a
}

// repoNamePattern matches a full OCI repository path, which contains slashes
// and so cannot be captured by a single path segment.
const repoNamePattern = `([a-z0-9._/-]+)`

func (a *API) buildRoutes() []adminRoute {
	return []adminRoute{
		{
			pattern: regexp.MustCompile(`^/api/v1/version$`),
			methods: map[string]adminHandler{http.MethodGet: a.handleVersion},
		},
		{
			pattern:   regexp.MustCompile(`^/api/v1/organizations$`),
			adminOnly: true,
			methods: map[string]adminHandler{
				http.MethodGet:  a.handleListOrganizations,
				http.MethodPost: a.handleCreateOrganization,
			},
		},
		{
			pattern: regexp.MustCompile(`^/api/v1/repositories$`),
			methods: map[string]adminHandler{
				http.MethodGet:  a.handleListRepositories,
				http.MethodPost: a.handleCreateRepository,
			},
		},
		{
			pattern: regexp.MustCompile(`^/api/v1/repositories/` + repoNamePattern + `/ledger$`),
			methods: map[string]adminHandler{http.MethodGet: a.handleRepositoryLedger},
		},
		{
			pattern: regexp.MustCompile(`^/api/v1/repositories/` + repoNamePattern + `$`),
			methods: map[string]adminHandler{
				http.MethodGet:    a.handleGetRepository,
				http.MethodPatch:  a.handleUpdateRepository,
				http.MethodDelete: a.handleDeleteRepository,
			},
		},
		{
			pattern:   regexp.MustCompile(`^/api/v1/users$`),
			adminOnly: true,
			methods: map[string]adminHandler{
				http.MethodGet:  a.handleListUsers,
				http.MethodPost: a.handleCreateUser,
			},
		},
		{
			pattern:   regexp.MustCompile(`^/api/v1/tokens$`),
			adminOnly: true,
			methods: map[string]adminHandler{
				http.MethodGet:  a.handleListTokens,
				http.MethodPost: a.handleCreateToken,
			},
		},
		{
			pattern:   regexp.MustCompile(`^/api/v1/tokens/([0-9a-f-]+)$`),
			adminOnly: true,
			methods:   map[string]adminHandler{http.MethodDelete: a.handleRevokeToken},
		},
		{
			pattern:   regexp.MustCompile(`^/api/v1/grants$`),
			adminOnly: true,
			methods:   map[string]adminHandler{http.MethodPost: a.handleCreateGrant},
		},
		{
			// The Tier 1 deploy-reporting endpoint (§13.2). Not admin-only: a
			// deploy token must be able to call it, since it is the one thing
			// the user has to wire up themselves.
			pattern: regexp.MustCompile(`^/api/v1/deployments$`),
			methods: map[string]adminHandler{
				http.MethodPost: a.handleRecordDeployment,
				http.MethodGet:  a.handleListDeployments,
			},
		},
		{
			pattern:   regexp.MustCompile(`^/api/v1/gc/run$`),
			adminOnly: true,
			methods:   map[string]adminHandler{http.MethodPost: a.handleRunGC},
		},
		{
			pattern:   regexp.MustCompile(`^/api/v1/gc/status$`),
			adminOnly: true,
			methods:   map[string]adminHandler{http.MethodGet: a.handleGCStatus},
		},
		{
			pattern:   regexp.MustCompile(`^/api/v1/gc/reconcile$`),
			adminOnly: true,
			methods:   map[string]adminHandler{http.MethodPost: a.handleReconcile},
		},
	}
}

// ServeHTTP authenticates and dispatches an admin request.
func (a *API) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	actor, err := a.authenticate(r)
	if err != nil {
		w.Header().Set("WWW-Authenticate", `Basic realm="mantle admin"`)
		writeError(w, http.StatusUnauthorized, "unauthorized", err.Error())
		return
	}

	path := r.URL.EscapedPath()
	for _, route := range a.routes {
		matches := route.pattern.FindStringSubmatch(path)
		if matches == nil {
			continue
		}
		handler, ok := route.methods[r.Method]
		if !ok {
			w.Header().Set("Allow", adminAllow(route.methods))
			writeError(w, http.StatusMethodNotAllowed, "method_not_allowed",
				fmt.Sprintf("%s is not supported on this resource", r.Method))
			return
		}
		if route.adminOnly && (actor == nil || !actor.InstanceAdmin) {
			writeError(w, http.StatusForbidden, "forbidden",
				"this operation requires instance administrator rights")
			return
		}
		handler(w, r, matches, actor)
		return
	}

	writeError(w, http.StatusNotFound, "not_found", "no such API resource")
}

// authenticate resolves the caller.
//
// Both a registry bearer token and HTTP Basic credentials are accepted, so the
// CLI can reuse whatever the operator already has configured rather than
// needing a second credential type.
func (a *API) authenticate(r *http.Request) (*identity.Identity, error) {
	header := r.Header.Get("Authorization")
	switch {
	case strings.HasPrefix(header, "Bearer "):
		raw := strings.TrimSpace(strings.TrimPrefix(header, "Bearer "))
		claims, err := a.opts.Verifier.Verify(raw)
		if err != nil {
			return nil, fmt.Errorf("the presented token is not valid")
		}
		uuid, ok := token.SubjectIdentityUUID(claims.Subject)
		if !ok {
			return nil, fmt.Errorf("authentication required")
		}
		actor, err := a.opts.Identities.ByUUID(r.Context(), uuid)
		if err != nil {
			return nil, fmt.Errorf("authentication required")
		}
		if err := actor.Usable(); err != nil {
			return nil, fmt.Errorf("this credential is no longer usable")
		}
		return actor, nil

	case strings.HasPrefix(header, "Basic "):
		username, password, ok := r.BasicAuth()
		if !ok {
			return nil, fmt.Errorf("malformed credentials")
		}
		actor, err := a.opts.Identities.Authenticate(r.Context(), username, password)
		if err != nil {
			return nil, fmt.Errorf("invalid username or password")
		}
		return actor, nil

	default:
		return nil, fmt.Errorf("authentication required")
	}
}

func adminAllow(methods map[string]adminHandler) string {
	ordered := []string{http.MethodGet, http.MethodPost, http.MethodPatch, http.MethodDelete}
	var allowed []string
	for _, m := range ordered {
		if _, ok := methods[m]; ok {
			allowed = append(allowed, m)
		}
	}
	return strings.Join(allowed, ", ")
}

// --- response helpers ---

// errorBody is the admin API's error shape. It is deliberately different from
// the OCI envelope: this is not the registry protocol, and pretending it is
// would mislead anyone writing a client against it.
type errorBody struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
		// Remedy is what to do about it, when there is something to do.
		Remedy string `json:"remedy,omitempty"`
	} `json:"error"`
}

func writeError(w http.ResponseWriter, status int, code, message string, remedy ...string) {
	var body errorBody
	body.Error.Code = code
	body.Error.Message = message
	if len(remedy) > 0 {
		body.Error.Remedy = remedy[0]
	}
	writeJSON(w, status, body)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	encoded, err := json.Marshal(body)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(len(encoded)))
	w.WriteHeader(status)
	_, _ = w.Write(encoded)
}

// decodeBody reads a JSON request body with a size limit.
func decodeBody(r *http.Request, into any) error {
	decoder := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20))
	// Unknown fields are rejected here, unlike in manifest parsing. A typo in an
	// administrative request should fail loudly rather than silently not apply
	// the setting the operator believed they were changing.
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(into); err != nil {
		return fmt.Errorf("invalid request body: %w", err)
	}
	return nil
}

// internalError logs and returns a generic failure.
func (a *API) internalError(w http.ResponseWriter, r *http.Request, what string, err error) {
	a.opts.Logger.Error("admin API request failed", "operation", what, "error", err,
		"request_id", observability.RequestID(r.Context()))
	writeError(w, http.StatusInternalServerError, "internal_error",
		"the registry encountered an internal error",
		"check the daemon log for request "+observability.RequestID(r.Context()))
}

func (a *API) handleVersion(w http.ResponseWriter, r *http.Request, _ []string, _ *identity.Identity) {
	writeJSON(w, http.StatusOK, map[string]string{
		"version": a.opts.Version,
		"api":     "v1",
	})
}

// notFound reports a missing resource consistently.
func notFound(w http.ResponseWriter, kind, name string) {
	writeError(w, http.StatusNotFound, "not_found", fmt.Sprintf("%s %q does not exist", kind, name))
}

// mapStoreError turns a catalog error into an HTTP response, returning whether
// it handled one.
func mapStoreError(w http.ResponseWriter, err error, kind, name string) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, catalog.ErrNotFound), errors.Is(err, identity.ErrNotFound),
		errors.Is(err, ledger.ErrNotFound):
		notFound(w, kind, name)
	case errors.Is(err, catalog.ErrAlreadyExists), errors.Is(err, identity.ErrAlreadyExists):
		writeError(w, http.StatusConflict, "already_exists",
			fmt.Sprintf("%s %q already exists", kind, name))
	case errors.Is(err, catalog.ErrStillReferenced):
		writeError(w, http.StatusConflict, "still_referenced", err.Error())
	case errors.Is(err, catalog.ErrImmutable):
		writeError(w, http.StatusConflict, "immutable", err.Error())
	default:
		return false
	}
	return true
}
