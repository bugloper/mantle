package filesystem_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/mantle-sh/mantle/internal/oci"
	"github.com/mantle-sh/mantle/internal/storage/driver"
	"github.com/mantle-sh/mantle/internal/storage/filesystem"
)

func newDriver(t *testing.T) *filesystem.Driver {
	t.Helper()
	d, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatalf("creating driver: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

func TestUploadCommitAndRead(t *testing.T) {
	ctx := context.Background()
	d := newDriver(t)
	payload := []byte("the registry that knows what you deployed")
	want := oci.FromBytes(payload)

	session := uuid.NewString()
	up, err := d.NewUpload(ctx, session, oci.SHA256)
	if err != nil {
		t.Fatalf("new upload: %v", err)
	}
	if _, err := up.Write(payload); err != nil {
		t.Fatalf("write: %v", err)
	}
	if got := up.Offset(); got != int64(len(payload)) {
		t.Errorf("offset = %d, want %d", got, len(payload))
	}
	if got := up.Digest(); !got.Equal(want) {
		t.Errorf("digest = %s, want %s", got, want)
	}
	if _, err := up.Checkpoint(ctx); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	if err := up.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := d.Commit(ctx, session, want); err != nil {
		t.Fatalf("commit: %v", err)
	}

	info, err := d.Stat(ctx, want)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Size != int64(len(payload)) {
		t.Errorf("stat size = %d, want %d", info.Size, len(payload))
	}

	r, err := d.Open(ctx, want)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer r.Close()
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("read back %q, want %q", got, payload)
	}

	// Committing must clear the staging directory, or every push leaks a
	// directory until the janitor runs.
	if _, err := os.Stat(filepath.Join(d.Root(), "uploads", session)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("upload session directory survived commit: %v", err)
	}
}

// Resuming on another node is the case §10.4 exists for: the checkpointed hash
// and the staged bytes must combine into the same digest a single-node push
// would have produced.
func TestResumeAcrossDriverInstances(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	payload := make([]byte, 512*1024)
	if _, err := rand.Read(payload); err != nil {
		t.Fatal(err)
	}
	want := oci.FromBytes(payload)
	session := uuid.NewString()
	const split = 300 * 1024

	first, err := filesystem.New(root)
	if err != nil {
		t.Fatal(err)
	}
	up, err := first.NewUpload(ctx, session, oci.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := up.Write(payload[:split]); err != nil {
		t.Fatal(err)
	}
	state, err := up.Checkpoint(ctx)
	if err != nil {
		t.Fatal(err)
	}
	up.Close()
	first.Close()

	// A different driver instance, as a different process would see it.
	second, err := filesystem.New(root)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()

	resumed, err := second.ResumeUpload(ctx, session, oci.SHA256, state)
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if resumed.Offset() != split {
		t.Errorf("resumed offset = %d, want %d", resumed.Offset(), split)
	}
	if _, err := resumed.Write(payload[split:]); err != nil {
		t.Fatal(err)
	}
	if got := resumed.Digest(); !got.Equal(want) {
		t.Fatalf("digest after resume = %s, want %s", got, want)
	}
	if _, err := resumed.Checkpoint(ctx); err != nil {
		t.Fatal(err)
	}
	resumed.Close()

	if err := second.Commit(ctx, session, want); err != nil {
		t.Fatalf("commit: %v", err)
	}
	r, err := second.Open(ctx, want)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Error("committed blob does not match the original payload")
	}
}

// If the process died between a write and its fsync, the staging file can hold
// more bytes than the checkpoint covers. Resuming must truncate back, or the
// resumed hash would be computed over a different byte sequence than the file
// contains — producing a blob that passes digest verification and is wrong.
func TestResumeTruncatesUncheckpointedTail(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	d, err := filesystem.New(root)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	session := uuid.NewString()
	up, err := d.NewUpload(ctx, session, oci.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := up.Write([]byte("durable")); err != nil {
		t.Fatal(err)
	}
	state, err := up.Checkpoint(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Bytes written after the checkpoint, as a crash would leave them.
	if _, err := up.Write([]byte("lost-tail")); err != nil {
		t.Fatal(err)
	}
	up.Close()

	resumed, err := d.ResumeUpload(ctx, session, oci.SHA256, state)
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if resumed.Offset() != int64(len("durable")) {
		t.Fatalf("resumed offset = %d, want %d", resumed.Offset(), len("durable"))
	}
	if _, err := resumed.Write([]byte(" continuation")); err != nil {
		t.Fatal(err)
	}
	resumed.Checkpoint(ctx)
	resumed.Close()

	want := oci.FromBytes([]byte("durable continuation"))
	if got := resumed.Digest(); !got.Equal(want) {
		t.Errorf("digest = %s, want %s: the uncheckpointed tail was not discarded", got, want)
	}
	if err := d.Commit(ctx, session, want); err != nil {
		t.Fatal(err)
	}
	r, _ := d.Open(ctx, want)
	defer r.Close()
	got, _ := io.ReadAll(r)
	if string(got) != "durable continuation" {
		t.Errorf("stored bytes = %q, want %q", got, "durable continuation")
	}
}

func TestStatAndOpenReportNotFound(t *testing.T) {
	ctx := context.Background()
	d := newDriver(t)
	missing := oci.FromBytes([]byte("never stored"))

	if _, err := d.Stat(ctx, missing); !errors.Is(err, driver.ErrNotFound) {
		t.Errorf("Stat error = %v, want ErrNotFound", err)
	}
	if _, err := d.Open(ctx, missing); !errors.Is(err, driver.ErrNotFound) {
		t.Errorf("Open error = %v, want ErrNotFound", err)
	}
}

// GC retries a failed sweep, so deleting an already-deleted blob must succeed.
func TestDeleteIsIdempotent(t *testing.T) {
	ctx := context.Background()
	d := newDriver(t)
	digest := oci.FromBytes([]byte("gone"))
	if err := d.Delete(ctx, digest); err != nil {
		t.Errorf("deleting an absent blob returned %v, want nil", err)
	}
}

func TestAbortStaleRemovesOldSessions(t *testing.T) {
	ctx := context.Background()
	d := newDriver(t)

	fresh := uuid.NewString()
	if _, err := d.NewUpload(ctx, fresh, oci.SHA256); err != nil {
		t.Fatal(err)
	}
	stale := uuid.NewString()
	if _, err := d.NewUpload(ctx, stale, oci.SHA256); err != nil {
		t.Fatal(err)
	}
	staleDir := filepath.Join(d.Root(), "uploads", stale)
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(staleDir, old, old); err != nil {
		t.Fatal(err)
	}

	n, err := d.AbortStale(ctx, time.Now().Add(-24*time.Hour))
	if err != nil {
		t.Fatalf("abort stale: %v", err)
	}
	if n != 1 {
		t.Errorf("removed %d sessions, want 1", n)
	}
	if _, err := os.Stat(staleDir); !errors.Is(err, os.ErrNotExist) {
		t.Error("stale session directory survived")
	}
	if _, err := os.Stat(filepath.Join(d.Root(), "uploads", fresh)); err != nil {
		t.Errorf("fresh session was removed: %v", err)
	}
}

// The storage path must derive from the digest alone, so that no repository
// name can influence it (SEC-01).
func TestBlobPathDerivesFromDigestOnly(t *testing.T) {
	digest := oci.MustParseDigest(
		"sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")
	got := driver.BlobPath(digest)
	want := "blobs/sha256/01/0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	if got != want {
		t.Errorf("BlobPath = %q, want %q", got, want)
	}
}

func TestSessionIDMustBeUUID(t *testing.T) {
	ctx := context.Background()
	d := newDriver(t)
	if _, err := d.NewUpload(ctx, "../../etc/passwd", oci.SHA256); err == nil {
		t.Error("a non-UUID session id was accepted")
	}
}

func TestWalkBlobs(t *testing.T) {
	ctx := context.Background()
	d := newDriver(t)

	want := map[string]int64{}
	for _, content := range []string{"one", "two", "three"} {
		digest := oci.FromBytes([]byte(content))
		session := uuid.NewString()
		up, err := d.NewUpload(ctx, session, oci.SHA256)
		if err != nil {
			t.Fatal(err)
		}
		up.Write([]byte(content))
		up.Checkpoint(ctx)
		up.Close()
		if err := d.Commit(ctx, session, digest); err != nil {
			t.Fatal(err)
		}
		want[digest.String()] = int64(len(content))
	}

	got := map[string]int64{}
	if err := d.WalkBlobs(ctx, func(digest oci.Digest, size int64) error {
		got[digest.String()] = size
		return nil
	}); err != nil {
		t.Fatalf("walk: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("walk found %d blobs, want %d", len(got), len(want))
	}
	for digest, size := range want {
		if got[digest] != size {
			t.Errorf("blob %s: walk reported size %d, want %d", digest, got[digest], size)
		}
	}
}

func TestUsageReportsCapacity(t *testing.T) {
	d := newDriver(t)
	usage, ok, err := d.Usage(context.Background())
	if err != nil {
		t.Fatalf("usage: %v", err)
	}
	if !ok {
		t.Skip("capacity reporting is unavailable on this platform")
	}
	if usage.TotalBytes <= 0 || usage.AvailableBytes <= 0 {
		t.Errorf("usage = %+v, want positive values", usage)
	}
	if usage.AvailableBytes > usage.TotalBytes {
		t.Errorf("available (%d) exceeds total (%d)", usage.AvailableBytes, usage.TotalBytes)
	}
}
