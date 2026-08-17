package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/mantle-sh/mantle/internal/auth/authz"
	"github.com/mantle-sh/mantle/internal/catalog"
	"github.com/mantle-sh/mantle/internal/gc"
	"github.com/mantle-sh/mantle/internal/ledger"
)

// Tier 0 (§13.2): with no integration at all, a plain push must yield the
// commit that produced the image, read from the standard OCI labels that
// builders already write.
//
// This is the tier that has to carry the product on day one, and the M0 spike
// flagged it as the thing that could change the roadmap — so it is asserted
// directly rather than assumed.
func TestTier0ProvenanceFromImageLabels(t *testing.T) {
	h := newHarness(t)
	secret := h.deployToken("builder", "acme", "acme/", authz.RoleContributor)
	c := h.TokenClient(secret)

	const commit = "a3f81c2e9b7d4f0a1c2e3b4d5f6a7b8c9d0e1f2a"
	image := buildImage(t, []string{"layer"}, map[string]string{
		"org.opencontainers.image.revision": commit,
		"org.opencontainers.image.source":   "https://github.com/acme/web",
		"org.opencontainers.image.version":  "2.4.1",
	}, nil)
	c.PushImage("acme/web", "v2.4.1", image)

	provenance := waitForProvenance(t, h, "acme/web", image.ManifestDigest)
	if provenance.CommitSHA != commit {
		t.Errorf("commit = %q, want %q", provenance.CommitSHA, commit)
	}
	if provenance.SourceURL != "https://github.com/acme/web" {
		t.Errorf("source = %q, want the repository URL", provenance.SourceURL)
	}
	if provenance.Source != ledger.SourceLabel {
		t.Errorf("provenance source = %q, want %q (a label is a fact, not a guess)",
			provenance.Source, ledger.SourceLabel)
	}
	if provenance.Confidence != ledger.ConfidenceCertain {
		t.Errorf("confidence = %q, want certain", provenance.Confidence)
	}
}

// Manifest annotations take precedence over config labels, and both are treated
// as facts rather than inferences.
func TestTier0ProvenanceFromManifestAnnotations(t *testing.T) {
	h := newHarness(t)
	secret := h.deployToken("builder", "acme", "acme/", authz.RoleContributor)
	c := h.TokenClient(secret)

	const commit = "b4e92d3f8a6c5b0e1d2f3a4b5c6d7e8f9a0b1c2d"
	image := buildImage(t, []string{"layer"}, nil, map[string]string{
		"org.opencontainers.image.revision": commit,
	})
	c.PushImage("acme/web", "v1", image)

	provenance := waitForProvenance(t, h, "acme/web", image.ManifestDigest)
	if provenance.CommitSHA != commit {
		t.Errorf("commit = %q, want %q", provenance.CommitSHA, commit)
	}
	if provenance.Source != ledger.SourceAnnotation {
		t.Errorf("provenance source = %q, want annotation", provenance.Source)
	}
}

// When an image carries no provenance at all, the tag's shape is the last
// resort — and the result must be marked probable, so a wrong guess is
// distinguishable from a fact.
func TestTier0ProvenanceInferredFromTagIsMarkedProbable(t *testing.T) {
	h := newHarness(t)
	secret := h.deployToken("builder", "acme", "acme/", authz.RoleContributor)
	c := h.TokenClient(secret)

	image := buildImage(t, []string{"layer"}, nil, nil)
	c.PushImage("acme/web", "sha-a3f81c2", image)

	provenance := waitForProvenance(t, h, "acme/web", image.ManifestDigest)
	if provenance.CommitSHA != "a3f81c2" {
		t.Errorf("commit = %q, want a3f81c2 inferred from the tag", provenance.CommitSHA)
	}
	if provenance.Source != ledger.SourceTagPattern {
		t.Errorf("source = %q, want tag_pattern", provenance.Source)
	}
	if provenance.Confidence != ledger.ConfidenceProbable {
		t.Errorf("confidence = %q, want probable: an inference must not be presented as a fact",
			provenance.Confidence)
	}
}

