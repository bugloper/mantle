package catalog

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

// Two column lists for the same columns in the same order: one qualified for
// joins, one bare for RETURNING clauses. scanBlob reads either, since it scans
// positionally.
const (
	blobColumns = `b.id, b.digest, b.size_bytes, coalesce(b.media_type, ''),
		b.storage_ref, b.state, b.created_at`
	blobColumnsBare = `id, digest, size_bytes, coalesce(media_type, ''),
		storage_ref, state, created_at`
)

func scanBlob(row pgx.Row) (*Blob, error) {
	var b Blob
	err := row.Scan(&b.ID, &b.Digest, &b.SizeBytes, &b.MediaType, &b.StorageRef, &b.State, &b.CreatedAt)
	if err != nil {
		return nil, noRows(err)
	}
	return &b, nil
}

// BlobInRepository returns a blob only if the repository is permitted to serve
// it.
//
// The repository_blobs join is the access control, not an optimisation. Blobs
// are globally deduplicated, so without it any caller who can read one
// repository could pull any layer in the instance by digest — the same
// exfiltration hole as an unchecked cross-repository mount (SEC-03).
//
// Quarantined blobs are excluded: an object in quarantine has stopped being
// served, which is what makes the state recoverable rather than merely a label.
func (s *Store) BlobInRepository(ctx context.Context, repoID int64, digest string) (*Blob, error) {
	return scanBlob(s.pool.QueryRow(ctx, `
		SELECT `+blobColumns+`
		FROM blobs b
		JOIN repository_blobs rb ON rb.blob_id = b.id
		WHERE rb.repository_id = $1 AND b.digest = $2 AND b.state = 'available'`,
		repoID, digest))
}

// BlobGlobal looks a blob up by digest across the whole instance, ignoring
// repository scoping. It exists for cross-repository mount and for garbage
// collection, both of which have already made their own authorization
// decisions. It must never back a client-facing read.
func (s *Store) BlobGlobal(ctx context.Context, digest string) (*Blob, error) {
	return scanBlob(s.pool.QueryRow(ctx,
		`SELECT `+blobColumns+` FROM blobs b WHERE b.digest = $1 AND b.state = 'available'`, digest))
}

// CommitBlob records a completed upload and links it to the repository.
//
// Concurrent pushes of the same layer are routine — two CI jobs building from
// the same base image will race — so an existing row is not an error. Content
// addressing makes the outcome identical either way: whoever inserted it stored
// the same bytes.
func (s *Store) CommitBlob(ctx context.Context, repoID int64, digest string, size int64, mediaType, storageRef string) (*Blob, error) {
	var blob *Blob
	err := s.tx(ctx, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			INSERT INTO blobs (digest, size_bytes, media_type, storage_ref)
			VALUES ($1, $2, nullif($3, ''), $4)
			ON CONFLICT (digest) DO UPDATE
			  SET state = CASE
			        -- A re-push of a quarantined blob revives it. The bytes are
			        -- still there and someone evidently wants them; making them
			        -- available again is both correct and what GC's
			        -- unquarantine phase would do on its next cycle anyway.
			        WHEN blobs.state = 'quarantined' THEN 'available'
			        ELSE blobs.state END,
			      quarantined_at = CASE
			        WHEN blobs.state = 'quarantined' THEN NULL
			        ELSE blobs.quarantined_at END
			RETURNING `+blobColumnsBare,
			digest, size, mediaType, storageRef)
		var err error
		blob, err = scanBlob(row)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `
			INSERT INTO repository_blobs (repository_id, blob_id)
			VALUES ($1, $2) ON CONFLICT DO NOTHING`, repoID, blob.ID)
		return err
	})
	if err != nil {
		return nil, err
	}
	return blob, nil
}

// LinkBlob makes an existing blob servable from another repository, which is
// what a cross-repository mount does once authorization has passed.
func (s *Store) LinkBlob(ctx context.Context, repoID, blobID int64) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO repository_blobs (repository_id, blob_id)
		VALUES ($1, $2) ON CONFLICT DO NOTHING`, repoID, blobID)
	return err
}

