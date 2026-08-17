package distribution

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/mantle-sh/mantle/internal/auth/authz"
	"github.com/mantle-sh/mantle/internal/catalog"
	ocierrors "github.com/mantle-sh/mantle/internal/distribution/errors"
	"github.com/mantle-sh/mantle/internal/oci"
	"github.com/mantle-sh/mantle/internal/storage/driver"
)

// handleUploadStart implements end-4a, end-4b, and end-11.
//
// One POST serves three quite different operations, distinguished by query
// parameters: begin a chunked upload, complete a whole blob in a single
// request, or mount an existing blob from another repository.
func (s *Service) handleUploadStart(w http.ResponseWriter, r *http.Request, p params) {
	repo, ok := s.authorizeRepository(w, r, p.Name, authz.ActionPush)
	if !ok {
		return
	}

	auth := authFrom(r.Context())
	if auth.Identity == nil {
		s.challenge(w, r, ocierrors.New(ocierrors.CodeUnauthorized, "authentication required"))
		return
	}

	// Create the repository on first push. Authorization already passed against
	// the name, so a namespace grant is what permits this.
	if repo == nil {
		created, err := s.opts.Catalog.EnsureRepository(r.Context(), p.Name, s.opts.DefaultOrg, auth.IdentityID())
		if err != nil {
			ocierrors.ServeJSON(w, ocierrors.Denied(err.Error()), nil)
			return
		}
		repo = created
	}

	query := r.URL.Query()

	// --- end-11: cross-repository mount ---
	if mount := query.Get("mount"); mount != "" {
		if s.tryMount(w, r, repo, mount, query.Get("from")) {
			return
		}
		// Mount declined. The specification says to fall through to a normal
		// upload rather than erroring, which also avoids confirming whether the
		// blob existed in a repository the caller cannot read.
	}

	// Bound concurrent uploads per credential (SEC-07).
	active, err := s.opts.Catalog.CountActiveUploads(r.Context(), auth.Identity.ID)
	if err != nil {
		s.serveInternal(w, r, "counting active uploads", err)
		return
	}
	if active >= s.opts.MaxUploadsPerID {
		w.Header().Set("Retry-After", "30")
		ocierrors.ServeJSON(w, ocierrors.New(ocierrors.CodeTooManyRequests,
			fmt.Sprintf("this credential already has %d upload sessions open, the limit is %d",
				active, s.opts.MaxUploadsPerID)), nil)
		return
	}

	session, err := s.opts.Catalog.CreateUploadSession(r.Context(), repo.ID, auth.Identity.ID,
		"", s.opts.UploadSessionTTL)
	if err != nil {
		s.serveInternal(w, r, "creating upload session", err)
		return
	}

	upload, err := s.opts.Storage.NewUpload(r.Context(), session.ID, oci.SHA256)
	if err != nil {
		_ = s.opts.Catalog.DeleteUploadSession(r.Context(), session.ID)
		s.serveInternal(w, r, "creating upload staging", err)
		return
	}

	// --- end-4b: monolithic upload, body and digest in one request ---
	if digestParam := query.Get("digest"); digestParam != "" {
		s.completeUpload(w, r, repo, session, upload, digestParam, true)
		return
	}

	// --- end-4a: begin a chunked upload ---
	if err := upload.Close(); err != nil {
		s.logger(r.Context()).Warn("closing new upload staging", "error", err)
	}
	if s.opts.Metrics != nil {
		s.opts.Metrics.UploadsActive.Inc()
	}

	s.writeUploadAccepted(w, repo.Name, session.ID, 0, http.StatusAccepted)
}

