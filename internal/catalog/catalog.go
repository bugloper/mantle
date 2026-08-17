// Package catalog is Mantle's metadata layer: repositories, blobs, manifests,
// tags, and upload sessions in PostgreSQL.
//
// Everything semantic about the registry lives here. The storage driver knows
// only how to turn a digest into bytes; this package knows which repository may
// serve that blob, which manifest references it, and which tag points at that
// manifest. Keeping the split clean is what makes online garbage collection
// possible: liveness is a query, not a filesystem walk.
package catalog

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Sentinel errors. Handlers map these onto OCI error codes, so the set is kept
// small and meaningful rather than mirroring every SQL failure.
var (
	ErrNotFound        = errors.New("not found")
	ErrAlreadyExists   = errors.New("already exists")
	ErrImmutable       = errors.New("tag is immutable")
	ErrQuotaExceeded   = errors.New("storage quota exceeded")
	ErrStillReferenced = errors.New("object is still referenced")
)

// ErrMissingBlobs reports a manifest whose referenced blobs are not all present
// in the target repository (REQ-OCI-05). The digests are carried so the client
// receives MANIFEST_BLOB_UNKNOWN naming the blob it needs to push.
type ErrMissingBlobs struct {
	Digests []string
}

func (e *ErrMissingBlobs) Error() string {
	if len(e.Digests) == 1 {
		return fmt.Sprintf("manifest references unknown blob %s", e.Digests[0])
	}
	return fmt.Sprintf("manifest references %d unknown blobs, first is %s", len(e.Digests), e.Digests[0])
}

// Store is the metadata layer.
type Store struct {
	pool *pgxpool.Pool
}

// New wraps a connection pool.
func New(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// Pool exposes the underlying pool for packages that own their own schema
// (ledger, audit, gc) and for health checks. It is not a licence for the
// distribution layer to write ad-hoc SQL.
func (s *Store) Pool() *pgxpool.Pool { return s.pool }

// State is the lifecycle state shared by blobs and manifests (§12.1).
//
// Quarantine is the whole reason deletion is survivable: an object in this
// state stops being served but its bytes remain, so a mistake is recoverable by
// flipping a column for as long as the quarantine window lasts.
type State string

const (
	StateAvailable   State = "available"
	StateQuarantined State = "quarantined"
	StateDeleting    State = "deleting"
)

// Repository is a named collection of manifests and tags.
type Repository struct {
	ID             int64
	UUID           string
	OrganizationID int64
	Name           string
	Visibility     string
	ImmutableTags  bool
	QuotaBytes     *int64
	SourceURL      string
	CreatedAt      time.Time
}

// IsPublic reports whether anonymous readers may pull from this repository.
func (r *Repository) IsPublic() bool { return r.Visibility == "public" }

// Blob is a content-addressed object's metadata.
type Blob struct {
	ID         int64
	Digest     string
	SizeBytes  int64
	MediaType  string
	StorageRef string
	State      State
	CreatedAt  time.Time
}

// Manifest is a stored manifest, including its exact original bytes.
type Manifest struct {
	ID            int64
	RepositoryID  int64
	Digest        string
	MediaType     string
	ArtifactType  string
	SubjectDigest string
	SizeBytes     int
	// Payload is byte-identical to what was received (REQ-OCI-02). It is never
	// re-encoded, because the digest is over these exact bytes.
	Payload      []byte
	ConfigDigest string
	Pinned       bool
	State        State
	CreatedAt    time.Time
}

// Tag is a mutable name pointing at a manifest.
type Tag struct {
	ID             int64
	RepositoryID   int64
	Name           string
	ManifestID     int64
	ManifestDigest string
	Protected      bool
	UpdatedAt      time.Time
}

// tx runs fn inside a transaction, rolling back on error.
//
// Every multi-statement write in this package goes through here. The manifest
// write in particular must be atomic with its blob-existence check, or a
// concurrent garbage collection could remove a blob between the check and the
// insert (REQ-OCI-05, §12.1 mechanism 2).
func (s *Store) tx(ctx context.Context, fn func(pgx.Tx) error) error {
	return pgx.BeginFunc(ctx, s.pool, fn)
}

// isUniqueViolation reports whether an error is a duplicate-key failure, which
// concurrent pushes of the same content produce routinely and which is usually
// a signal to re-read rather than to fail.
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// isForeignKeyViolation reports whether an error is a referential-integrity
// failure. On the GC path this means an object became live mid-sweep, which is
// the ON DELETE RESTRICT safety net doing its job (§11.1).
func isForeignKeyViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23503"
}

// noRows normalises pgx.ErrNoRows onto ErrNotFound so callers match one thing.
func noRows(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	return err
}
