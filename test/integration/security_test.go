package integration

import (
	"bytes"
	"net/http"
	"strings"
	"testing"

	"github.com/mantle-sh/mantle/internal/auth/authz"
	"github.com/mantle-sh/mantle/internal/config"
	"github.com/mantle-sh/mantle/internal/oci"
)

// REQ-AUTHZ-01 / SEC-03. Cross-repository mount must verify pull permission on
// the *source* repository, not only push on the target.
//
// Without the source check, anyone who can push anywhere can materialise any
// layer in the instance into a repository they control, given only its digest —
// and layer digests are not secret. This is the requirement the specification
// singles out as needing its own test file.
func TestCrossRepositoryMountRequiresSourcePermission(t *testing.T) {
	h := newHarness(t)
	h.createOrg("victim")

	// A private image in an organization the attacker cannot read.
	ownerSecret := h.deployToken("victim-builder", "victim", "victim/", authz.RoleContributor)
	owner := h.TokenClient(ownerSecret)
	secretImage := buildImage(t, []string{"proprietary layer contents"}, nil, nil)
	owner.PushImage("victim/private", "v1", secretImage)
	secretDigest := secretImage.LayerDigests[0]

	// The attacker can push to their own namespace and nothing else.
	attackerSecret := h.deployToken("attacker", "acme", "acme/", authz.RoleContributor)
	attacker := h.TokenClient(attackerSecret)

	mountPath := "/v2/acme/steal/blobs/uploads/?mount=" + secretDigest + "&from=victim/private"
	resp := attacker.Post(mountPath, nil, nil)
	defer resp.Body.Close()

	// The mount must be declined. The spec-sanctioned way to decline is to fall
	// back to an ordinary upload with 202, which also avoids confirming whether
	// the blob existed at all.
	if resp.StatusCode == http.StatusCreated {
		t.Fatal("cross-repository mount succeeded without pull permission on the source: " +
			"any pushable identity can now exfiltrate private layers by digest")
	}
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("declined mount returned %d, want 202 (fall back to a normal upload)", resp.StatusCode)
	}

	// And the blob must genuinely not be readable from the attacker's repository.
	check := attacker.Head("/v2/acme/steal/blobs/" + secretDigest)
	defer check.Body.Close()
	if check.StatusCode == http.StatusOK {
		t.Error("the private layer became readable from the attacker's repository")
	}
}

// The same mount must succeed when the caller legitimately holds pull on the
// source — otherwise the check is simply broken in the safe direction and
// deduplication across a team's repositories stops working.
func TestCrossRepositoryMountSucceedsWithPermission(t *testing.T) {
	h := newHarness(t)
	secret := h.deployToken("builder", "acme", "acme/", authz.RoleContributor)
	c := h.TokenClient(secret)

	image := buildImage(t, []string{"a shared base layer"}, nil, nil)
	c.PushImage("acme/base", "v1", image)
	layerDigest := image.LayerDigests[0]

	resp := c.Post("/v2/acme/derived/blobs/uploads/?mount="+layerDigest+"&from=acme/base", nil, nil)
	defer resp.Body.Close()
	requireStatus(t, resp, http.StatusCreated, "mounting a layer the caller may read")

	if got := resp.Header.Get("Docker-Content-Digest"); got != layerDigest {
		t.Errorf("Docker-Content-Digest = %q, want %q", got, layerDigest)
	}

	// The mounted layer is now servable from the target without re-uploading.
	check := c.Head("/v2/acme/derived/blobs/" + layerDigest)
	defer check.Body.Close()
	requireStatus(t, check, http.StatusOK, "reading the mounted layer")
}

// REQ-OCI-11. An unauthenticated request for a private repository and one for a
// repository that does not exist must be indistinguishable, or the private
// namespace can be enumerated — and namespaces leak customer names.
func TestExistenceIsNotDisclosedToAnonymousCallers(t *testing.T) {
	h := newHarness(t)
	secret := h.deployToken("builder", "acme", "acme/", authz.RoleContributor)
	image := buildImage(t, []string{"private"}, nil, nil)
	h.TokenClient(secret).PushImage("acme/real", "v1", image)

	anon := h.Anonymous()

	existing := anon.Get("/v2/acme/real/manifests/v1")
	missing := anon.Get("/v2/acme/does-not-exist/manifests/v1")
	defer existing.Body.Close()
	defer missing.Body.Close()

	if existing.StatusCode != http.StatusUnauthorized {
		t.Errorf("anonymous request for an existing private repository = %d, want 401", existing.StatusCode)
	}
	if missing.StatusCode != existing.StatusCode {
		t.Errorf("existing repository returned %d but nonexistent returned %d: "+
			"the difference lets an anonymous caller enumerate private repositories",
			existing.StatusCode, missing.StatusCode)
	}
	// The challenges must be identical once the repository name each caller
	// asked for is normalised away. The scope names what was *requested*, so it
	// necessarily differs; anything else differing — realm, service, the set of
	// parameters — would be a signal about what exists.
	normalise := func(resp *http.Response, name string) string {
		return strings.ReplaceAll(resp.Header.Get("WWW-Authenticate"), name, "<repo>")
	}
	existingChallenge := normalise(existing, "acme/real")
	missingChallenge := normalise(missing, "acme/does-not-exist")
	if existingChallenge != missingChallenge {
		t.Errorf("the challenge differs beyond the requested name:\n existing: %s\n  missing: %s",
			existingChallenge, missingChallenge)
	}
	if existingChallenge == "" {
		t.Error("no WWW-Authenticate challenge was sent")
	}

	// The bodies must also be identical: an error message that says "not found"
	// for one and "unauthorized" for the other discloses the same thing.
	if got, want := readBody(existing), readBody(missing); got != want {
		t.Errorf("the error body differs between an existing and a nonexistent repository:\n %s\n %s", got, want)
	}
}