// UnlinkBlob removes a blob from a repository without deleting it. This is what
// DELETE /v2/<name>/blobs/<digest> does: the blob stops being servable from
// this repository, and garbage collection reclaims the bytes later if nothing
// else references them (end-10, §12.3).
func (s *Store) UnlinkBlob(ctx context.Context, repoID int64, digest string) error {
	tag, err := s.pool.Exec(ctx, `
		DELETE FROM repository_blobs
		WHERE repository_id = $1
		  AND blob_id = (SELECT id FROM blobs WHERE digest = $2)`, repoID, digest)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// RepositoryUsage reports the bytes attributable to a repository: the distinct
// blobs it may serve plus its manifest payloads.
func (s *Store) RepositoryUsage(ctx context.Context, repoID int64) (int64, error) {
	var usage int64
	err := s.pool.QueryRow(ctx, `
		SELECT coalesce((SELECT sum(b.size_bytes)
		                 FROM blobs b JOIN repository_blobs rb ON rb.blob_id = b.id
		                 WHERE rb.repository_id = $1), 0)
		     + coalesce((SELECT sum(m.size_bytes) FROM manifests m WHERE m.repository_id = $1), 0)`,
		repoID).Scan(&usage)
	return usage, err
}

// OrganizationUsage reports an organization's storage usage (§15.3).
//
// The accounting rule is the sum of the sizes of distinct blobs referenced by
// the organization's repositories, plus manifest bytes. A layer shared between
// two repositories in the same organization is counted once; a layer shared
// across two organizations is counted once in each. Attributing shared layers
// fractionally would be unimplementable in a way anyone could reason about, and
// undercounting invites abuse — so the rule is documented rather than clever.
func (s *Store) OrganizationUsage(ctx context.Context, orgID int64) (int64, error) {
	var usage int64
	err := s.pool.QueryRow(ctx, `
		SELECT coalesce((SELECT sum(b.size_bytes) FROM blobs b
		                 WHERE b.id IN (SELECT rb.blob_id
		                                FROM repository_blobs rb
		                                JOIN repositories r ON r.id = rb.repository_id
		                                WHERE r.organization_id = $1)), 0)
		     + coalesce((SELECT sum(m.size_bytes) FROM manifests m
		                 JOIN repositories r ON r.id = m.repository_id
		                 WHERE r.organization_id = $1), 0)`,
		orgID).Scan(&usage)
	return usage, err
}

// QuotaStatus describes an organization's quota position.
type QuotaStatus struct {
	UsedBytes  int64
	QuotaBytes *int64
	// Exceeded reports that a write should be refused. Reads are never blocked
	// by quota: a full quota must not take production down (§15.3).
	Exceeded bool
	// SoftExceeded reports crossing the 80% warning threshold.
	SoftExceeded bool
}

// SoftQuotaThreshold is the fraction at which a warning is raised.
const SoftQuotaThreshold = 0.8

// CheckQuota reports whether an organization may accept more data.
func (s *Store) CheckQuota(ctx context.Context, orgID int64, incoming int64) (*QuotaStatus, error) {
	var quota *int64
	if err := s.pool.QueryRow(ctx,
		`SELECT quota_bytes FROM organizations WHERE id = $1`, orgID).Scan(&quota); err != nil {
		return nil, noRows(err)
	}
	used, err := s.OrganizationUsage(ctx, orgID)
	if err != nil {
		return nil, err
	}
	status := &QuotaStatus{UsedBytes: used, QuotaBytes: quota}
	if quota == nil {
		return status, nil
	}
	status.Exceeded = used+incoming > *quota
	status.SoftExceeded = float64(used) >= float64(*quota)*SoftQuotaThreshold
	return status, nil
}

// --- upload sessions (§11.2) ---

// UploadSession is a resumable upload's durable state.
type UploadSession struct {
	ID           string
	RepositoryID int64
	IdentityID   int64
	ByteOffset   int64
	HashState    []byte
	StorageRef   string
	S3UploadID   string
	S3Parts      []byte
	ExpiresAt    time.Time
}

// CreateUploadSession opens a new upload session.
func (s *Store) CreateUploadSession(ctx context.Context, repoID, identityID int64, storageRef string, ttl time.Duration) (*UploadSession, error) {
	session := &UploadSession{}
	err := s.pool.QueryRow(ctx, `
		INSERT INTO upload_sessions (repository_id, identity_id, storage_ref, expires_at)
		VALUES ($1, $2, $3, now() + $4::interval)
		RETURNING id::text, repository_id, identity_id, byte_offset, hash_state,
		          storage_ref, coalesce(s3_upload_id, ''), s3_parts, expires_at`,
		repoID, identityID, storageRef, ttl.String()).Scan(
		&session.ID, &session.RepositoryID, &session.IdentityID, &session.ByteOffset,
		&session.HashState, &session.StorageRef, &session.S3UploadID, &session.S3Parts,
		&session.ExpiresAt)
	if err != nil {
		return nil, err
	}
	return session, nil
}

// UploadSessionByID loads a session, treating an expired one as absent so a
// client receives BLOB_UPLOAD_UNKNOWN rather than resuming into a session whose
// staging the janitor may already have discarded.
func (s *Store) UploadSessionByID(ctx context.Context, id string) (*UploadSession, error) {
	session := &UploadSession{}
	err := s.pool.QueryRow(ctx, `
		SELECT id::text, repository_id, identity_id, byte_offset, hash_state,
		       storage_ref, coalesce(s3_upload_id, ''), s3_parts, expires_at
		FROM upload_sessions
		WHERE id = $1 AND expires_at > now()`, id).Scan(
		&session.ID, &session.RepositoryID, &session.IdentityID, &session.ByteOffset,
		&session.HashState, &session.StorageRef, &session.S3UploadID, &session.S3Parts,
		&session.ExpiresAt)
	if err != nil {
		return nil, noRows(err)
	}
	return session, nil
}

// UpdateUploadSession checkpoints progress.
//
// The update is conditional on the offset not having moved: two nodes writing
// to one session concurrently is a client error, but it must not silently
// produce a session whose recorded offset and hash state come from different
// writers. A conflict here surfaces as BLOB_UPLOAD_INVALID.
func (s *Store) UpdateUploadSession(ctx context.Context, id string, previousOffset, offset int64, hashState []byte, s3UploadID string, s3Parts []byte) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE upload_sessions
		SET byte_offset = $3, hash_state = $4,
		    s3_upload_id = nullif($5, ''),
		    s3_parts = coalesce($6, s3_parts),
		    updated_at = now()
		WHERE id = $1 AND byte_offset = $2`,
		id, previousOffset, offset, hashState, s3UploadID, s3Parts)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteUploadSession removes a session row.
func (s *Store) DeleteUploadSession(ctx context.Context, id string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM upload_sessions WHERE id = $1`, id)
	return err
}

// ExpiredUploadSessions returns sessions past their expiry, for the janitor.
func (s *Store) ExpiredUploadSessions(ctx context.Context, limit int) ([]*UploadSession, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id::text, repository_id, identity_id, byte_offset, hash_state,
		       storage_ref, coalesce(s3_upload_id, ''), s3_parts, expires_at
		FROM upload_sessions
		WHERE expires_at <= now()
		LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var sessions []*UploadSession
	for rows.Next() {
		session := &UploadSession{}
		if err := rows.Scan(&session.ID, &session.RepositoryID, &session.IdentityID,
			&session.ByteOffset, &session.HashState, &session.StorageRef,
			&session.S3UploadID, &session.S3Parts, &session.ExpiresAt); err != nil {
			return nil, err
		}
		sessions = append(sessions, session)
	}
	return sessions, rows.Err()
}

// CountActiveUploads reports how many sessions an identity currently holds,
// bounding concurrent upload fan-out per credential (SEC-07).
func (s *Store) CountActiveUploads(ctx context.Context, identityID int64) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM upload_sessions WHERE identity_id = $1 AND expires_at > now()`,
		identityID).Scan(&n)
	return n, err
}