// tryMount attempts a cross-repository mount, reporting whether it handled the
// request.
//
// REQ-AUTHZ-01. The caller must hold pull on the source repository as well as
// push on the target. Without the source check, anyone who can push anywhere
// could materialise any layer in the instance into a repository they control,
// given only its digest — and layer digests are not secret.
//
// A failed authorization is not an error. It returns false and the caller falls
// back to an ordinary upload, which is the specification-sanctioned behaviour
// and, importantly, does not reveal whether the blob existed.
func (s *Service) tryMount(w http.ResponseWriter, r *http.Request, target *catalog.Repository, mountDigest, from string) bool {
	digest, err := oci.ParseDigest(mountDigest)
	if err != nil {
		return false
	}
	if from == "" {
		return false
	}
	if err := oci.ValidateName(from); err != nil {
		return false
	}

	source, err := s.opts.Catalog.RepositoryByName(r.Context(), from)
	if err != nil {
		return false
	}

	// The source-side permission check. This is the whole point of the
	// requirement and has its own test.
	permitted, err := s.hasPermission(r, from, source, authz.ActionPull)
	if err != nil || !permitted {
		s.logger(r.Context()).Info("declined cross-repository mount",
			"from", from, "to", target.Name, "digest", digest.String(),
			"actor", authFrom(r.Context()).Name(),
			"reason", "caller lacks pull permission on the source repository")
		return false
	}

	blob, err := s.opts.Catalog.BlobInRepository(r.Context(), source.ID, digest.String())
	if err != nil {
		return false
	}
	if err := s.opts.Catalog.LinkBlob(r.Context(), target.ID, blob.ID); err != nil {
		s.logger(r.Context()).Warn("linking mounted blob", "error", err)
		return false
	}

	s.logger(r.Context()).Info("mounted blob across repositories",
		"from", from, "to", target.Name, "digest", digest.String())

	w.Header().Set("Location", fmt.Sprintf("/v2/%s/blobs/%s", target.Name, digest))
	setContentDigest(w, digest.String())
	w.Header().Set("Content-Length", "0")
	w.WriteHeader(http.StatusCreated)
	return true
}

// handleUploadStatus implements end-13: GET the current offset of a session.
func (s *Service) handleUploadStatus(w http.ResponseWriter, r *http.Request, p params) {
	repo, session, ok := s.resolveSession(w, r, p, authz.ActionPush)
	if !ok {
		return
	}
	s.writeUploadAccepted(w, repo.Name, session.ID, session.ByteOffset, http.StatusNoContent)
}

// handleUploadChunk implements end-5: PATCH a chunk into a session.
func (s *Service) handleUploadChunk(w http.ResponseWriter, r *http.Request, p params) {
	repo, session, ok := s.resolveSession(w, r, p, authz.ActionPush)
	if !ok {
		return
	}

	// REQ-OCI-07: a chunk that does not begin exactly where the last one ended
	// is 416, and the response must state the range the server actually holds
	// so the client can resynchronise rather than guess.
	if contentRange := r.Header.Get("Content-Range"); contentRange != "" {
		start, _, err := parseContentRange(contentRange)
		if err != nil {
			ocierrors.ServeJSON(w, ocierrors.New(ocierrors.CodeBlobUploadInvalid, err.Error()), nil)
			return
		}
		if start != session.ByteOffset {
			s.writeRangeMismatch(w, repo.Name, session)
			return
		}
	}

	upload, err := s.resumeUpload(r, session)
	if err != nil {
		s.serveInternal(w, r, "resuming upload", err)
		return
	}
	defer upload.Close()

	written, err := s.copyChunk(r, upload, session.ByteOffset)
	if err != nil {
		var tooLarge *errBlobTooLarge
		if errors.As(err, &tooLarge) {
			_ = upload.Cancel(r.Context())
			_ = s.opts.Catalog.DeleteUploadSession(r.Context(), session.ID)
			ocierrors.ServeJSON(w, ocierrors.New(ocierrors.CodeSizeInvalid, tooLarge.Error()), nil)
			return
		}
		// A client that disconnects mid-chunk is ordinary. The session keeps
		// whatever was durably written and the client resumes from there.
		s.logger(r.Context()).Info("upload chunk interrupted",
			"session", session.ID, "written", written, "error", err)
	}

	state, err := upload.Checkpoint(r.Context())
	if err != nil {
		s.serveInternal(w, r, "checkpointing upload", err)
		return
	}
	if err := s.opts.Catalog.UpdateUploadSession(r.Context(), session.ID,
		session.ByteOffset, state.Offset, state.HashState, state.S3UploadID, state.S3Parts); err != nil {
		if errors.Is(err, catalog.ErrNotFound) {
			// The recorded offset moved under us: another request is writing
			// to the same session. Refusing is the only safe answer, since the
			// two writers' bytes would interleave.
			ocierrors.ServeJSON(w, ocierrors.New(ocierrors.CodeBlobUploadInvalid,
				"this upload session was modified concurrently; restart the upload"), nil)
			return
		}
		s.serveInternal(w, r, "recording upload progress", err)
		return
	}

	s.observeBytesIn("upload", written)
	s.writeUploadAccepted(w, repo.Name, session.ID, state.Offset, http.StatusAccepted)
}

