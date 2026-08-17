// Package driver defines the blob storage contract (§10.6).
//
// The interface is deliberately small, and deliberately knows nothing about
// repositories, tags, or permissions. It maps a digest to bytes. Everything
// semantic lives in Postgres, which is what makes online garbage collection
// tractable, makes deduplication global and automatic, and keeps the escape
// path in §18.3 honest — the blobs on disk are ordinary content-addressed
// files that any OCI tool can be pointed at.
package driver

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/mantle-sh/mantle/internal/oci"
)

// ErrNotFound is returned when a digest is absent from the store. Callers
// distinguish it from an I/O failure: a missing blob is a 404, an unreadable
// one is a 500, and conflating them turns a disk fault into a silent data-loss
// report.
var ErrNotFound = errors.New("blob not found in storage")

// ErrSessionNotFound is returned for an unknown or expired upload session.
var ErrSessionNotFound = errors.New("upload session not found in storage")

// Info describes a stored blob.
type Info struct {
	Digest    oci.Digest
	Size      int64
	CreatedAt time.Time
}

// State is the durable checkpoint of an in-progress upload (§10.4).
//
// Every field here must be durable at the moment it is returned. Recording an
// offset whose bytes are not yet on stable storage is worse than resuming from
// further back: on resume the client would continue from a position the store
// cannot actually produce, and the resulting blob would be corrupt but
// digest-verified against a hash that also skipped the missing bytes.
type State struct {
	// Offset is the number of bytes durably accepted.
	Offset int64
	// HashState is the checkpointed digest state, as produced by
	// oci.Verifier.MarshalState.
	HashState []byte
	// StorageRef locates the in-progress data for the driver that wrote it.
	StorageRef string
	// S3UploadID and S3Parts carry multipart bookkeeping. Empty for filesystem.
	S3UploadID string
	S3Parts    []byte
}

// Upload is an append-only staging area for one blob.
//
// Writes must be sequential. The distribution layer enforces the OCI Range
// semantics (REQ-OCI-07) before calling Write, so a driver may treat an
// out-of-order write as a programming error rather than a client error.
type Upload interface {
	io.Writer

	// Offset reports how many bytes have been written, including bytes not yet
	// durable. Checkpoint reports the durable prefix, which may be smaller.
	Offset() int64

	// Digest returns the digest of everything written so far.
	Digest() oci.Digest

	// Checkpoint flushes to durable storage and returns resumable state.
	Checkpoint(ctx context.Context) (State, error)

	// Cancel discards the upload and releases its resources.
	Cancel(ctx context.Context) error

	// Close releases in-memory resources without discarding the staged data,
	// so another node can resume the session.
	Close() error
}

// Driver is a blob store.
type Driver interface {
	// Name identifies the driver in logs, metrics, and doctor output.
	Name() string

	// Stat reports a blob's presence and size.
	Stat(ctx context.Context, digest oci.Digest) (Info, error)

	// Open returns a seekable reader. Seekability is required: range requests
	// are how a container runtime resumes an interrupted layer pull, and
	// serving them by reading and discarding would make a 4 GiB layer resume
	// cost a 4 GiB read.
	Open(ctx context.Context, digest oci.Digest) (io.ReadSeekCloser, error)

	// PresignGet returns a URL the client may fetch directly. The boolean
	// reports whether presigning is supported at all; a driver that cannot
	// presign returns false and the caller proxies the bytes instead (§7.3).
	PresignGet(ctx context.Context, digest oci.Digest, ttl time.Duration) (string, bool, error)

	// NewUpload begins a staging area for a session.
	NewUpload(ctx context.Context, sessionID string, algorithm oci.Algorithm) (Upload, error)

	// ResumeUpload reattaches to an existing session from checkpointed state.
	ResumeUpload(ctx context.Context, sessionID string, algorithm oci.Algorithm, state State) (Upload, error)

	// Commit atomically moves a completed session's bytes to the digest's
	// permanent location. The digest must already have been verified by the
	// caller; the driver does not re-verify.
	Commit(ctx context.Context, sessionID string, digest oci.Digest) error

	// Delete removes a blob's bytes. Deleting an absent blob is not an error —
	// garbage collection retries, and a sweep that already succeeded must not
	// fail the second time.
	Delete(ctx context.Context, digest oci.Digest) error

	// AbortStale discards upload staging older than the cutoff and returns how
	// many were removed. For S3 this also aborts multipart uploads with no
	// corresponding session, which otherwise accrue storage cost invisibly
	// (§10.5).
	AbortStale(ctx context.Context, before time.Time) (int, error)

	// Usage reports total and available bytes where the driver can determine
	// them, for preflight and doctor. ok is false for stores with no
	// meaningful capacity, such as object storage.
	Usage(ctx context.Context) (usage Usage, ok bool, err error)

	// Close releases driver resources.
	Close() error
}

// Usage is a capacity report for a storage backend.
type Usage struct {
	TotalBytes     int64
	AvailableBytes int64
}

// BlobPath returns the canonical content-addressed key for a digest, shared by
// every driver so that a filesystem store and an S3 store lay content out
// identically (§10.2).
//
// The path derives only from the digest — never from a repository name. That is
// the structural defence against SEC-01: there is no string a client controls
// anywhere in this function, so no repository name can construct a path.
func BlobPath(d oci.Digest) string {
	if !d.Valid() {
		// Unreachable through the HTTP surface, which parses digests before
		// they reach storage. Panicking beats silently returning "blobs//".
		panic("driver.BlobPath: called with an unvalidated digest")
	}
	encoded := d.Encoded()
	return fmt.Sprintf("blobs/%s/%s/%s", d.Algorithm(), encoded[:2], encoded)
}

// UploadPath returns the staging directory for a session.
func UploadPath(sessionID string) string {
	return "uploads/" + sessionID
}
