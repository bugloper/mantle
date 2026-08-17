package oci

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// Media types Mantle accepts on push and round-trips byte-exactly on pull
// (REQ-OCI-04).
const (
	MediaTypeOCIManifest       = "application/vnd.oci.image.manifest.v1+json"
	MediaTypeOCIIndex          = "application/vnd.oci.image.index.v1+json"
	MediaTypeDockerManifest    = "application/vnd.docker.distribution.manifest.v2+json"
	MediaTypeDockerList        = "application/vnd.docker.distribution.manifest.list.v2+json"
	MediaTypeOCIArtifact       = "application/vnd.oci.artifact.manifest.v1+json"
	MediaTypeOCIEmptyJSON      = "application/vnd.oci.empty.v1+json"
	MediaTypeOCIImageConfig    = "application/vnd.oci.image.config.v1+json"
	MediaTypeDockerImageConfig = "application/vnd.docker.container.image.v1+json"

	// MediaTypeDockerSchema1 is explicitly unsupported (REQ-OCI-04, D-05). It is
	// named here so the rejection message can be specific rather than a generic
	// "unsupported media type".
	MediaTypeDockerSchema1       = "application/vnd.docker.distribution.manifest.v1+json"
	MediaTypeDockerSchema1Signed = "application/vnd.docker.distribution.manifest.v1+prettyjws"
)

// manifestMediaTypes is the set accepted on PUT. A media type absent from this
// set is MANIFEST_INVALID — an allowlist, because the alternative is guessing at
// the shape of a document we are about to store under a content address.
var manifestMediaTypes = map[string]bool{
	MediaTypeOCIManifest:    true,
	MediaTypeOCIIndex:       true,
	MediaTypeDockerManifest: true,
	MediaTypeDockerList:     true,
	MediaTypeOCIArtifact:    true,
}

// IsManifestMediaType reports whether the media type may be stored as a manifest.
func IsManifestMediaType(mt string) bool { return manifestMediaTypes[mt] }

// IsIndexMediaType reports whether the media type describes a manifest of
// manifests, whose children are manifests rather than blobs.
func IsIndexMediaType(mt string) bool {
	return mt == MediaTypeOCIIndex || mt == MediaTypeDockerList
}

// IsSchema1 reports whether the media type is a deprecated Docker schema 1
// manifest, which Mantle declines to support.
func IsSchema1(mt string) bool {
	return mt == MediaTypeDockerSchema1 || mt == MediaTypeDockerSchema1Signed
}

// Descriptor is the {mediaType, digest, size} pointer that ties the whole
// format together.
type Descriptor struct {
	MediaType    string            `json:"mediaType"`
	Digest       string            `json:"digest"`
	Size         int64             `json:"size"`
	URLs         []string          `json:"urls,omitempty"`
	Annotations  map[string]string `json:"annotations,omitempty"`
	Platform     *Platform         `json:"platform,omitempty"`
	ArtifactType string            `json:"artifactType,omitempty"`
}

// Platform identifies the target of a child manifest in an index.
type Platform struct {
	Architecture string   `json:"architecture"`
	OS           string   `json:"os"`
	OSVersion    string   `json:"os.version,omitempty"`
	OSFeatures   []string `json:"os.features,omitempty"`
	Variant      string   `json:"variant,omitempty"`
}

// Manifest is the parsed form of every manifest media type Mantle accepts. Image
// manifests and indexes are unified into one struct because the fields are
// disjoint: an image manifest has config and layers, an index has manifests.
// Parsing into one shape avoids two nearly identical code paths on the hot push
// path, at the cost of a struct with some always-empty fields.
type Manifest struct {
	SchemaVersion int               `json:"schemaVersion"`
	MediaType     string            `json:"mediaType"`
	ArtifactType  string            `json:"artifactType,omitempty"`
	Config        *Descriptor       `json:"config,omitempty"`
	Layers        []Descriptor      `json:"layers,omitempty"`
	Manifests     []Descriptor      `json:"manifests,omitempty"`
	Subject       *Descriptor       `json:"subject,omitempty"`
	Annotations   map[string]string `json:"annotations,omitempty"`
}

// Limits bound what a manifest may contain (SEC-05). They are enforced during
// validation, after a size check but before any recursion, so a manifest bomb
// costs one parse of a bounded document and nothing more.
type Limits struct {
	MaxManifestSize int64
	MaxLayers       int
	MaxIndexDepth   int
	MaxIndexEntries int
}

// DefaultLimits matches the shipped configuration defaults (§17).
var DefaultLimits = Limits{
	MaxManifestSize: 4 << 20, // 4 MiB
	MaxLayers:       200,
	MaxIndexDepth:   3,
	MaxIndexEntries: 256,
}

