package distribution

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/mantle-sh/mantle/internal/auth/authz"
	"github.com/mantle-sh/mantle/internal/catalog"
	ocierrors "github.com/mantle-sh/mantle/internal/distribution/errors"
	"github.com/mantle-sh/mantle/internal/events"
	"github.com/mantle-sh/mantle/internal/oci"
)

// handleManifestGet implements end-3: GET and HEAD of a manifest.
//
// This is the first request of every pull and the most latency-sensitive
// operation in the registry (§16.3: p99 under 50 ms). It is one indexed query
// returning bytes straight from the row, with no storage round-trip.
func (s *Service) handleManifestGet(w http.ResponseWriter, r *http.Request, p params) {
	if err := oci.ValidateReference(p.Reference); err != nil {
		ocierrors.ServeJSON(w, ocierrors.ManifestUnknown(p.Reference), nil)
		return
	}

	repo, ok := s.requireRepository(w, r, p.Name, authz.ActionPull)
	if !ok {
		return
	}

	manifest, err := s.opts.Catalog.ManifestByReference(r.Context(), repo.ID, p.Reference)
	if errors.Is(err, catalog.ErrNotFound) {
		ocierrors.ServeJSON(w, ocierrors.ManifestUnknown(p.Reference), nil)
		return
	}
	if err != nil {
		s.serveInternal(w, r, "looking up manifest", err)
		return
	}

	// Content negotiation. A client that lists acceptable manifest types and
	// gets something else back will fail to parse it, so an explicit Accept
	// that excludes this manifest's type is answered with 404 rather than a
	// document the client cannot read.
	if !acceptsMediaType(r, manifest.MediaType) {
		ocierrors.ServeJSON(w, ocierrors.WithDetail(ocierrors.CodeManifestUnknown,
			fmt.Sprintf("manifest is %s, which the request's Accept header does not permit",
				manifest.MediaType),
			map[string]string{"Tag": p.Reference, "MediaType": manifest.MediaType}), nil)
		return
	}

	// REQ-OCI-03: HEAD returns identical headers to GET, with no body.
	w.Header().Set("Content-Type", manifest.MediaType)
	w.Header().Set("Content-Length", strconv.Itoa(manifest.SizeBytes))
	setContentDigest(w, manifest.Digest)

	if oci.IsDigestReference(p.Reference) {
		// A manifest addressed by digest is immutable; one addressed by tag is
		// not, and caching it would serve a stale image after a tag move.
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		w.Header().Set("Cache-Control", "no-cache")
	}

	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		s.recordPull(r, repo.ID, &manifest.ID, p.Reference, manifest.Digest, "manifest")
		return
	}

	w.WriteHeader(http.StatusOK)
	// REQ-OCI-02: the stored bytes, verbatim. Nothing between the database row
	// and the socket re-encodes them, because the digest is over exactly these
	// bytes and any reserialization would change it.
	if _, err := w.Write(manifest.Payload); err != nil {
		s.logger(r.Context()).Debug("client disconnected during manifest write", "error", err)
		return
	}
	s.observeBytesOut("manifest", int64(manifest.SizeBytes))
	s.recordPull(r, repo.ID, &manifest.ID, p.Reference, manifest.Digest, "manifest")
}

