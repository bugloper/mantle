package integration

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/mantle-sh/mantle/internal/auth/authz"
	"github.com/mantle-sh/mantle/internal/oci"
)

// TestPushPullRoundTrip is the test that matters most: an image goes in and
// comes back out, byte for byte, over the real protocol.
func TestPushPullRoundTrip(t *testing.T) {
	h := newHarness(t)
	secret := h.deployToken("builder", "acme", "acme/", authz.RoleContributor)
	c := h.TokenClient(secret)

	image := buildImage(t, []string{"layer one", "layer two"}, nil, nil)
	c.PushImage("acme/web", "v1.0.0", image)

	// --- manifest by tag ---
	resp := c.Get("/v2/acme/web/manifests/v1.0.0")
	defer resp.Body.Close()
	requireStatus(t, resp, http.StatusOK, "getting the manifest by tag")

	body := make([]byte, len(image.Manifest)+64)
	n, _ := resp.Body.Read(body)
	got := body[:n]

	// REQ-OCI-02: byte fidelity. The digest is over these exact bytes, so any
	// reserialization anywhere in the stack shows up here.
	if !bytes.Equal(got, image.Manifest) {
		t.Errorf("manifest was not returned byte-for-byte\n got: %s\nwant: %s", got, image.Manifest)
	}
	if digest := resp.Header.Get("Docker-Content-Digest"); digest != image.ManifestDigest {
		t.Errorf("Docker-Content-Digest = %q, want %q", digest, image.ManifestDigest)
	}
	if ct := resp.Header.Get("Content-Type"); ct != oci.MediaTypeOCIManifest {
		t.Errorf("Content-Type = %q, want %q", ct, oci.MediaTypeOCIManifest)
	}

	// --- manifest by digest resolves to the same content ---
	byDigest := c.Get("/v2/acme/web/manifests/" + image.ManifestDigest)
	defer byDigest.Body.Close()
	requireStatus(t, byDigest, http.StatusOK, "getting the manifest by digest")

	// --- layers ---
	for i, digest := range image.LayerDigests {
		layerResp := c.Get("/v2/acme/web/blobs/" + digest)
		requireStatus(t, layerResp, http.StatusOK, fmt.Sprintf("getting layer %d", i))
		content := make([]byte, len(image.Layers[i])+16)
		read, _ := layerResp.Body.Read(content)
		layerResp.Body.Close()
		if !bytes.Equal(content[:read], image.Layers[i]) {
			t.Errorf("layer %d content = %q, want %q", i, content[:read], image.Layers[i])
		}
	}

	// --- tag list ---
	tagsResp := c.Get("/v2/acme/web/tags/list")
	defer tagsResp.Body.Close()
	requireStatus(t, tagsResp, http.StatusOK, "listing tags")
	var tags struct {
		Name string   `json:"name"`
		Tags []string `json:"tags"`
	}
	if err := json.NewDecoder(tagsResp.Body).Decode(&tags); err != nil {
		t.Fatalf("decoding the tag list: %v", err)
	}
	if tags.Name != "acme/web" {
		t.Errorf("tag list name = %q, want acme/web", tags.Name)
	}
	if len(tags.Tags) != 1 || tags.Tags[0] != "v1.0.0" {
		t.Errorf("tags = %v, want [v1.0.0]", tags.Tags)
	}
}

// REQ-OCI-03: HEAD must return the same headers as GET, with no body. Docker
// issues a HEAD before every layer push, so a mismatch here causes either a
// redundant multi-gigabyte upload or a manifest referencing a blob we cannot serve.
func TestHeadMatchesGet(t *testing.T) {
	h := newHarness(t)
	secret := h.deployToken("builder", "acme", "acme/", authz.RoleContributor)
	c := h.TokenClient(secret)

	image := buildImage(t, []string{"a layer"}, nil, nil)
	c.PushImage("acme/web", "v1", image)

	for _, path := range []string{
		"/v2/acme/web/manifests/v1",
		"/v2/acme/web/blobs/" + image.LayerDigests[0],
	} {
		getResp := c.Get(path)
		headResp := c.Head(path)

		if getResp.StatusCode != headResp.StatusCode {
			t.Errorf("%s: HEAD status %d, GET status %d", path, headResp.StatusCode, getResp.StatusCode)
		}
		for _, header := range []string{"Content-Type", "Content-Length", "Docker-Content-Digest"} {
			if got, want := headResp.Header.Get(header), getResp.Header.Get(header); got != want {
				t.Errorf("%s: HEAD %s = %q, GET %s = %q", path, header, got, header, want)
			}
		}

		headBody := make([]byte, 1)
		n, _ := headResp.Body.Read(headBody)
		if n != 0 {
			t.Errorf("%s: HEAD returned a body", path)
		}
		getResp.Body.Close()
		headResp.Body.Close()
	}
}

