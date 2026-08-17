package distribution

import (
	"net/http"

	"github.com/mantle-sh/mantle/internal/auth/authz"
	ocierrors "github.com/mantle-sh/mantle/internal/distribution/errors"
)

// handleBase implements end-1: GET /v2/.
//
// This is the endpoint every client hits first to discover the API version and
// to learn how to authenticate. It must answer 200 for a caller who can
// authenticate and 401 with a challenge for one who cannot — a client reads the
// challenge here and uses it for everything that follows, so getting this
// wrong breaks login before any image is involved.
func (s *Service) handleBase(w http.ResponseWriter, r *http.Request, _ params) {
	auth := authFrom(r.Context())

	// An anonymous caller is challenged unless anonymous pull is on. `docker
	// login` depends on this: it presents credentials to /v2/ and treats a 200
	// as a successful login.
	if auth.Anonymous && !s.opts.AnonymousPull {
		s.challenge(w, r, ocierrors.New(ocierrors.CodeUnauthorized, "authentication required"))
		return
	}

	w.Header().Set("Docker-Distribution-API-Version", "registry/2.0")
	w.Header().Set("Content-Length", "2")
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write([]byte("{}"))
	}
}

// handleCatalog implements GET /v2/_catalog.
//
// Not part of the OCI specification, but universally expected by tooling. It is
// gated behind authentication and filtered to what the caller may read: an
// unfiltered catalog hands over the private repository namespace, and
// namespaces leak customer names (SEC-04).
func (s *Service) handleCatalog(w http.ResponseWriter, r *http.Request, _ params) {
	auth := authFrom(r.Context())
	if auth.Anonymous && !s.opts.AnonymousPull {
		s.challenge(w, r, ocierrors.New(ocierrors.CodeUnauthorized, "authentication required"))
		return
	}

	n, last, errs := paginationParams(r, s.opts.PaginationDefault, s.opts.PaginationMax)
	if errs != nil {
		ocierrors.ServeJSON(w, errs, nil)
		return
	}

	page, err := s.opts.Catalog.ListRepositories(r.Context(),
		auth.visibilityFilter(s.opts.AnonymousPull), n, last)
	if err != nil {
		s.serveInternal(w, r, "listing repositories", err)
		return
	}

	if page.HasMore && len(page.Names) > 0 {
		setLinkHeader(w, r, n, page.Names[len(page.Names)-1])
	}
	if err := writeJSON(w, http.StatusOK, map[string]any{
		"repositories": page.Names,
	}); err != nil {
		s.logger(r.Context()).Warn("writing catalog response", "error", err)
	}
}

// handleTagsList implements end-8a and end-8b.
func (s *Service) handleTagsList(w http.ResponseWriter, r *http.Request, p params) {
	repo, ok := s.requireRepository(w, r, p.Name, authz.ActionPull)
	if !ok {
		return
	}

	n, last, errs := paginationParams(r, s.opts.PaginationDefault, s.opts.PaginationMax)
	if errs != nil {
		ocierrors.ServeJSON(w, errs, nil)
		return
	}

	page, err := s.opts.Catalog.ListTags(r.Context(), repo.ID, n, last)
	if err != nil {
		s.serveInternal(w, r, "listing tags", err)
		return
	}

	if page.HasMore && len(page.Tags) > 0 {
		setLinkHeader(w, r, n, page.Tags[len(page.Tags)-1])
	}
	if err := writeJSON(w, http.StatusOK, map[string]any{
		"name": repo.Name,
		"tags": page.Tags,
	}); err != nil {
		s.logger(r.Context()).Warn("writing tag list response", "error", err)
	}
}