// ParseManifest decodes manifest bytes and validates their structure.
//
// The declared content type is passed separately because the specification
// permits a manifest to omit mediaType from its body, in which case the
// Content-Type header is authoritative. Where both are present and disagree,
// that is a client bug worth rejecting rather than silently resolving.
func ParseManifest(payload []byte, contentType string, limits Limits) (*Manifest, error) {
	if int64(len(payload)) > limits.MaxManifestSize {
		return nil, fmt.Errorf("manifest is %d bytes, limit is %d", len(payload), limits.MaxManifestSize)
	}
	if len(payload) == 0 {
		return nil, fmt.Errorf("manifest is empty")
	}
	if IsSchema1(contentType) {
		return nil, fmt.Errorf("Docker image manifest schema 1 (%s) is not supported; "+
			"rebuild the image with a current builder to produce a schema 2 or OCI manifest", contentType)
	}

	// Unknown fields are deliberately permitted. The format is extensible and a
	// field added by a future specification revision must not make an image
	// unpushable — we store the bytes verbatim regardless of what we understood.
	var m Manifest
	dec := json.NewDecoder(bytes.NewReader(payload))
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("manifest is not valid JSON: %w", err)
	}
	if dec.More() {
		return nil, fmt.Errorf("manifest has trailing content after the JSON document")
	}

	// Resolve the effective media type. Body wins when present, because that is
	// what the digest covers and therefore what other registries will see.
	effective := m.MediaType
	if effective == "" {
		effective = contentType
		m.MediaType = contentType
	} else if contentType != "" && contentType != effective {
		return nil, fmt.Errorf("Content-Type %q disagrees with the manifest's own mediaType %q",
			contentType, effective)
	}
	if effective == "" {
		return nil, fmt.Errorf("manifest declares no mediaType and none was supplied in Content-Type")
	}
	if IsSchema1(effective) {
		return nil, fmt.Errorf("Docker image manifest schema 1 (%s) is not supported; "+
			"rebuild the image with a current builder to produce a schema 2 or OCI manifest", effective)
	}
	if !IsManifestMediaType(effective) {
		return nil, fmt.Errorf("media type %q is not a manifest media type", effective)
	}

	if err := m.validate(limits); err != nil {
		return nil, err
	}
	return &m, nil
}

func (m *Manifest) validate(limits Limits) error {
	isIndex := IsIndexMediaType(m.MediaType)

	if isIndex {
		if len(m.Manifests) > limits.MaxIndexEntries {
			return fmt.Errorf("index has %d entries, limit is %d", len(m.Manifests), limits.MaxIndexEntries)
		}
		if m.Config != nil || len(m.Layers) > 0 {
			return fmt.Errorf("index must not declare config or layers")
		}
		for i, d := range m.Manifests {
			if err := d.validate(); err != nil {
				return fmt.Errorf("index entry %d: %w", i, err)
			}
		}
	} else {
		if len(m.Layers) > limits.MaxLayers {
			return fmt.Errorf("manifest has %d layers, limit is %d", len(m.Layers), limits.MaxLayers)
		}
		if len(m.Manifests) > 0 {
			return fmt.Errorf("image manifest must not declare a manifests array")
		}
		// An artifact manifest may legitimately carry no config, and the OCI
		// image manifest permits an empty-JSON config descriptor. Both are
		// accepted; only a malformed descriptor is rejected.
		if m.Config != nil {
			if err := m.Config.validate(); err != nil {
				return fmt.Errorf("config descriptor: %w", err)
			}
		}
		for i, d := range m.Layers {
			if err := d.validate(); err != nil {
				return fmt.Errorf("layer %d: %w", i, err)
			}
		}
	}

	if m.Subject != nil {
		if err := m.Subject.validate(); err != nil {
			return fmt.Errorf("subject descriptor: %w", err)
		}
	}
	return nil
}

func (d *Descriptor) validate() error {
	if d.Digest == "" {
		return fmt.Errorf("descriptor has no digest")
	}
	if _, err := ParseDigest(d.Digest); err != nil {
		return fmt.Errorf("descriptor digest: %w", err)
	}
	if d.Size < 0 {
		return fmt.Errorf("descriptor size is negative")
	}
	if d.MediaType == "" {
		return fmt.Errorf("descriptor for %s has no mediaType", d.Digest)
	}
	return nil
}

// BlobReferences returns the descriptors an image manifest expects to find in
// the blob store: its config, if any, and its layers. For an index it returns
// nothing, because an index's children are manifests, not blobs.
//
// Layers carrying a urls field are foreign or non-distributable and are
// excluded: the registry is not expected to hold them, and requiring their
// presence would reject legitimate Windows base images (REQ-OCI-05).
func (m *Manifest) BlobReferences() []Descriptor {
	if IsIndexMediaType(m.MediaType) {
		return nil
	}
	refs := make([]Descriptor, 0, len(m.Layers)+1)
	if m.Config != nil {
		refs = append(refs, *m.Config)
	}
	for _, l := range m.Layers {
		if len(l.URLs) > 0 {
			continue
		}
		refs = append(refs, l)
	}
	return refs
}

// ChildReferences returns the child manifest descriptors of an index.
func (m *Manifest) ChildReferences() []Descriptor {
	if !IsIndexMediaType(m.MediaType) {
		return nil
	}
	return m.Manifests
}

// SubjectDigest returns the digest this manifest is a referrer of, or the empty
// string if it stands alone.
func (m *Manifest) SubjectDigest() string {
	if m.Subject == nil {
		return ""
	}
	return m.Subject.Digest
}

// EffectiveArtifactType is the value the referrers API reports for this
// manifest. The specification prefers the artifactType field and falls back to
// the config descriptor's media type for image manifests that predate it.
func (m *Manifest) EffectiveArtifactType() string {
	if m.ArtifactType != "" {
		return m.ArtifactType
	}
	if m.Config != nil && m.Config.MediaType != MediaTypeOCIEmptyJSON {
		return m.Config.MediaType
	}
	return ""
}