// handleUploadComplete implements end-6: PUT to finalise a session.
func (s *Service) handleUploadComplete(w http.ResponseWriter, r *http.Request, p params) {
	repo, session, ok := s.resolveSession(w, r, p, authz.ActionPush)
	if !ok {
		return
	}

	digestParam := r.URL.Query().Get("digest")
	if digestParam == "" {
		ocierrors.ServeJSON(w, ocierrors.New(ocierrors.CodeDigestInvalid,
			"the digest query parameter is required to complete an upload"), nil)
		return
	}

	upload, err := s.resumeUpload(r, session)
	if err != nil {
		s.serveInternal(w, r, "resuming upload", err)
		return
	}
	s.completeUpload(w, r, repo, session, upload, digestParam, false)
}

// completeUpload consumes any remaining body, verifies the digest, and commits.
func (s *Service) completeUpload(w http.ResponseWriter, r *http.Request, repo *catalog.Repository,
	session *catalog.UploadSession, upload driver.Upload, digestParam string, monolithic bool) {

	defer upload.Close()

	expected, err := oci.ParseDigest(digestParam)
	if err != nil {
		var unsupported *oci.ErrUnsupportedAlgorithm
		if errors.As(err, &unsupported) {
			ocierrors.ServeJSON(w, ocierrors.Unsupported(unsupported.Error()), nil)
		} else {
			ocierrors.ServeJSON(w, ocierrors.DigestInvalid(err.Error()), nil)
		}
		return
	}

	// A final PUT may carry the last chunk in its body.
	written, copyErr := s.copyChunk(r, upload, upload.Offset())
	if copyErr != nil {
		var tooLarge *errBlobTooLarge
		if errors.As(copyErr, &tooLarge) {
			_ = upload.Cancel(r.Context())
			_ = s.opts.Catalog.DeleteUploadSession(r.Context(), session.ID)
			ocierrors.ServeJSON(w, ocierrors.New(ocierrors.CodeSizeInvalid, tooLarge.Error()), nil)
			return
		}
		_ = s.opts.Catalog.DeleteUploadSession(r.Context(), session.ID)
		_ = upload.Cancel(r.Context())
		ocierrors.ServeJSON(w, ocierrors.New(ocierrors.CodeBlobUploadInvalid,
			"the upload body was truncated: "+copyErr.Error()), nil)
		return
	}

	if _, err := upload.Checkpoint(r.Context()); err != nil {
		s.serveInternal(w, r, "flushing completed upload", err)
		return
	}

	// REQ-OCI-01. The digest is computed server-side over what was actually
	// received and compared in constant time. The client's claim is never
	// trusted for addressing before this point.
	actual := upload.Digest()
	if !actual.Equal(expected) {
		_ = upload.Cancel(r.Context())
		_ = s.opts.Catalog.DeleteUploadSession(r.Context(), session.ID)
		s.logger(r.Context()).Warn("rejected blob with mismatched digest",
			"repository", repo.Name, "claimed", expected.String(), "computed", actual.String())
		ocierrors.ServeJSON(w, ocierrors.WithDetail(ocierrors.CodeDigestInvalid,
			"the uploaded content does not match the provided digest",
			map[string]string{"Expected": expected.String(), "Actual": actual.String()}), nil)
		return
	}

	size := upload.Offset()

	// Quota is enforced at commit as well as at session start (§15.3), because
	// the size is only known now.
	if status, err := s.opts.Catalog.CheckQuota(r.Context(), repo.OrganizationID, size); err == nil && status.Exceeded {
		_ = upload.Cancel(r.Context())
		_ = s.opts.Catalog.DeleteUploadSession(r.Context(), session.ID)
		ocierrors.ServeJSON(w, ocierrors.Denied(fmt.Sprintf(
			"storage quota exceeded: this organization uses %d of %d bytes and this layer adds %d",
			status.UsedBytes, derefQuota(status.QuotaBytes), size)), nil)
		return
	}

	if err := s.opts.Storage.Commit(r.Context(), session.ID, expected); err != nil {
		s.serveInternal(w, r, "committing blob to storage", err)
		return
	}

	if _, err := s.opts.Catalog.CommitBlob(r.Context(), repo.ID, expected.String(), size,
		r.Header.Get("Content-Type"), driver.BlobPath(expected)); err != nil {
		s.serveInternal(w, r, "recording committed blob", err)
		return
	}

	if err := s.opts.Catalog.DeleteUploadSession(r.Context(), session.ID); err != nil {
		s.logger(r.Context()).Warn("removing completed upload session", "error", err)
	}
	if s.opts.Metrics != nil && !monolithic {
		s.opts.Metrics.UploadsActive.Dec()
	}
	s.observeBytesIn("upload", written)

	w.Header().Set("Location", fmt.Sprintf("/v2/%s/blobs/%s", repo.Name, expected))
	setContentDigest(w, expected.String())
	w.Header().Set("Content-Length", "0")
	w.WriteHeader(http.StatusCreated)
}