// REQ-OCI-05: a manifest referencing a blob that was never uploaded must be
// refused, naming the missing digest so the client knows what to push.
func TestManifestReferencingMissingBlobIsRejected(t *testing.T) {
	h := newHarness(t)
	secret := h.deployToken("builder", "acme", "acme/", authz.RoleContributor)
	c := h.TokenClient(secret)

	image := buildImage(t, []string{"never uploaded"}, nil, nil)
	// Deliberately push only the config, not the layer.
	c.PushBlob("acme/web", image.ConfigBlob)

	resp := c.Put("/v2/acme/web/manifests/v1", bytes.NewReader(image.Manifest),
		map[string]string{"Content-Type": oci.MediaTypeOCIManifest})
	defer resp.Body.Close()
	requireStatus(t, resp, http.StatusNotFound, "pushing a manifest with a missing layer")

	errs := decodeOCIError(t, resp)
	if len(errs) == 0 || errs[0].Code != "MANIFEST_BLOB_UNKNOWN" {
		t.Fatalf("error code = %v, want MANIFEST_BLOB_UNKNOWN", errs)
	}
	detail, _ := json.Marshal(errs[0].Detail)
	if !strings.Contains(string(detail), image.LayerDigests[0]) {
		t.Errorf("error detail %s does not name the missing digest %s", detail, image.LayerDigests[0])
	}
}

// REQ-OCI-01: the server computes the digest and refuses content that does not
// match what the client claimed.
func TestDigestMismatchIsRejected(t *testing.T) {
	h := newHarness(t)
	secret := h.deployToken("builder", "acme", "acme/", authz.RoleContributor)
	c := h.TokenClient(secret)

	startResp := c.Post("/v2/acme/web/blobs/uploads/", nil, nil)
	requireStatus(t, startResp, http.StatusAccepted, "starting an upload")
	location := startResp.Header.Get("Location")
	startResp.Body.Close()

	// Claim the digest of one payload while uploading a different one.
	claimed := oci.FromBytes([]byte("the expected content")).String()
	resp := c.Put(appendQuery(location, "digest", claimed),
		bytes.NewReader([]byte("something else entirely")), nil)
	defer resp.Body.Close()
	requireStatus(t, resp, http.StatusBadRequest, "completing an upload with a mismatched digest")

	errs := decodeOCIError(t, resp)
	if len(errs) == 0 || errs[0].Code != "DIGEST_INVALID" {
		t.Fatalf("error code = %v, want DIGEST_INVALID", errs)
	}

	// The blob must not exist afterwards.
	check := c.Head("/v2/acme/web/blobs/" + claimed)
	defer check.Body.Close()
	if check.StatusCode == http.StatusOK {
		t.Error("a blob was stored despite the digest mismatch")
	}
}

