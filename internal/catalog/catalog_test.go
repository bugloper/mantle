package catalog_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mantle-sh/mantle/internal/catalog"
	"github.com/mantle-sh/mantle/internal/oci"
	"github.com/mantle-sh/mantle/internal/testsupport"
)

type fixture struct {
	store  *catalog.Store
	pool   *pgxpool.Pool
	orgID  int64
	repoID int64
	ctx    context.Context
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	pool := testsupport.NewDB(t)
	orgID, repoID := testsupport.OrgAndRepo(t, pool, "acme", "acme/web")
	return &fixture{
		store:  catalog.New(pool),
		pool:   pool,
		orgID:  orgID,
		repoID: repoID,
		ctx:    context.Background(),
	}
}

// putBlob stores a blob and links it to the repository, returning its digest.
func (f *fixture) putBlob(t *testing.T, content string) string {
	t.Helper()
	digest := oci.FromBytes([]byte(content)).String()
	_, err := f.store.CommitBlob(f.ctx, f.repoID, digest, int64(len(content)),
		"application/vnd.oci.image.layer.v1.tar+gzip", "ref/"+digest)
	if err != nil {
		t.Fatalf("committing blob: %v", err)
	}
	return digest
}

// imageManifest builds a minimal but valid image manifest over the given
// config and layer digests.
func imageManifest(t *testing.T, configDigest string, layerDigests ...string) ([]byte, *oci.Manifest) {
	t.Helper()
	m := map[string]any{
		"schemaVersion": 2,
		"mediaType":     oci.MediaTypeOCIManifest,
		"config": map[string]any{
			"mediaType": oci.MediaTypeOCIImageConfig,
			"digest":    configDigest,
			"size":      100,
		},
		"layers": []map[string]any{},
	}
	layers := make([]map[string]any, 0, len(layerDigests))
	for _, d := range layerDigests {
		layers = append(layers, map[string]any{
			"mediaType": "application/vnd.oci.image.layer.v1.tar+gzip",
			"digest":    d,
			"size":      100,
		})
	}
	m["layers"] = layers

	payload, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := oci.ParseManifest(payload, oci.MediaTypeOCIManifest, oci.DefaultLimits)
	if err != nil {
		t.Fatalf("building test manifest: %v", err)
	}
	return payload, parsed
}

func TestRepositoryLifecycle(t *testing.T) {
	f := newFixture(t)

	repo, err := f.store.RepositoryByName(f.ctx, "acme/web")
	if err != nil {
		t.Fatalf("looking up repository: %v", err)
	}
	if repo.Visibility != "private" {
		t.Errorf("new repository visibility = %q, want private: private by default is the safe default", repo.Visibility)
	}

	if _, err := f.store.RepositoryByName(f.ctx, "acme/absent"); !errors.Is(err, catalog.ErrNotFound) {
		t.Errorf("missing repository error = %v, want ErrNotFound", err)
	}

	// Auto-creation on push, which is what makes `docker push` work with no
	// setup step.
	created, err := f.store.EnsureRepository(f.ctx, "acme/api", "acme", nil)
	if err != nil {
		t.Fatalf("ensuring repository: %v", err)
	}
	if created.Name != "acme/api" {
		t.Errorf("created repository name = %q", created.Name)
	}
	again, err := f.store.EnsureRepository(f.ctx, "acme/api", "acme", nil)
	if err != nil {
		t.Fatalf("re-ensuring repository: %v", err)
	}
	if again.ID != created.ID {
		t.Error("EnsureRepository created a second repository for the same name")
	}

	// A push into an organization that does not exist must say so, rather than
	// silently inventing one.
	if _, err := f.store.EnsureRepository(f.ctx, "ghost/thing", "ghost", nil); err == nil {
		t.Error("pushing into a nonexistent organization succeeded")
	}
}

