package ledger

import (
	"encoding/json"
	"regexp"
	"strings"
	"time"
)

// Standard OCI annotation keys Mantle reads for Tier 0 provenance (§13.2).
const (
	AnnotationRevision    = "org.opencontainers.image.revision"
	AnnotationSource      = "org.opencontainers.image.source"
	AnnotationCreated     = "org.opencontainers.image.created"
	AnnotationVersion     = "org.opencontainers.image.version"
	AnnotationTitle       = "org.opencontainers.image.title"
	AnnotationDescription = "org.opencontainers.image.description"
)

// Source records how a provenance fact was obtained, so the interface can show
// the difference between a fact and a guess — and so a wrong guess is
// diagnosable rather than mysterious (§13.2).
type Source string

const (
	// SourceAnnotation is a manifest annotation: authoritative.
	SourceAnnotation Source = "annotation"
	// SourceLabel is an image config label: authoritative.
	SourceLabel Source = "label"
	// SourceTagPattern is inferred from the tag's shape: a guess.
	SourceTagPattern Source = "tag_pattern"
	// SourceReported came from a deploy report.
	SourceReported Source = "reported"
)

// Confidence qualifies an inferred fact.
type Confidence string

const (
	ConfidenceCertain  Confidence = "certain"
	ConfidenceProbable Confidence = "probable"
)

// Provenance is what Mantle learned about where an image came from.
type Provenance struct {
	CommitSHA   string
	SourceURL   string
	BuiltAt     *time.Time
	Version     string
	Title       string
	Description string
	Source      Source
	Confidence  Confidence
	// Raw keeps every annotation and label that contributed, so a wrong
	// inference can be diagnosed without re-pulling the image.
	Raw map[string]string
}

// Empty reports whether nothing useful was found.
func (p *Provenance) Empty() bool {
	return p.CommitSHA == "" && p.SourceURL == "" && p.BuiltAt == nil && p.Version == ""
}

// imageConfig is the subset of an OCI image config Mantle reads.
//
// Mantle reads config metadata and treats layer bytes as opaque (§3.5, SEC-06).
// This struct is the boundary of that: labels and a creation timestamp, nothing
// that requires decompressing anything.
type imageConfig struct {
	Created *time.Time `json:"created"`
	Config  struct {
		Labels map[string]string `json:"Labels"`
	} `json:"config"`
	// Docker writes the same data under a lowercase key in some versions.
	ContainerConfig struct {
		Labels map[string]string `json:"Labels"`
	} `json:"container_config"`
}

// ExtractProvenance derives provenance from a manifest's annotations, its image
// config labels, and finally the tag's shape.
//
// The order is a precedence order, strongest evidence first. Annotations and
// labels are statements the builder made; a tag pattern is an inference from
// what the tag looks like, and it is recorded as probable so nothing downstream
// mistakes it for a fact.
func ExtractProvenance(annotations map[string]string, configBlob []byte, tag string) *Provenance {
	p := &Provenance{Raw: map[string]string{}}

	// --- annotations ---
	if len(annotations) > 0 {
		p.absorb(annotations, SourceAnnotation)
	}

	// --- image config labels ---
	if len(configBlob) > 0 {
		var config imageConfig
		if err := json.Unmarshal(configBlob, &config); err == nil {
			labels := config.Config.Labels
			if len(labels) == 0 {
				labels = config.ContainerConfig.Labels
			}
			if len(labels) > 0 {
				p.absorb(labels, SourceLabel)
			}
			if p.BuiltAt == nil && config.Created != nil && !config.Created.IsZero() {
				p.BuiltAt = config.Created
				if p.Source == "" {
					p.Source = SourceLabel
				}
			}
		}
	}

	// --- tag shape, last resort ---
	if p.CommitSHA == "" && tag != "" {
		if sha, ok := commitFromTag(tag); ok {
			p.CommitSHA = sha
			p.Source = SourceTagPattern
			p.Confidence = ConfidenceProbable
			p.Raw["inferred_from_tag"] = tag
		}
	}

	if p.Source == "" {
		p.Source = SourceAnnotation
	}
	if p.Confidence == "" {
		p.Confidence = ConfidenceCertain
	}
	return p
}

// absorb copies recognised keys from an annotation or label map.
func (p *Provenance) absorb(values map[string]string, source Source) {
	set := func(target *string, key string) {
		if *target != "" {
			return
		}
		v := strings.TrimSpace(values[key])
		if v == "" {
			return
		}
		*target = v
		p.Raw[key] = v
		if p.Source == "" {
			p.Source = source
		}
	}

	set(&p.CommitSHA, AnnotationRevision)
	set(&p.SourceURL, AnnotationSource)
	set(&p.Version, AnnotationVersion)
	set(&p.Title, AnnotationTitle)
	set(&p.Description, AnnotationDescription)

	if p.BuiltAt == nil {
		if raw := strings.TrimSpace(values[AnnotationCreated]); raw != "" {
			if parsed, err := time.Parse(time.RFC3339, raw); err == nil {
				p.BuiltAt = &parsed
				p.Raw[AnnotationCreated] = raw
				if p.Source == "" {
					p.Source = source
				}
			}
		}
	}

	// A commit SHA that is not hex is not a commit SHA. Builders occasionally
	// write a branch name into the revision annotation, and storing that as a
	// commit would produce a ledger entry that links to nothing.
	if p.CommitSHA != "" && !looksLikeCommitSHA(p.CommitSHA) {
		p.Raw["rejected_revision"] = p.CommitSHA
		p.CommitSHA = ""
	}
}

var (
	hexOnly = regexp.MustCompile(`^[0-9a-f]{7,40}$`)
	// Common CI tag conventions (§13.2): sha-<hex>, <branch>-<hex>,
	// <branch>-<yyyymmdd>. Only the hex forms yield a commit.
	shaPrefixTag = regexp.MustCompile(`^(?:sha|commit|git)[-_]([0-9a-f]{7,40})$`)
	suffixHexTag = regexp.MustCompile(`^.+[-_]([0-9a-f]{7,40})$`)
)

// looksLikeCommitSHA reports whether a string is plausibly a git object id.
func looksLikeCommitSHA(s string) bool {
	return hexOnly.MatchString(strings.ToLower(s))
}

// commitFromTag infers a commit from a tag's shape.
//
// This is deliberately conservative. A tag like "v1.2.3" or "2026081" is not a
// commit even though it is short and partly numeric, and a false positive here
// puts a wrong commit link in front of someone during an incident — worse than
// showing nothing.
func commitFromTag(tag string) (string, bool) {
	lower := strings.ToLower(tag)

	if m := shaPrefixTag.FindStringSubmatch(lower); m != nil {
		return m[1], true
	}

	// A bare hex string, but only if it is long enough to be unambiguous and
	// not all digits — "1234567" is far more likely a build number than a
	// commit, and an all-digit tag is a date or a counter in practice.
	if hexOnly.MatchString(lower) && containsHexLetter(lower) {
		return lower, true
	}

	if m := suffixHexTag.FindStringSubmatch(lower); m != nil && containsHexLetter(m[1]) {
		return m[1], true
	}
	return "", false
}

func containsHexLetter(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 'a' && s[i] <= 'f' {
			return true
		}
	}
	return false
}