// The authenticated half of REQ-OCI-11: a caller who cannot read a repository
// gets 404 NAME_UNKNOWN, not 403. A 403 would confirm the repository exists.
func TestAuthenticatedCallerWithoutPermissionGetsNotFound(t *testing.T) {
	h := newHarness(t)
	h.createOrg("victim")

	ownerSecret := h.deployToken("victim-builder", "victim", "victim/", authz.RoleContributor)
	image := buildImage(t, []string{"private"}, nil, nil)
	h.TokenClient(ownerSecret).PushImage("victim/private", "v1", image)

	outsiderSecret := h.deployToken("outsider", "acme", "acme/", authz.RoleContributor)
	outsider := h.TokenClient(outsiderSecret)

	existing := outsider.Get("/v2/victim/private/manifests/v1")
	defer existing.Body.Close()
	if existing.StatusCode != http.StatusNotFound {
		t.Fatalf("reading another organization's repository = %d, want 404 "+
			"(403 would confirm it exists)", existing.StatusCode)
	}
	errs := decodeOCIError(t, existing)
	if len(errs) == 0 || errs[0].Code != "NAME_UNKNOWN" {
		t.Errorf("error code = %v, want NAME_UNKNOWN", errs)
	}

	missing := outsider.Get("/v2/victim/imaginary/manifests/v1")
	defer missing.Body.Close()
	if missing.StatusCode != existing.StatusCode {
		t.Errorf("existing-but-forbidden returned %d, nonexistent returned %d: these must match",
			existing.StatusCode, missing.StatusCode)
	}
}

// A reader must not be able to push, and the refusal must not reveal anything
// extra about the repository.
func TestReaderCannotPush(t *testing.T) {
	h := newHarness(t)
	builderSecret := h.deployToken("builder", "acme", "acme/", authz.RoleContributor)
	image := buildImage(t, []string{"layer"}, nil, nil)
	h.TokenClient(builderSecret).PushImage("acme/web", "v1", image)

	readerSecret := h.deployToken("servers", "acme", "acme/", authz.RoleReader)
	reader := h.TokenClient(readerSecret)

	// Reading works.
	read := reader.Get("/v2/acme/web/manifests/v1")
	defer read.Body.Close()
	requireStatus(t, read, http.StatusOK, "a reader pulling")

	// Writing does not.
	write := reader.Post("/v2/acme/web/blobs/uploads/", nil, nil)
	defer write.Body.Close()
	if write.StatusCode == http.StatusAccepted {
		t.Error("a pull-only deploy token was able to start an upload")
	}

	// Nor does deleting.
	del := reader.Delete("/v2/acme/web/manifests/v1")
	defer del.Body.Close()
	if del.StatusCode == http.StatusAccepted {
		t.Error("a pull-only deploy token was able to delete a manifest")
	}
}

// SEC-04: the catalog must list only repositories the caller can read.
func TestCatalogIsFilteredByPermission(t *testing.T) {
	h := newHarness(t)
	h.createOrg("victim")

	acmeSecret := h.deployToken("acme-builder", "acme", "acme/", authz.RoleContributor)
	victimSecret := h.deployToken("victim-builder", "victim", "victim/", authz.RoleContributor)

	image := buildImage(t, []string{"layer"}, nil, nil)
	h.TokenClient(acmeSecret).PushImage("acme/web", "v1", image)
	h.TokenClient(victimSecret).PushImage("victim/secret", "v1", image)

	resp := h.TokenClient(acmeSecret).Get("/v2/_catalog")
	defer resp.Body.Close()
	requireStatus(t, resp, http.StatusOK, "listing the catalog")

	body := readBody(resp)
	if !strings.Contains(body, "acme/web") {
		t.Errorf("the catalog omitted a repository the caller can read: %s", body)
	}
	if strings.Contains(body, "victim/secret") {
		t.Errorf("the catalog disclosed a repository the caller cannot read: %s", body)
	}
}