// REQ-OCI-05: a manifest referencing a blob that is not present must be
// refused, naming the missing digest.
func TestPutManifestRejectsMissingBlobs(t *testing.T) {
	f := newFixture(t)

	configDigest := oci.FromBytes([]byte("config")).String()
	missingLayer := oci.FromBytes([]byte("never uploaded")).String()
	payload, parsed := imageManifest(t, configDigest, missingLayer)

	_, err := f.store.PutManifest(f.ctx, catalog.PutManifestParams{
		RepositoryID: f.repoID,
		Digest:       oci.FromBytes(payload).String(),
		Tag:          "v1",
		MediaType:    oci.MediaTypeOCIManifest,
		Payload:      payload,
		Parsed:       parsed,
	})
	var missing *catalog.ErrMissingBlobs
	if !errors.As(err, &missing) {
		t.Fatalf("error = %v, want ErrMissingBlobs", err)
	}
	if len(missing.Digests) != 2 {
		t.Errorf("reported %d missing digests, want 2 (config and layer)", len(missing.Digests))
	}

	// Nothing may have been written.
	if _, err := f.store.ManifestByTag(f.ctx, f.repoID, "v1"); !errors.Is(err, catalog.ErrNotFound) {
		t.Error("a rejected manifest left a tag behind; the write was not atomic")
	}
}

func TestPutAndGetManifest(t *testing.T) {
	f := newFixture(t)

	configDigest := f.putBlob(t, "config")
	layerDigest := f.putBlob(t, "layer one")
	payload, parsed := imageManifest(t, configDigest, layerDigest)
	digest := oci.FromBytes(payload).String()

	result, err := f.store.PutManifest(f.ctx, catalog.PutManifestParams{
		RepositoryID: f.repoID,
		Digest:       digest,
		Tag:          "v1.0.0",
		MediaType:    oci.MediaTypeOCIManifest,
		Payload:      payload,
		Parsed:       parsed,
	})
	if err != nil {
		t.Fatalf("putting manifest: %v", err)
	}
	if result.TagMoved {
		t.Error("a new tag was reported as moved")
	}

	// REQ-OCI-02: the payload must come back byte-for-byte.
	got, err := f.store.ManifestByTag(f.ctx, f.repoID, "v1.0.0")
	if err != nil {
		t.Fatalf("getting manifest by tag: %v", err)
	}
	if string(got.Payload) != string(payload) {
		t.Error("manifest payload was not returned byte-for-byte")
	}
	if oci.FromBytes(got.Payload).String() != digest {
		t.Error("stored payload no longer hashes to its digest")
	}

	byDigest, err := f.store.ManifestByDigest(f.ctx, f.repoID, digest)
	if err != nil {
		t.Fatalf("getting manifest by digest: %v", err)
	}
	if byDigest.ID != got.ID {
		t.Error("tag and digest resolved to different manifests")
	}

	// A repeated push of identical content is idempotent, because clients retry.
	if _, err := f.store.PutManifest(f.ctx, catalog.PutManifestParams{
		RepositoryID: f.repoID, Digest: digest, Tag: "v1.0.0",
		MediaType: oci.MediaTypeOCIManifest, Payload: payload, Parsed: parsed,
	}); err != nil {
		t.Errorf("re-pushing identical content failed: %v", err)
	}
}

func TestTagMoveIsDetectedAndRecorded(t *testing.T) {
	f := newFixture(t)

	configA := f.putBlob(t, "config a")
	layerA := f.putBlob(t, "layer a")
	payloadA, parsedA := imageManifest(t, configA, layerA)
	digestA := oci.FromBytes(payloadA).String()

	configB := f.putBlob(t, "config b")
	layerB := f.putBlob(t, "layer b")
	payloadB, parsedB := imageManifest(t, configB, layerB)
	digestB := oci.FromBytes(payloadB).String()

	put := func(digest string, payload []byte, parsed *oci.Manifest) *catalog.PutManifestResult {
		t.Helper()
		r, err := f.store.PutManifest(f.ctx, catalog.PutManifestParams{
			RepositoryID: f.repoID, Digest: digest, Tag: "latest",
			MediaType: oci.MediaTypeOCIManifest, Payload: payload, Parsed: parsed,
		})
		if err != nil {
			t.Fatalf("putting manifest: %v", err)
		}
		return r
	}

	put(digestA, payloadA, parsedA)
	moved := put(digestB, payloadB, parsedB)
	if !moved.TagMoved {
		t.Error("repointing 'latest' at new content was not reported as a move")
	}
	if moved.PreviousDigest != digestA {
		t.Errorf("previous digest = %s, want %s", moved.PreviousDigest, digestA)
	}

	history, err := f.store.TagHistory(f.ctx, f.repoID, "latest", 10)
	if err != nil {
		t.Fatalf("reading tag history: %v", err)
	}
	if len(history) != 2 {
		t.Fatalf("tag history has %d entries, want 2", len(history))
	}
	if history[0].Action != "moved" || history[1].Action != "set" {
		t.Errorf("history actions = [%s %s], want [moved set]", history[0].Action, history[1].Action)
	}
}