// REQ-OCI-07: chunked upload range semantics, including the 416 on a
// misaligned chunk. This is a classic interoperability bug.
func TestChunkedUploadRangeSemantics(t *testing.T) {
	h := newHarness(t)
	secret := h.deployToken("builder", "acme", "acme/", authz.RoleContributor)
	c := h.TokenClient(secret)

	// Offsets are derived from the payload rather than hardcoded. They were
	// literals once, and renaming the project changed the repeated string's
	// length — which broke the test without anything about the registry changing.
	content := []byte(strings.Repeat("mantle", 200))
	total := len(content)
	split := total * 2 / 5 // an arbitrary but reproducible chunk boundary
	digest := oci.FromBytes(content).String()

	startResp := c.Post("/v2/acme/web/blobs/uploads/", nil, nil)
	requireStatus(t, startResp, http.StatusAccepted, "starting an upload")
	location := startResp.Header.Get("Location")
	if got := startResp.Header.Get("Range"); got != "0-0" {
		t.Errorf("Range after upload creation = %q, want 0-0", got)
	}
	if startResp.Header.Get("Docker-Upload-UUID") == "" {
		t.Error("upload creation returned no Docker-Upload-UUID")
	}
	startResp.Body.Close()

	// First chunk.
	firstRange := fmt.Sprintf("0-%d", split-1)
	first := c.Patch(location, bytes.NewReader(content[:split]), map[string]string{
		"Content-Range": firstRange,
		"Content-Type":  "application/octet-stream",
	})
	requireStatus(t, first, http.StatusAccepted, "first chunk")
	// Inclusive and zero-indexed: after n bytes the range ends at n-1.
	if got := first.Header.Get("Range"); got != firstRange {
		t.Errorf("Range after %d bytes = %q, want %s", split, got, firstRange)
	}
	first.Body.Close()

	// A chunk starting at the wrong offset must be refused with 416 and the
	// server's actual range, so the client can resynchronise.
	misaligned := c.Patch(location, bytes.NewReader(content[split+100:split+200]), map[string]string{
		"Content-Range": fmt.Sprintf("%d-%d", split+100, split+199),
	})
	requireStatus(t, misaligned, http.StatusRequestedRangeNotSatisfiable, "misaligned chunk")
	if got := misaligned.Header.Get("Range"); got != firstRange {
		t.Errorf("Range on 416 = %q, want %s (the server's true offset)", got, firstRange)
	}
	misaligned.Body.Close()

	// Correctly aligned continuation.
	fullRange := fmt.Sprintf("0-%d", total-1)
	second := c.Patch(location, bytes.NewReader(content[split:]), map[string]string{
		"Content-Range": fmt.Sprintf("%d-%d", split, total-1),
	})
	requireStatus(t, second, http.StatusAccepted, "second chunk")
	if got := second.Header.Get("Range"); got != fullRange {
		t.Errorf("Range after %d bytes = %q, want %s", total, got, fullRange)
	}
	second.Body.Close()

	// end-13: status of an in-progress upload.
	status := c.Get(location)
	requireStatus(t, status, http.StatusNoContent, "upload status")
	if got := status.Header.Get("Range"); got != fullRange {
		t.Errorf("Range from upload status = %q, want %s", got, fullRange)
	}
	status.Body.Close()

	// Complete with no further body.
	complete := c.Put(appendQuery(location, "digest", digest), nil, nil)
	requireStatus(t, complete, http.StatusCreated, "completing the upload")
	if got := complete.Header.Get("Docker-Content-Digest"); got != digest {
		t.Errorf("Docker-Content-Digest = %q, want %q", got, digest)
	}
	complete.Body.Close()

	// The reassembled blob must be exactly what was sent.
	fetch := c.Get("/v2/acme/web/blobs/" + digest)
	defer fetch.Body.Close()
	requireStatus(t, fetch, http.StatusOK, "fetching the reassembled blob")
	fetched := make([]byte, len(content)+16)
	n, _ := fetch.Body.Read(fetched)
	if !bytes.Equal(fetched[:n], content) {
		t.Error("the reassembled blob does not match the uploaded content")
	}
}

// end-4b: a monolithic upload completes in a single POST.
func TestMonolithicUpload(t *testing.T) {
	h := newHarness(t)
	secret := h.deployToken("builder", "acme", "acme/", authz.RoleContributor)
	c := h.TokenClient(secret)

	content := []byte("a whole blob in one request")
	digest := oci.FromBytes(content).String()

	resp := c.Post("/v2/acme/web/blobs/uploads/?digest="+digest,
		bytes.NewReader(content), map[string]string{"Content-Type": "application/octet-stream"})
	defer resp.Body.Close()
	requireStatus(t, resp, http.StatusCreated, "monolithic upload")

	check := c.Head("/v2/acme/web/blobs/" + digest)
	defer check.Body.Close()
	requireStatus(t, check, http.StatusOK, "the monolithically uploaded blob")
}