// A disabled credential must stop working immediately, not at token expiry.
// This is what makes revoking a compromised deploy token meaningful.
func TestDisabledIdentityIsRefused(t *testing.T) {
	h := newHarness(t)
	secret := h.deployToken("compromised", "acme", "acme/", authz.RoleContributor)
	c := h.TokenClient(secret)

	image := buildImage(t, []string{"layer"}, nil, nil)
	c.PushImage("acme/web", "v1", image)

	// Confirm the cached token works before disabling.
	before := c.Get("/v2/acme/web/manifests/v1")
	before.Body.Close()
	requireStatus(t, before, http.StatusOK, "reading before the credential is disabled")

	if _, err := h.pool.Exec(t.Context(),
		`UPDATE identities SET disabled = true WHERE name = 'compromised'`); err != nil {
		t.Fatal(err)
	}

	// The client still holds a valid, unexpired token. It must stop working
	// anyway (REQ-AUTHZ-02).
	after := c.Get("/v2/acme/web/manifests/v1")
	defer after.Body.Close()
	if after.StatusCode == http.StatusOK {
		t.Error("a disabled credential's existing token still worked; " +
			"revocation would not take effect until the token expired")
	}
}

// SEC-05: a manifest larger than the configured limit is refused before it is
// parsed.
func TestOversizedManifestIsRejected(t *testing.T) {
	h := newHarness(t, func(cfg *config.Config) {
		cfg.Limits.MaxManifestSize = 1024
	})
	secret := h.deployToken("builder", "acme", "acme/", authz.RoleContributor)
	c := h.TokenClient(secret)

	// Valid JSON, simply too large.
	padding := strings.Repeat("a", 4096)
	payload := []byte(`{"schemaVersion":2,"mediaType":"` + oci.MediaTypeOCIManifest +
		`","layers":[],"annotations":{"pad":"` + padding + `"}}`)

	resp := c.Put("/v2/acme/web/manifests/big", bytes.NewReader(payload),
		map[string]string{"Content-Type": oci.MediaTypeOCIManifest})
	defer resp.Body.Close()
	requireStatus(t, resp, http.StatusBadRequest, "pushing an oversized manifest")
}

// SEC-07: a blob larger than the configured maximum is refused, and the limit
// is enforced against the stream rather than the client's Content-Length.
func TestOversizedBlobIsRejected(t *testing.T) {
	h := newHarness(t, func(cfg *config.Config) {
		cfg.Limits.MaxBlobSize = 1024
	})
	secret := h.deployToken("builder", "acme", "acme/", authz.RoleContributor)
	c := h.TokenClient(secret)

	content := bytes.Repeat([]byte("x"), 4096)
	digest := oci.FromBytes(content).String()

	startResp := c.Post("/v2/acme/web/blobs/uploads/", nil, nil)
	requireStatus(t, startResp, http.StatusAccepted, "starting an upload")
	location := startResp.Header.Get("Location")
	startResp.Body.Close()

	resp := c.Put(appendQuery(location, "digest", digest), bytes.NewReader(content), nil)
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusCreated {
		t.Fatal("a blob exceeding limits.max_blob_size was accepted")
	}
	errs := decodeOCIError(t, resp)
	if len(errs) == 0 || errs[0].Code != "SIZE_INVALID" {
		t.Errorf("error code = %v, want SIZE_INVALID", errs)
	}
}

// An upload session belongs to the identity that opened it. Another principal
// must not be able to finalise it and land a blob in a repository.
func TestUploadSessionIsBoundToItsIdentity(t *testing.T) {
	h := newHarness(t)
	firstSecret := h.deployToken("builder-one", "acme", "acme/", authz.RoleContributor)
	secondSecret := h.deployToken("builder-two", "acme", "acme/", authz.RoleContributor)

	startResp := h.TokenClient(firstSecret).Post("/v2/acme/web/blobs/uploads/", nil, nil)
	requireStatus(t, startResp, http.StatusAccepted, "starting an upload")
	location := startResp.Header.Get("Location")
	startResp.Body.Close()

	content := []byte("hijacked content")
	digest := oci.FromBytes(content).String()

	resp := h.TokenClient(secondSecret).Put(appendQuery(location, "digest", digest),
		bytes.NewReader(content), nil)
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusCreated {
		t.Error("a different identity completed an upload session it did not open")
	}
}

// Anonymous pull works for public repositories when it is enabled, and only for
// public repositories.
func TestAnonymousPullOfPublicRepository(t *testing.T) {
	h := newHarness(t, func(cfg *config.Config) {
		cfg.Auth.AnonymousPull = true
	})
	secret := h.deployToken("builder", "acme", "acme/", authz.RoleContributor)
	c := h.TokenClient(secret)

	image := buildImage(t, []string{"public layer"}, nil, nil)
	c.PushImage("acme/public", "v1", image)
	c.PushImage("acme/private", "v1", image)

	if _, err := h.pool.Exec(t.Context(),
		`UPDATE repositories SET visibility = 'public' WHERE name = 'acme/public'`); err != nil {
		t.Fatal(err)
	}

	anon := h.Anonymous()

	public := anon.Get("/v2/acme/public/manifests/v1")
	defer public.Body.Close()
	requireStatus(t, public, http.StatusOK, "anonymous pull of a public repository")

	private := anon.Get("/v2/acme/private/manifests/v1")
	defer private.Body.Close()
	if private.StatusCode == http.StatusOK {
		t.Error("anonymous pull reached a private repository")
	}
}