// handleManifestPut implements end-7.
func (s *Service) handleManifestPut(w http.ResponseWriter, r *http.Request, p params) {
	if err := oci.ValidateReference(p.Reference); err != nil {
		ocierrors.ServeJSON(w, ocierrors.New(ocierrors.CodeManifestInvalid,
			"invalid manifest reference: "+err.Error()), nil)
		return
	}

	repo, ok := s.authorizeRepository(w, r, p.Name, authz.ActionPush)
	if !ok {
		return
	}
	auth := authFrom(r.Context())
	if auth.Identity == nil {
		s.challenge(w, r, ocierrors.New(ocierrors.CodeUnauthorized, "authentication required"))
		return
	}

	if repo == nil {
		created, err := s.opts.Catalog.EnsureRepository(r.Context(), p.Name, s.opts.DefaultOrg, auth.IdentityID())
		if err != nil {
			ocierrors.ServeJSON(w, ocierrors.Denied(err.Error()), nil)
			return
		}
		repo = created
	}

	// Read with a hard limit rather than trusting Content-Length (SEC-05).
	limit := s.opts.Limits.MaxManifestSize
	payload, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil {
		ocierrors.ServeJSON(w, ocierrors.New(ocierrors.CodeManifestInvalid,
			"could not read the manifest body: "+err.Error()), nil)
		return
	}
	if int64(len(payload)) > limit {
		ocierrors.ServeJSON(w, ocierrors.New(ocierrors.CodeManifestInvalid,
			fmt.Sprintf("manifest exceeds the maximum size of %d bytes", limit)), nil)
		return
	}

	contentType := normaliseContentType(r.Header.Get("Content-Type"))
	parsed, err := oci.ParseManifest(payload, contentType, s.opts.Limits)
	if err != nil {
		ocierrors.ServeJSON(w, ocierrors.New(ocierrors.CodeManifestInvalid, err.Error()), nil)
		return
	}

	// The digest is computed over the received bytes, never taken from the
	// client (REQ-OCI-01). When the reference is itself a digest, the two must
	// agree or the client is asking us to store content at an address that is
	// not its hash.
	computed := oci.FromBytes(payload)
	if oci.IsDigestReference(p.Reference) {
		claimed, parseErr := oci.ParseDigest(p.Reference)
		if parseErr != nil {
			ocierrors.ServeJSON(w, ocierrors.DigestInvalid(parseErr.Error()), nil)
			return
		}
		if !computed.Equal(claimed) {
			ocierrors.ServeJSON(w, ocierrors.WithDetail(ocierrors.CodeDigestInvalid,
				"the manifest content does not match the digest in the request path",
				map[string]string{"Expected": claimed.String(), "Actual": computed.String()}), nil)
			return
		}
	}

	tag := ""
	if !oci.IsDigestReference(p.Reference) {
		tag = p.Reference
	}

	// Tag protection (§15.2): repository-wide immutability, or a pattern rule.
	immutable := repo.ImmutableTags
	if tag != "" && !immutable {
		rules, err := s.opts.Catalog.TagProtection(r.Context(), repo.ID)
		if err != nil {
			s.serveInternal(w, r, "loading tag protection rules", err)
			return
		}
		if rule := catalog.MatchProtection(rules, tag); rule != nil && rule.Immutable {
			immutable = true
		}
	}

	result, err := s.opts.Catalog.PutManifest(r.Context(), catalog.PutManifestParams{
		RepositoryID:  repo.ID,
		Digest:        computed.String(),
		Tag:           tag,
		MediaType:     parsed.MediaType,
		Payload:       payload,
		Parsed:        parsed,
		ActorID:       auth.IdentityID(),
		ImmutableTags: immutable,
	})
	if err != nil {
		s.serveManifestWriteError(w, r, err)
		return
	}

	s.observeBytesIn("manifest", int64(len(payload)))
	s.logger(r.Context()).Info("stored manifest",
		"repository", repo.Name, "digest", computed.String(), "tag", tag,
		"media_type", parsed.MediaType, "actor", auth.Name())

	s.opts.Events.RecordPush(events.Push{
		RepositoryID:   repo.ID,
		ManifestID:     result.Manifest.ID,
		Digest:         computed.String(),
		Tag:            tag,
		MediaType:      parsed.MediaType,
		IdentityID:     auth.IdentityID(),
		ConfigDigest:   result.Manifest.ConfigDigest,
		Annotations:    parsed.Annotations,
		OccurredAt:     time.Now(),
		TagMoved:       result.TagMoved,
		PreviousDigest: result.PreviousDigest,
	})

	w.Header().Set("Location", fmt.Sprintf("/v2/%s/manifests/%s", repo.Name, computed))
	setContentDigest(w, computed.String())
	w.Header().Set("Content-Length", "0")
	w.WriteHeader(http.StatusCreated)
}

// serveManifestWriteError maps a catalog failure onto the right OCI code.
func (s *Service) serveManifestWriteError(w http.ResponseWriter, r *http.Request, err error) {
	var missing *catalog.ErrMissingBlobs
	if errors.As(err, &missing) {
		// REQ-OCI-05: name the missing digest so the client knows what to push.
		envelope := &ocierrors.Errors{}
		for _, digest := range missing.Digests {
			envelope = envelope.Append(ocierrors.CodeManifestBlobUnknown,
				"manifest references a blob unknown to this repository",
				map[string]string{"Digest": digest})
		}
		ocierrors.ServeJSON(w, envelope, nil)
		return
	}
	if errors.Is(err, catalog.ErrImmutable) {
		ocierrors.ServeJSON(w, ocierrors.Denied(err.Error()), nil)
		return
	}
	if errors.Is(err, catalog.ErrQuotaExceeded) {
		ocierrors.ServeJSON(w, ocierrors.Denied(err.Error()), nil)
		return
	}
	s.serveInternal(w, r, "writing manifest", err)
}