// Tier 1 (§13.2): a single HTTP call from whatever already deploys upgrades the
// record to reported, with the real host list.
func TestTier1DeployReporting(t *testing.T) {
	h := newHarness(t)
	secret := h.deployToken("builder", "acme", "acme/", authz.RoleContributor)
	c := h.TokenClient(secret)

	const commit = "c5fa03e4b9d7c6a1f2e3d4c5b6a7f8e9d0c1b2a3"
	image := buildImage(t, []string{"layer"}, map[string]string{
		"org.opencontainers.image.revision": commit,
	}, nil)
	c.PushImage("acme/web", "v2.4.1", image)
	waitForProvenance(t, h, "acme/web", image.ManifestDigest)

	body := map[string]any{
		"repository":  "acme/web",
		"digest":      image.ManifestDigest,
		"environment": "production",
		"status":      "active",
		"performer":   "nima",
		"deploy_tool": "ansible",
		"hosts":       []string{"web-1", "web-2"},
		"external_id": "run-4711",
	}
	payload, _ := json.Marshal(body)

	resp := c.Post("/api/v1/deployments", bytes.NewReader(payload),
		map[string]string{"Content-Type": "application/json"})
	defer resp.Body.Close()
	requireStatus(t, resp, http.StatusCreated, "recording a deployment")

	var recorded struct {
		Digest     string `json:"digest"`
		Status     string `json:"status"`
		Confidence string `json:"confidence"`
		CommitSHA  string `json:"commit_sha"`
		Performer  string `json:"performer"`
		Hosts      []struct {
			Hostname string `json:"hostname"`
			Status   string `json:"status"`
		} `json:"hosts"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&recorded); err != nil {
		t.Fatal(err)
	}
	if recorded.Confidence != "reported" {
		t.Errorf("confidence = %q, want reported", recorded.Confidence)
	}
	// The commit is filled in from the image even though the caller did not
	// send one — a deploy hook rarely knows it, the image usually does.
	if recorded.CommitSHA != commit {
		t.Errorf("commit = %q, want %q inferred from the image", recorded.CommitSHA, commit)
	}
	if len(recorded.Hosts) != 2 {
		t.Errorf("recorded %d hosts, want 2", len(recorded.Hosts))
	}

	// Idempotency: the same external id must collapse onto one record, because
	// a fire-and-forget hook will retry.
	for i := 0; i < 3; i++ {
		retry := c.Post("/api/v1/deployments", bytes.NewReader(payload),
			map[string]string{"Content-Type": "application/json"})
		retry.Body.Close()
		requireStatus(t, retry, http.StatusCreated, "retrying a deploy report")
	}

	var count int
	if err := h.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM deployments WHERE external_id = 'run-4711'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Errorf("four identical reports produced %d deployment rows, want 1", count)
	}
}

// Deploying a new image must supersede the previous one, and only within the
// same environment — a staging deploy must never disturb production.
func TestDeploymentSupersedesPreviousInSameEnvironment(t *testing.T) {
	h := newHarness(t)
	secret := h.deployToken("builder", "acme", "acme/", authz.RoleContributor)
	c := h.TokenClient(secret)

	first := buildImage(t, []string{"v1 layer"}, nil, nil)
	second := buildImage(t, []string{"v2 layer"}, nil, nil)
	c.PushImage("acme/web", "v1", first)
	c.PushImage("acme/web", "v2", second)

	recordDeploy(t, c, "acme/web", first.ManifestDigest, "production")
	recordDeploy(t, c, "acme/web", second.ManifestDigest, "production")
	recordDeploy(t, c, "acme/web", first.ManifestDigest, "staging")

	var activeProduction int
	if err := h.pool.QueryRow(context.Background(), `
		SELECT count(*) FROM deployments d
		JOIN repositories r ON r.id = d.repository_id
		WHERE r.name = 'acme/web' AND d.environment = 'production' AND d.status = 'active'`).
		Scan(&activeProduction); err != nil {
		t.Fatal(err)
	}
	if activeProduction != 1 {
		t.Errorf("%d production deployments are active, want exactly 1", activeProduction)
	}

	// Staging must still be active: superseding is scoped to one environment.
	var activeStaging int
	if err := h.pool.QueryRow(context.Background(), `
		SELECT count(*) FROM deployments d
		JOIN repositories r ON r.id = d.repository_id
		WHERE r.name = 'acme/web' AND d.environment = 'staging' AND d.status = 'active'`).
		Scan(&activeStaging); err != nil {
		t.Fatal(err)
	}
	if activeStaging != 1 {
		t.Errorf("%d staging deployments are active, want 1: superseding leaked across environments",
			activeStaging)
	}
}

// §13.4 — the promise the whole product rests on: a retention or collection
// pass cannot remove the image production is running, nor the rollback window
// behind it.
func TestDeployedImageSurvivesGarbageCollection(t *testing.T) {
	h := newHarness(t)
	secret := h.deployToken("builder", "acme", "acme/", authz.RoleContributor)
	c := h.TokenClient(secret)

	deployed := buildImage(t, []string{"the running image"}, nil, nil)
	abandoned := buildImage(t, []string{"an untagged leftover"}, nil, nil)

	c.PushImage("acme/web", "v1", deployed)
	// Pushed by digest and never tagged: exactly what GC exists to reclaim.
	c.PushImage("acme/web", abandoned.ManifestDigest, abandoned)

	recordDeploy(t, c, "acme/web", deployed.ManifestDigest, "production")

	// Backdate everything past the grace period, and run a collector whose
	// quarantine window has already elapsed, so one pass both marks and sweeps.
	backdate(t, h, -48*time.Hour)
	collector := gc.New(gc.Options{
		Pool:             h.pool,
		Storage:          h.server.Storage,
		Ledger:           ledger.New(h.pool),
		Logger:           h.server.Logger,
		GracePeriod:      time.Hour,
		QuarantinePeriod: time.Hour,
		BatchSize:        100,
		RollbackDepth:    3,
		UploadSessionTTL: time.Hour,
	})

	// First pass quarantines the abandoned manifest; the second sweeps it,
	// since its quarantine timestamp must also age past the window.
	if _, err := collector.Run(context.Background(), false); err != nil {
		t.Fatalf("first collection pass: %v", err)
	}
	backdate(t, h, -48*time.Hour)
	stats, err := collector.Run(context.Background(), false)
	if err != nil {
		t.Fatalf("second collection pass: %v", err)
	}
	t.Logf("collection reclaimed %d bytes from %d blobs", stats.BytesReclaimed, stats.BlobsSwept)

	// The deployed image must still pull, completely.
	resp := c.Get("/v2/acme/web/manifests/v1")
	defer resp.Body.Close()
	requireStatus(t, resp, http.StatusOK, "pulling the deployed image after collection")

	for _, digest := range deployed.LayerDigests {
		layer := c.Get("/v2/acme/web/blobs/" + digest)
		layer.Body.Close()
		if layer.StatusCode != http.StatusOK {
			t.Errorf("a layer of the deployed image was collected: %s returned %d",
				digest, layer.StatusCode)
		}
	}
	configResp := c.Get("/v2/acme/web/blobs/" + deployed.ConfigDigest)
	configResp.Body.Close()
	if configResp.StatusCode != http.StatusOK {
		t.Error("the deployed image's config blob was collected")
	}

	// And the abandoned one must be gone, or the test proved nothing.
	gone := c.Get("/v2/acme/web/manifests/" + abandoned.ManifestDigest)
	gone.Body.Close()
	if gone.StatusCode == http.StatusOK {
		t.Error("the untagged, undeployed manifest survived collection; " +
			"this test would pass even if pinning did nothing")
	}
}

// The database itself must refuse to remove a manifest a deployment references
// (§11.1, §13.4). Application checks can be bypassed by a bug; a foreign key
// cannot.
func TestDatabaseRefusesToDeleteADeployedManifest(t *testing.T) {
	h := newHarness(t)
	secret := h.deployToken("builder", "acme", "acme/", authz.RoleContributor)
	c := h.TokenClient(secret)

	image := buildImage(t, []string{"layer"}, nil, nil)
	c.PushImage("acme/web", "v1", image)
	recordDeploy(t, c, "acme/web", image.ManifestDigest, "production")

	ctx := context.Background()
	// Remove the tag so nothing but the deployment holds the manifest.
	if _, err := h.pool.Exec(ctx, `
		DELETE FROM tags WHERE manifest_id =
		  (SELECT id FROM manifests WHERE digest = $1)`, image.ManifestDigest); err != nil {
		t.Fatal(err)
	}

	_, err := h.pool.Exec(ctx, `DELETE FROM manifests WHERE digest = $1`, image.ManifestDigest)
	if err == nil {
		t.Fatal("the database allowed deletion of a manifest referenced by a deployment; " +
			"the ON DELETE RESTRICT guarantee in §13.4 is not in force")
	}
	t.Logf("database correctly refused: %v", err)
}

// The API-level deletion path must refuse too, with an explanation rather than
// a constraint violation.
func TestDeletingADeployedManifestIsRefusedWithAReason(t *testing.T) {
	h := newHarness(t)
	secret := h.deployToken("builder", "acme", "acme/", authz.RoleMaintainer)
	c := h.TokenClient(secret)

	image := buildImage(t, []string{"layer"}, nil, nil)
	c.PushImage("acme/web", "v1", image)
	recordDeploy(t, c, "acme/web", image.ManifestDigest, "production")

	resp := c.Delete("/v2/acme/web/manifests/" + image.ManifestDigest)
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusAccepted {
		t.Fatal("a deployed manifest was deleted through the registry API")
	}
	errs := decodeOCIError(t, resp)
	if len(errs) == 0 {
		t.Fatal("no error envelope was returned")
	}
	if errs[0].Code != "DENIED" {
		t.Errorf("error code = %q, want DENIED", errs[0].Code)
	}
	// The message must say why, not merely refuse.
	if !bytes.Contains([]byte(errs[0].Message), []byte("deployment")) {
		t.Errorf("message %q does not explain that a deployment is blocking the delete",
			errs[0].Message)
	}
}

// The composed ledger resource (§13.5) is what `mantle ledger status` and any
// future UI page read. It must answer the whole question in one response.
func TestLedgerViewIsComposedInOneResponse(t *testing.T) {
	h := newHarness(t)
	secret := h.deployToken("builder", "acme", "acme/", authz.RoleContributor)
	c := h.TokenClient(secret)

	const commit = "d6ab14f5c0e8d7b2a3f4e5d6c7b8a9f0e1d2c3b4"
	older := buildImage(t, []string{"v1"}, nil, nil)
	current := buildImage(t, []string{"v2"}, map[string]string{
		"org.opencontainers.image.revision": commit,
		"org.opencontainers.image.source":   "https://github.com/acme/web",
	}, nil)
	c.PushImage("acme/web", "v1", older)
	c.PushImage("acme/web", "v2", current)
	waitForProvenance(t, h, "acme/web", current.ManifestDigest)

	recordDeploy(t, c, "acme/web", older.ManifestDigest, "production")
	recordDeploy(t, c, "acme/web", current.ManifestDigest, "production")

	resp := c.Get("/api/v1/repositories/acme/web/ledger")
	defer resp.Body.Close()
	requireStatus(t, resp, http.StatusOK, "reading the ledger view")

	var view struct {
		Repository string `json:"repository"`
		SourceURL  string `json:"source_url"`
		Running    *struct {
			Digest     string `json:"digest"`
			Tag        string `json:"tag"`
			CommitSHA  string `json:"commit_sha"`
			Confidence string `json:"confidence"`
		} `json:"running"`
		Rollback []struct {
			Digest string `json:"digest"`
			Tag    string `json:"tag"`
			Pinned bool   `json:"pinned"`
		} `json:"rollback_candidates"`
		Tags []struct {
			Name string `json:"name"`
		} `json:"tags"`
		Storage struct {
			TotalBytes int64 `json:"total_bytes"`
			Manifests  int   `json:"manifests"`
		} `json:"storage"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&view); err != nil {
		t.Fatal(err)
	}

	if view.Running == nil {
		t.Fatal("the ledger view reports nothing running")
	}
	if view.Running.Digest != current.ManifestDigest {
		t.Errorf("running digest = %s, want %s", view.Running.Digest, current.ManifestDigest)
	}
	if view.Running.CommitSHA != commit {
		t.Errorf("running commit = %q, want %q", view.Running.CommitSHA, commit)
	}
	if view.SourceURL != "https://github.com/acme/web" {
		t.Errorf("source URL = %q, want the repository URL from the image", view.SourceURL)
	}

	if len(view.Rollback) == 0 {
		t.Fatal("no rollback candidate was offered despite an earlier deployment")
	}
	if view.Rollback[0].Digest != older.ManifestDigest {
		t.Errorf("rollback target = %s, want the previous deployment %s",
			view.Rollback[0].Digest, older.ManifestDigest)
	}
	if !view.Rollback[0].Pinned {
		t.Error("the rollback target is not pinned; it could be collected, " +
			"which is exactly what §13.4 promises cannot happen")
	}

	if len(view.Tags) != 2 {
		t.Errorf("ledger view lists %d tags, want 2", len(view.Tags))
	}
	if view.Storage.Manifests != 2 {
		t.Errorf("ledger view reports %d manifests, want 2", view.Storage.Manifests)
	}
}