// §15.2: an immutable tag cannot be repointed, but re-pushing identical content
// under it must still succeed — otherwise a retried push fails.
func TestImmutableTags(t *testing.T) {
	f := newFixture(t)

	configA := f.putBlob(t, "config a")
	payloadA, parsedA := imageManifest(t, configA)
	digestA := oci.FromBytes(payloadA).String()
	configB := f.putBlob(t, "config b")
	payloadB, parsedB := imageManifest(t, configB)
	digestB := oci.FromBytes(payloadB).String()

	if _, err := f.store.PutManifest(f.ctx, catalog.PutManifestParams{
		RepositoryID: f.repoID, Digest: digestA, Tag: "v1",
		MediaType: oci.MediaTypeOCIManifest, Payload: payloadA, Parsed: parsedA,
		ImmutableTags: true,
	}); err != nil {
		t.Fatalf("first push to an immutable tag: %v", err)
	}

	if _, err := f.store.PutManifest(f.ctx, catalog.PutManifestParams{
		RepositoryID: f.repoID, Digest: digestA, Tag: "v1",
		MediaType: oci.MediaTypeOCIManifest, Payload: payloadA, Parsed: parsedA,
		ImmutableTags: true,
	}); err != nil {
		t.Errorf("re-pushing identical content to an immutable tag failed: %v", err)
	}

	_, err := f.store.PutManifest(f.ctx, catalog.PutManifestParams{
		RepositoryID: f.repoID, Digest: digestB, Tag: "v1",
		MediaType: oci.MediaTypeOCIManifest, Payload: payloadB, Parsed: parsedB,
		ImmutableTags: true,
	})
	if !errors.Is(err, catalog.ErrImmutable) {
		t.Errorf("moving an immutable tag returned %v, want ErrImmutable", err)
	}
}

func TestListTagsPaginatesLexically(t *testing.T) {
	f := newFixture(t)
	config := f.putBlob(t, "shared config")

	want := []string{"alpha", "beta", "delta", "gamma", "omega"}
	for i, name := range want {
		payload, parsed := imageManifest(t, config, f.putBlob(t, fmt.Sprintf("layer %d", i)))
		if _, err := f.store.PutManifest(f.ctx, catalog.PutManifestParams{
			RepositoryID: f.repoID, Digest: oci.FromBytes(payload).String(), Tag: name,
			MediaType: oci.MediaTypeOCIManifest, Payload: payload, Parsed: parsed,
		}); err != nil {
			t.Fatal(err)
		}
	}

	page, err := f.store.ListTags(f.ctx, f.repoID, 2, "")
	if err != nil {
		t.Fatalf("listing tags: %v", err)
	}
	if len(page.Tags) != 2 || page.Tags[0] != "alpha" || page.Tags[1] != "beta" {
		t.Fatalf("first page = %v, want [alpha beta]", page.Tags)
	}
	if !page.HasMore {
		t.Error("first page did not report more results")
	}

	var all []string
	last := ""
	for {
		page, err := f.store.ListTags(f.ctx, f.repoID, 2, last)
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, page.Tags...)
		if !page.HasMore {
			break
		}
		last = page.Tags[len(page.Tags)-1]
	}
	if len(all) != len(want) {
		t.Fatalf("paging collected %v, want %v", all, want)
	}
	for i := range want {
		if all[i] != want[i] {
			t.Errorf("tag %d = %q, want %q", i, all[i], want[i])
		}
	}

	// REQ-OCI-08: n=0 returns an empty list, not everything.
	empty, err := f.store.ListTags(f.ctx, f.repoID, 0, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(empty.Tags) != 0 {
		t.Errorf("n=0 returned %d tags, want 0", len(empty.Tags))
	}
}