// handleUploadCancel implements DELETE of an upload session.
func (s *Service) handleUploadCancel(w http.ResponseWriter, r *http.Request, p params) {
	_, session, ok := s.resolveSession(w, r, p, authz.ActionPush)
	if !ok {
		return
	}

	upload, err := s.resumeUpload(r, session)
	if err == nil {
		_ = upload.Cancel(r.Context())
	}
	if err := s.opts.Catalog.DeleteUploadSession(r.Context(), session.ID); err != nil {
		s.serveInternal(w, r, "cancelling upload session", err)
		return
	}
	if s.opts.Metrics != nil {
		s.opts.Metrics.UploadsActive.Dec()
	}
	w.Header().Set("Content-Length", "0")
	w.WriteHeader(http.StatusNoContent)
}

// resolveSession authorizes the repository and loads the upload session.
func (s *Service) resolveSession(w http.ResponseWriter, r *http.Request, p params, action authz.Action) (*catalog.Repository, *catalog.UploadSession, bool) {
	repo, ok := s.requireRepository(w, r, p.Name, action)
	if !ok {
		return nil, nil, false
	}

	if _, err := uuid.Parse(p.Session); err != nil {
		ocierrors.ServeJSON(w, ocierrors.UploadUnknown(p.Session), nil)
		return nil, nil, false
	}

	session, err := s.opts.Catalog.UploadSessionByID(r.Context(), p.Session)
	if errors.Is(err, catalog.ErrNotFound) {
		ocierrors.ServeJSON(w, ocierrors.UploadUnknown(p.Session), nil)
		return nil, nil, false
	}
	if err != nil {
		s.serveInternal(w, r, "loading upload session", err)
		return nil, nil, false
	}

	// A session belongs to the repository it was opened against. Without this,
	// a caller with push on one repository could finalise a session opened
	// against another and land the blob there.
	if session.RepositoryID != repo.ID {
		ocierrors.ServeJSON(w, ocierrors.UploadUnknown(p.Session), nil)
		return nil, nil, false
	}

	// The session is bound to the identity that opened it, so token expiry
	// mid-push is survivable but a different principal cannot hijack it (§9.1).
	auth := authFrom(r.Context())
	if auth.Identity == nil || (session.IdentityID != auth.Identity.ID && !auth.Identity.InstanceAdmin) {
		ocierrors.ServeJSON(w, ocierrors.UploadUnknown(p.Session), nil)
		return nil, nil, false
	}

	return repo, session, true
}

// resumeUpload reattaches to a session's staging area.
func (s *Service) resumeUpload(r *http.Request, session *catalog.UploadSession) (driver.Upload, error) {
	if session.ByteOffset == 0 && len(session.HashState) == 0 {
		return s.opts.Storage.NewUpload(r.Context(), session.ID, oci.SHA256)
	}
	return s.opts.Storage.ResumeUpload(r.Context(), session.ID, oci.SHA256, driver.State{
		Offset:     session.ByteOffset,
		HashState:  session.HashState,
		StorageRef: session.StorageRef,
		S3UploadID: session.S3UploadID,
		S3Parts:    session.S3Parts,
	})
}

// errBlobTooLarge reports a blob exceeding the configured maximum (SEC-07).
type errBlobTooLarge struct {
	Limit int64
}

func (e *errBlobTooLarge) Error() string {
	return fmt.Sprintf("blob exceeds the maximum size of %d bytes", e.Limit)
}

