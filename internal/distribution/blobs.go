package distribution

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/mantle-sh/mantle/internal/auth/authz"
	"github.com/mantle-sh/mantle/internal/catalog"
	ocierrors "github.com/mantle-sh/mantle/internal/distribution/errors"
	"github.com/mantle-sh/mantle/internal/events"
	"github.com/mantle-sh/mantle/internal/oci"
	"github.com/mantle-sh/mantle/internal/storage/driver"
)

// handleBlobGet implements end-2: GET and HEAD of a blob.
//
// HEAD must return exactly the headers GET would, including Content-Length and
// Content-Type, with no body (REQ-OCI-03). Docker issues a HEAD before every
// layer push to decide whether to upload, so a HEAD that disagrees with its GET
// causes either a redundant multi-gigabyte upload or a push that references a
// blob the registry cannot serve.
func (s *Service) handleBlobGet(w http.ResponseWriter, r *http.Request, p params) {
	digest, errs := parseDigestParam(p.Digest)
	if errs != nil {
		ocierrors.ServeJSON(w, errs, nil)
		return
	}

	repo, ok := s.requireRepository(w, r, p.Name, authz.ActionPull)
	if !ok {
		return
	}

	blob, err := s.opts.Catalog.BlobInRepository(r.Context(), repo.ID, digest.String())
	if errors.Is(err, catalog.ErrNotFound) {
		ocierrors.ServeJSON(w, ocierrors.BlobUnknown(digest.String()), nil)
		return
	}
	if err != nil {
		s.serveInternal(w, r, "looking up blob", err)
		return
	}

	setContentDigest(w, blob.Digest)
	contentType := blob.MediaType
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	w.Header().Set("Content-Type", contentType)
	// Content is immutable by construction: the URL is its hash. A long
	// immutable cache directive is safe and saves a container host from
	// re-fetching layers it already has.
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")

	if r.Method == http.MethodHead {
		w.Header().Set("Content-Length", strconv.FormatInt(blob.SizeBytes, 10))
		w.WriteHeader(http.StatusOK)
		return
	}

	s.recordPull(r, repo.ID, nil, digest.String(), digest.String(), "blob")

	// Offer a redirect where the backend can presign, unless policy forbids it
	// for this repository (§7.3, D-04).
	if s.opts.RedirectBlobs {
		if url, ok := s.presignedURL(r, repo, digest); ok {
			w.Header().Set("Content-Length", "0")
			http.Redirect(w, r, url, http.StatusTemporaryRedirect)
			return
		}
	}

	reader, err := s.opts.Storage.Open(r.Context(), digest)
	if errors.Is(err, driver.ErrNotFound) {
		// The row exists but the bytes do not. This is the "dangling row"
		// condition that §12.2's reconcile phase reports, and it is a
		// correctness alarm rather than a routine 404.
		s.logger(r.Context()).Error("blob metadata exists but its content is missing from storage",
			"digest", digest.String(), "repository", repo.Name,
			"remedy", "run 'mantle gc reconcile' to identify dangling rows")
		ocierrors.ServeJSON(w, ocierrors.BlobUnknown(digest.String()), nil)
		return
	}
	if err != nil {
		s.serveInternal(w, r, "opening blob", err)
		return
	}
	defer reader.Close()

	// ServeContent handles Range, If-Range, and partial responses. Range
	// support is what lets a container runtime resume an interrupted layer
	// pull instead of restarting a multi-gigabyte download.
	http.ServeContent(w, r, "", blob.CreatedAt, reader)
	s.observeBytesOut("blob", blob.SizeBytes)
}

// presignedURL returns a direct storage URL where that is both supported and
// safe.
func (s *Service) presignedURL(r *http.Request, repo *catalog.Repository, digest oci.Digest) (string, bool) {
	ttl := s.opts.PresignTTL

	// Never issue a presigned URL for a private repository that outlives the
	// token which authorised it (§7.3). A URL that remains fetchable after the
	// caller's access was revoked is an unauthenticated copy of private data.
	if !repo.IsPublic() {
		tokenTTL := s.opts.Issuer.TTL()
		if tokenTTL > 0 && ttl > tokenTTL {
			ttl = tokenTTL
		}
	}
	if ttl <= 0 {
		return "", false
	}

	url, ok, err := s.opts.Storage.PresignGet(r.Context(), digest, ttl)
	if err != nil {
		// Fall back to proxying. A presign failure must degrade to a slower
		// pull, never to a failed one.
		s.logger(r.Context()).Warn("presigning blob URL failed; proxying instead",
			"digest", digest.String(), "error", err)
		return "", false
	}
	return url, ok
}

// handleBlobDelete implements end-10.
//
// The blob is unlinked from this repository rather than erased. Actual deletion
// belongs to garbage collection, which has the grace window and the quarantine
// that make a mistake recoverable; deleting bytes here would bypass both
// (§12.3).
func (s *Service) handleBlobDelete(w http.ResponseWriter, r *http.Request, p params) {
	digest, errs := parseDigestParam(p.Digest)
	if errs != nil {
		ocierrors.ServeJSON(w, errs, nil)
		return
	}

	repo, ok := s.requireRepository(w, r, p.Name, authz.ActionDeleteTag)
	if !ok {
		return
	}

	err := s.opts.Catalog.UnlinkBlob(r.Context(), repo.ID, digest.String())
	if errors.Is(err, catalog.ErrNotFound) {
		ocierrors.ServeJSON(w, ocierrors.BlobUnknown(digest.String()), nil)
		return
	}
	if err != nil {
		s.serveInternal(w, r, "unlinking blob", err)
		return
	}

	s.logger(r.Context()).Info("unlinked blob from repository",
		"repository", repo.Name, "digest", digest.String(), "actor", authFrom(r.Context()).Name())

	setContentDigest(w, digest.String())
	w.WriteHeader(http.StatusAccepted)
}

// parseDigestParam validates a digest from the URL, distinguishing a malformed
// digest from an unsupported algorithm (REQ-OCI-01).
func parseDigestParam(raw string) (oci.Digest, *ocierrors.Errors) {
	digest, err := oci.ParseDigest(raw)
	if err == nil {
		return digest, nil
	}
	var unsupported *oci.ErrUnsupportedAlgorithm
	if errors.As(err, &unsupported) {
		return oci.Digest{}, ocierrors.Unsupported(unsupported.Error())
	}
	return oci.Digest{}, ocierrors.DigestInvalid(err.Error())
}

// recordPull hands an observation to the ledger without blocking (REQ-LEDGER-01).
func (s *Service) recordPull(r *http.Request, repoID int64, manifestID *int64, reference, digest, kind string) {
	auth := authFrom(r.Context())
	s.opts.Events.RecordPull(events.Pull{
		RepositoryID: repoID,
		ManifestID:   manifestID,
		Reference:    reference,
		Digest:       digest,
		IdentityID:   auth.IdentityID(),
		Address:      clientAddress(r),
		UserAgent:    r.UserAgent(),
		Kind:         kind,
		OccurredAt:   time.Now(),
	})
}