// end-1: the version endpoint, which is what `docker login` probes.
func TestBaseEndpoint(t *testing.T) {
	h := newHarness(t)
	secret := h.deployToken("reader", "acme", "acme/", authz.RoleReader)

	// Anonymous access is off by default, so the base endpoint must challenge.
	anon := h.Anonymous().Get("/v2/")
	defer anon.Body.Close()
	if anon.StatusCode != http.StatusUnauthorized {
		t.Errorf("anonymous GET /v2/ = %d, want 401", anon.StatusCode)
	}
	if anon.Header.Get("WWW-Authenticate") == "" {
		t.Error("401 from /v2/ carried no WWW-Authenticate challenge")
	}

	authed := h.TokenClient(secret).Get("/v2/")
	defer authed.Body.Close()
	requireStatus(t, authed, http.StatusOK, "authenticated GET /v2/")
	if got := authed.Header.Get("Docker-Distribution-API-Version"); got != "registry/2.0" {
		t.Errorf("Docker-Distribution-API-Version = %q, want registry/2.0", got)
	}
}

// REQ-OCI-08: tags/list paginates lexically and emits a Link header.
func TestTagPagination(t *testing.T) {
	h := newHarness(t)
	secret := h.deployToken("builder", "acme", "acme/", authz.RoleContributor)
	c := h.TokenClient(secret)

	names := []string{"alpha", "beta", "gamma", "delta"}
	for i, name := range names {
		image := buildImage(t, []string{fmt.Sprintf("layer %d", i)}, nil, nil)
		c.PushImage("acme/web", name, image)
	}

	resp := c.Get("/v2/acme/web/tags/list?n=2")
	defer resp.Body.Close()
	requireStatus(t, resp, http.StatusOK, "first page of tags")

	var page struct {
		Tags []string `json:"tags"`
	}
	json.NewDecoder(resp.Body).Decode(&page)
	if len(page.Tags) != 2 {
		t.Fatalf("first page has %d tags, want 2", len(page.Tags))
	}
	// Lexical order, not insertion order.
	if page.Tags[0] != "alpha" || page.Tags[1] != "beta" {
		t.Errorf("first page = %v, want [alpha beta]", page.Tags)
	}
	if link := resp.Header.Get("Link"); !strings.Contains(link, `rel="next"`) {
		t.Errorf("Link header = %q, want a rel=\"next\" link", link)
	}

	// n=0 returns nothing, not everything.
	empty := c.Get("/v2/acme/web/tags/list?n=0")
	defer empty.Body.Close()
	var emptyPage struct {
		Tags []string `json:"tags"`
	}
	json.NewDecoder(empty.Body).Decode(&emptyPage)
	if len(emptyPage.Tags) != 0 {
		t.Errorf("n=0 returned %d tags, want 0", len(emptyPage.Tags))
	}
}

// A multi-architecture index and its children must round-trip.
func TestImageIndex(t *testing.T) {
	h := newHarness(t)
	secret := h.deployToken("builder", "acme", "acme/", authz.RoleContributor)
	c := h.TokenClient(secret)

	amd64 := buildImage(t, []string{"amd64 layer"}, nil, nil)
	arm64 := buildImage(t, []string{"arm64 layer"}, nil, nil)
	c.PushImage("acme/web", amd64.ManifestDigest, amd64)
	c.PushImage("acme/web", arm64.ManifestDigest, arm64)

	index := map[string]any{
		"schemaVersion": 2,
		"mediaType":     oci.MediaTypeOCIIndex,
		"manifests": []map[string]any{
			{
				"mediaType": oci.MediaTypeOCIManifest,
				"digest":    amd64.ManifestDigest,
				"size":      len(amd64.Manifest),
				"platform":  map[string]string{"os": "linux", "architecture": "amd64"},
			},
			{
				"mediaType": oci.MediaTypeOCIManifest,
				"digest":    arm64.ManifestDigest,
				"size":      len(arm64.Manifest),
				"platform":  map[string]string{"os": "linux", "architecture": "arm64"},
			},
		},
	}
	payload, err := json.Marshal(index)
	if err != nil {
		t.Fatal(err)
	}

	digest := c.PushManifest("acme/web", "multiarch", payload, oci.MediaTypeOCIIndex)
	if digest != oci.FromBytes(payload).String() {
		t.Errorf("index digest = %q, want %q", digest, oci.FromBytes(payload).String())
	}

	resp := c.Get("/v2/acme/web/manifests/multiarch")
	defer resp.Body.Close()
	requireStatus(t, resp, http.StatusOK, "getting the index")
	if ct := resp.Header.Get("Content-Type"); ct != oci.MediaTypeOCIIndex {
		t.Errorf("index Content-Type = %q, want %q", ct, oci.MediaTypeOCIIndex)
	}
}