// --- helpers ---

func recordDeploy(t *testing.T, c *client, repo, digest, environment string) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"repository": repo, "digest": digest,
		"environment": environment, "status": "active",
		"external_id": fmt.Sprintf("%s-%s-%d", environment, digest[:20], time.Now().UnixNano()),
	})
	resp := c.Post("/api/v1/deployments", bytes.NewReader(body),
		map[string]string{"Content-Type": "application/json"})
	defer resp.Body.Close()
	requireStatus(t, resp, http.StatusCreated, "recording a deployment of "+repo)
}

// waitForProvenance polls until the ledger has processed the push. Provenance
// extraction is asynchronous by design (REQ-LEDGER-01), so a test that read it
// immediately would be racing the recorder.
func waitForProvenance(t *testing.T, h *harness, repo, digest string) *ledger.Provenance {
	t.Helper()
	ctx := context.Background()
	store := ledger.New(h.pool)
	cat := catalog.New(h.pool)

	repository, err := cat.RepositoryByName(ctx, repo)
	if err != nil {
		t.Fatalf("resolving %s: %v", repo, err)
	}
	manifest, err := cat.ManifestByDigest(ctx, repository.ID, digest)
	if err != nil {
		t.Fatalf("resolving manifest %s: %v", digest, err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		provenance, err := store.ProvenanceFor(ctx, manifest.ID)
		if err == nil {
			return provenance
		}
		if !errors.Is(err, ledger.ErrNotFound) {
			t.Fatalf("reading provenance: %v", err)
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("provenance for %s was not recorded within the timeout", digest)
	return nil
}

// backdate ages every timestamp that garbage collection consults, so a test can
// exercise the grace and quarantine windows without waiting hours.
func backdate(t *testing.T, h *harness, by time.Duration) {
	t.Helper()
	ctx := context.Background()
	interval := fmt.Sprintf("%d seconds", int(by.Seconds()))
	for _, statement := range []string{
		`UPDATE blobs SET created_at = created_at + $1::interval`,
		`UPDATE manifests SET created_at = created_at + $1::interval`,
		`UPDATE blobs SET quarantined_at = quarantined_at + $1::interval WHERE quarantined_at IS NOT NULL`,
		`UPDATE manifests SET quarantined_at = quarantined_at + $1::interval WHERE quarantined_at IS NOT NULL`,
	} {
		if _, err := h.pool.Exec(ctx, statement, interval); err != nil {
			t.Fatalf("backdating timestamps: %v", err)
		}
	}
}