// copyChunk streams the request body into the upload, enforcing the size limit
// as it goes.
//
// The limit is applied to the stream rather than to Content-Length, because
// Content-Length is client-supplied and a chunked request has none. Checking
// the header alone would let a chunked upload write until the disk filled.
func (s *Service) copyChunk(r *http.Request, upload driver.Upload, startOffset int64) (int64, error) {
	if r.Body == nil {
		return 0, nil
	}
	limit := s.opts.MaxBlobSize
	if limit <= 0 {
		written, err := io.Copy(upload, r.Body)
		return written, err
	}

	remaining := limit - startOffset
	if remaining < 0 {
		return 0, &errBlobTooLarge{Limit: limit}
	}
	// Read one byte past the limit so that hitting it exactly is not mistaken
	// for exceeding it.
	limited := io.LimitReader(r.Body, remaining+1)
	written, err := io.Copy(upload, limited)
	if err != nil {
		return written, err
	}
	if startOffset+written > limit {
		return written, &errBlobTooLarge{Limit: limit}
	}
	return written, nil
}

// writeUploadAccepted emits the headers common to every in-progress upload
// response.
//
// The Range header is inclusive and zero-indexed (REQ-OCI-07): after 100 bytes
// it reads "0-99". A session holding no bytes reports "0-0", which is what the
// reference implementation does and what clients expect, despite literally
// denoting one byte — the specification's own example, and a wart worth
// matching rather than correcting.
func (s *Service) writeUploadAccepted(w http.ResponseWriter, repoName, sessionID string, offset int64, status int) {
	w.Header().Set("Location", fmt.Sprintf("/v2/%s/blobs/uploads/%s", repoName, sessionID))
	w.Header().Set("Docker-Upload-UUID", sessionID)
	w.Header().Set("Range", formatUploadRange(offset))
	w.Header().Set("Content-Length", "0")
	w.WriteHeader(status)
}

func formatUploadRange(offset int64) string {
	if offset <= 0 {
		return "0-0"
	}
	return fmt.Sprintf("0-%d", offset-1)
}

// writeRangeMismatch answers a misaligned chunk with 416 and the valid range.
func (s *Service) writeRangeMismatch(w http.ResponseWriter, repoName string, session *catalog.UploadSession) {
	w.Header().Set("Location", fmt.Sprintf("/v2/%s/blobs/uploads/%s", repoName, session.ID))
	w.Header().Set("Docker-Upload-UUID", session.ID)
	w.Header().Set("Range", formatUploadRange(session.ByteOffset))
	w.Header().Set("Content-Length", "0")
	w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
}

// parseContentRange parses an upload Content-Range header, which uses the bare
// "start-end" form rather than the "bytes start-end/total" of a range response.
func parseContentRange(value string) (start, end int64, err error) {
	value = strings.TrimSpace(value)
	// Tolerate a "bytes " prefix: it is not correct for upload requests, but
	// some clients send it and rejecting them buys nothing.
	value = strings.TrimPrefix(value, "bytes ")
	if slash := strings.Index(value, "/"); slash >= 0 {
		value = value[:slash]
	}

	startStr, endStr, found := strings.Cut(value, "-")
	if !found {
		return 0, 0, fmt.Errorf("Content-Range %q is malformed: expected start-end", value)
	}
	start, err = strconv.ParseInt(strings.TrimSpace(startStr), 10, 64)
	if err != nil {
		return 0, 0, fmt.Errorf("Content-Range start %q is not an integer", startStr)
	}
	end, err = strconv.ParseInt(strings.TrimSpace(endStr), 10, 64)
	if err != nil {
		return 0, 0, fmt.Errorf("Content-Range end %q is not an integer", endStr)
	}
	if start < 0 || end < start {
		return 0, 0, fmt.Errorf("Content-Range %q describes an empty or negative range", value)
	}
	return start, end, nil
}

func derefQuota(q *int64) int64 {
	if q == nil {
		return 0
	}
	return *q
}

// clientAddress extracts the peer address for the ledger and rate limiting.
//
// X-Forwarded-For is honoured only for its last entry, which is the address the
// immediately-upstream proxy observed. Trusting the first entry would let a
// client forge its own source address by setting the header, which matters
// because the ledger infers host identity from it.
func clientAddress(r *http.Request) string {
	if forwarded := r.Header.Get("X-Forwarded-For"); forwarded != "" {
		parts := strings.Split(forwarded, ",")
		candidate := strings.TrimSpace(parts[len(parts)-1])
		if ip := net.ParseIP(candidate); ip != nil {
			return ip.String()
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