// An index naming a child that was never pushed must be refused, exactly as an
// image manifest naming a missing layer is.
func TestIndexWithMissingChildIsRejected(t *testing.T) {
	h := newHarness(t)
	secret := h.deployToken("builder", "acme", "acme/", authz.RoleContributor)
	c := h.TokenClient(secret)

	absent := buildImage(t, []string{"never pushed"}, nil, nil)
	index := map[string]any{
		"schemaVersion": 2,
		"mediaType":     oci.MediaTypeOCIIndex,
		"manifests": []map[string]any{{
			"mediaType": oci.MediaTypeOCIManifest,
			"digest":    absent.ManifestDigest,
			"size":      len(absent.Manifest),
		}},
	}
	payload, _ := json.Marshal(index)

	resp := c.Put("/v2/acme/web/manifests/broken", bytes.NewReader(payload),
		map[string]string{"Content-Type": oci.MediaTypeOCIIndex})
	defer resp.Body.Close()
	requireStatus(t, resp, http.StatusNotFound, "pushing an index with a missing child")
}

// D-05: Docker schema 1 is not supported, and the refusal names the format
// rather than failing obscurely.
func TestSchema1IsRejectedWithAClearMessage(t *testing.T) {
	h := newHarness(t)
	secret := h.deployToken("builder", "acme", "acme/", authz.RoleContributor)
	c := h.TokenClient(secret)

	resp := c.Put("/v2/acme/web/manifests/old", bytes.NewReader([]byte(`{"schemaVersion":1}`)),
		map[string]string{"Content-Type": "application/vnd.docker.distribution.manifest.v1+prettyjws"})
	defer resp.Body.Close()
	requireStatus(t, resp, http.StatusBadRequest, "pushing a schema 1 manifest")

	errs := decodeOCIError(t, resp)
	if len(errs) == 0 || errs[0].Code != "MANIFEST_INVALID" {
		t.Fatalf("error code = %v, want MANIFEST_INVALID", errs)
	}
	if !strings.Contains(errs[0].Message, "schema 1") {
		t.Errorf("message %q does not name the unsupported format", errs[0].Message)
	}
}

// SEC-01 / REQ-OCI-06: a repository name that violates the grammar is rejected
// before it can reach anything that builds a path.
func TestInvalidRepositoryNames(t *testing.T) {
	h := newHarness(t)
	secret := h.deployToken("builder", "acme", "acme/", authz.RoleContributor)
	c := h.TokenClient(secret)

	for _, name := range []string{
		"Acme/Web",    // uppercase
		"acme/../etc", // traversal
		"acme//web",   // empty component
		"-acme/web",   // leading separator
	} {
		resp := c.Get("/v2/" + name + "/tags/list")
		body := readBody(resp)
		resp.Body.Close()
		// Either NAME_INVALID or a 404 from the router is acceptable; what must
		// never happen is a 200 or a 500.
		if resp.StatusCode == http.StatusOK || resp.StatusCode >= 500 {
			t.Errorf("name %q produced status %d (body %s), want a 4xx rejection",
				name, resp.StatusCode, body)
		}
	}
}