// Two nodes pushing the same layer simultaneously is routine. Content
// addressing makes it safe, and the store must not turn it into an error.
func TestConcurrentBlobCommitIsSafe(t *testing.T) {
	f := newFixture(t)
	content := "a layer pushed by everyone at once"
	digest := oci.FromBytes([]byte(content)).String()

	const writers = 8
	var wg sync.WaitGroup
	errs := make([]error, writers)
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = f.store.CommitBlob(context.Background(), f.repoID, digest,
				int64(len(content)), "", "ref/"+digest)
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("concurrent writer %d failed: %v", i, err)
		}
	}

	var count int
	if err := f.pool.QueryRow(f.ctx, `SELECT count(*) FROM blobs WHERE digest = $1`, digest).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Errorf("blobs table holds %d rows for one digest, want 1", count)
	}
}

// SEC-03: a blob is only servable from a repository that has been linked to it.
func TestBlobIsScopedToItsRepository(t *testing.T) {
	f := newFixture(t)
	digest := f.putBlob(t, "private layer")

	otherRepoID := testsupport.Repo(t, f.pool, f.orgID, "acme/other")

	if _, err := f.store.BlobInRepository(f.ctx, f.repoID, digest); err != nil {
		t.Fatalf("blob not readable from its own repository: %v", err)
	}
	if _, err := f.store.BlobInRepository(f.ctx, otherRepoID, digest); !errors.Is(err, catalog.ErrNotFound) {
		t.Error("a blob was readable from a repository it was never linked to")
	}

	// Linking is what a cross-repository mount does, after authorization.
	blob, err := f.store.BlobGlobal(f.ctx, digest)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.LinkBlob(f.ctx, otherRepoID, blob.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.BlobInRepository(f.ctx, otherRepoID, digest); err != nil {
		t.Errorf("blob not readable after linking: %v", err)
	}
}

func TestQuotaAccounting(t *testing.T) {
	f := newFixture(t)
	f.putBlob(t, "0123456789") // 10 bytes

	status, err := f.store.CheckQuota(f.ctx, f.orgID, 0)
	if err != nil {
		t.Fatalf("checking quota: %v", err)
	}
	if status.QuotaBytes != nil {
		t.Error("a new organization has a quota; unlimited is the default")
	}
	if status.UsedBytes != 10 {
		t.Errorf("usage = %d, want 10", status.UsedBytes)
	}

	quota := int64(15)
	if _, err := f.pool.Exec(f.ctx,
		`UPDATE organizations SET quota_bytes = $2 WHERE id = $1`, f.orgID, quota); err != nil {
		t.Fatal(err)
	}

	status, err = f.store.CheckQuota(f.ctx, f.orgID, 3)
	if err != nil {
		t.Fatal(err)
	}
	if status.Exceeded {
		t.Error("a write within quota was reported as exceeding it")
	}
	if status.SoftExceeded {
		t.Error("usage at 67% of quota tripped the 80% soft threshold")
	}

	// Push usage over 80% and the warning threshold should trip while writes
	// are still permitted.
	f.putBlob(t, "12") // total 12 of 15 = 80%
	status, err = f.store.CheckQuota(f.ctx, f.orgID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if !status.SoftExceeded {
		t.Errorf("usage of %d/%d did not trip the soft threshold", status.UsedBytes, *status.QuotaBytes)
	}
	if status.Exceeded {
		t.Error("a write still within quota was refused")
	}

	status, err = f.store.CheckQuota(f.ctx, f.orgID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if !status.Exceeded {
		t.Error("a write beyond quota was not refused")
	}
}