// handleManifestDelete implements end-9.
func (s *Service) handleManifestDelete(w http.ResponseWriter, r *http.Request, p params) {
	repo, ok := s.requireRepository(w, r, p.Name, authz.ActionDeleteTag)
	if !ok {
		return
	}
	auth := authFrom(r.Context())

	// A tag reference deletes only the tag; a digest reference deletes the
	// manifest. Conflating them would make `docker rmi` of one tag destroy an
	// image other tags still point at.
	if !oci.IsDigestReference(p.Reference) {
		err := s.opts.Catalog.DeleteTag(r.Context(), repo.ID, p.Reference, auth.IdentityID())
		switch {
		case errors.Is(err, catalog.ErrNotFound):
			ocierrors.ServeJSON(w, ocierrors.ManifestUnknown(p.Reference), nil)
		case errors.Is(err, catalog.ErrImmutable):
			ocierrors.ServeJSON(w, ocierrors.Denied(err.Error()), nil)
		case err != nil:
			s.serveInternal(w, r, "deleting tag", err)
		default:
			s.logger(r.Context()).Info("deleted tag",
				"repository", repo.Name, "tag", p.Reference, "actor", auth.Name())
			w.Header().Set("Content-Length", "0")
			w.WriteHeader(http.StatusAccepted)
		}
		return
	}

	digest, errs := parseDigestParam(p.Reference)
	if errs != nil {
		ocierrors.ServeJSON(w, errs, nil)
		return
	}

	err := s.opts.Catalog.DeleteManifest(r.Context(), repo.ID, digest.String(), auth.IdentityID())
	switch {
	case errors.Is(err, catalog.ErrNotFound):
		ocierrors.ServeJSON(w, ocierrors.ManifestUnknown(p.Reference), nil)
	case errors.Is(err, catalog.ErrStillReferenced):
		// The ledger guarantee (§13.4) surfacing at the protocol edge: a
		// deployed image cannot be deleted, and the message says why.
		ocierrors.ServeJSON(w, ocierrors.Denied(err.Error()), nil)
	case err != nil:
		s.serveInternal(w, r, "deleting manifest", err)
	default:
		s.logger(r.Context()).Info("deleted manifest",
			"repository", repo.Name, "digest", digest.String(), "actor", auth.Name())
		w.Header().Set("Content-Length", "0")
		w.WriteHeader(http.StatusAccepted)
	}
}

// handleReferrers implements end-12a and end-12b.
func (s *Service) handleReferrers(w http.ResponseWriter, r *http.Request, p params) {
	digest, errs := parseDigestParam(p.Digest)
	if errs != nil {
		ocierrors.ServeJSON(w, errs, nil)
		return
	}

	repo, ok := s.requireRepository(w, r, p.Name, authz.ActionPull)
	if !ok {
		return
	}

	artifactType := r.URL.Query().Get("artifactType")
	referrers, err := s.opts.Catalog.Referrers(r.Context(), repo.ID, digest.String(), artifactType)
	if err != nil {
		s.serveInternal(w, r, "listing referrers", err)
		return
	}

	manifests := make([]oci.Descriptor, 0, len(referrers))
	for _, ref := range referrers {
		manifests = append(manifests, oci.Descriptor{
			MediaType:    ref.MediaType,
			Digest:       ref.Digest,
			Size:         int64(ref.Size),
			ArtifactType: ref.ArtifactType,
			Annotations:  ref.Annotations,
		})
	}

	// REQ-OCI-09: signal that filtering was actually applied, so a client can
	// tell a filtered empty list from an unfiltered one.
	if artifactType != "" {
		w.Header().Set("OCI-Filters-Applied", "artifactType")
	}
	w.Header().Set("Content-Type", oci.MediaTypeOCIIndex)

	// An unknown subject yields an empty list with 200, not a 404: "are there
	// signatures for this image?" must be answerable with "no".
	if err := writeJSON(w, http.StatusOK, map[string]any{
		"schemaVersion": 2,
		"mediaType":     oci.MediaTypeOCIIndex,
		"manifests":     manifests,
	}); err != nil {
		s.logger(r.Context()).Warn("writing referrers response", "error", err)
	}
}

// normaliseContentType strips parameters such as "; charset=utf-8", which some
// clients append and which would otherwise fail an exact media type comparison.
func normaliseContentType(value string) string {
	for i := 0; i < len(value); i++ {
		if value[i] == ';' {
			return trimSpace(value[:i])
		}
	}
	return trimSpace(value)
}

func trimSpace(s string) string {
	start, end := 0, len(s)
	for start < end && (s[start] == ' ' || s[start] == '\t') {
		start++
	}
	for end > start && (s[end-1] == ' ' || s[end-1] == '\t') {
		end--
	}
	return s[start:end]
}

// acceptsMediaType reports whether the request's Accept header permits a media
// type. An absent or wildcard Accept permits everything, which is what most
// clients send.
func acceptsMediaType(r *http.Request, mediaType string) bool {
	accept := r.Header.Values("Accept")
	if len(accept) == 0 {
		return true
	}
	sawManifestType := false
	for _, header := range accept {
		for _, entry := range splitComma(header) {
			candidate := normaliseContentType(entry)
			if candidate == "" {
				continue
			}
			if candidate == "*/*" || candidate == "application/*" {
				return true
			}
			if candidate == mediaType {
				return true
			}
			if oci.IsManifestMediaType(candidate) {
				sawManifestType = true
			}
		}
	}
	// If the client listed no manifest types at all, it is not doing manifest
	// negotiation — a browser, a health check, a curl with a default Accept —
	// and refusing it would be pedantic.
	return !sawManifestType
}

func splitComma(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == ',' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	return append(out, s[start:])
}
