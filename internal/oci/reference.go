// Package oci holds the primitive value types of the OCI Distribution
// Specification: repository names, tags, and digests.
//
// It is a leaf package with no dependencies outside the standard library, so
// every layer may import it. Validation lives here rather than in the HTTP
// handlers because a name that reaches a storage path builder unvalidated is
// SEC-01, and the only reliable defence is that there is exactly one place a
// name can be constructed from.
package oci

import (
	"fmt"
	"regexp"
	"strings"
)

// MaxNameLength is the total repository name limit from the specification.
const MaxNameLength = 255

// MaxTagLength is the tag limit: 128 characters including the leading one.
const MaxTagLength = 128

// nameRe is the repository name grammar (REQ-OCI-06), transcribed from the OCI
// Distribution Specification:
//
//	[a-z0-9]+((\.|_|__|-+)[a-z0-9]+)*(/[a-z0-9]+((\.|_|__|-+)[a-z0-9]+)*)*
//
// Note what it excludes by construction: uppercase, leading and trailing
// separators, empty path components, and therefore "." and ".." components. The
// explicit checks in ValidateName are belt-and-braces for the traversal case,
// because being wrong about that one is a filesystem escape.
var nameRe = regexp.MustCompile(`^[a-z0-9]+(?:(?:\.|_|__|-+)[a-z0-9]+)*(?:/[a-z0-9]+(?:(?:\.|_|__|-+)[a-z0-9]+)*)*$`)

// tagRe is the tag grammar: an alphanumeric or underscore, then up to 127 more
// characters from a slightly wider set.
var tagRe = regexp.MustCompile(`^[a-zA-Z0-9_][a-zA-Z0-9._-]{0,127}$`)

// ValidateName checks a repository name against the specification grammar and
// the additional hardening rules in REQ-OCI-06. The returned error is suitable
// for the detail field of a NAME_INVALID response.
func ValidateName(name string) error {
	if name == "" {
		return fmt.Errorf("name is empty")
	}
	if len(name) > MaxNameLength {
		return fmt.Errorf("name is %d bytes, limit is %d", len(name), MaxNameLength)
	}
	// Reject non-ASCII before the regex so the message names the real problem.
	// A Unicode name that normalises onto an existing ASCII name is a spoofing
	// vector, and no OCI client emits one.
	for i := 0; i < len(name); i++ {
		if name[i] >= 0x80 {
			return fmt.Errorf("name contains a non-ASCII byte at offset %d", i)
		}
	}
	if strings.HasPrefix(name, "/") || strings.HasSuffix(name, "/") {
		return fmt.Errorf("name has a leading or trailing '/'")
	}
	for _, component := range strings.Split(name, "/") {
		if component == "." || component == ".." {
			return fmt.Errorf("name contains a %q path component", component)
		}
	}
	if !nameRe.MatchString(name) {
		return fmt.Errorf("name does not match the required grammar " +
			"(lowercase alphanumerics separated by '.', '_', '__' or '-', in '/'-joined components)")
	}
	return nil
}

// ValidateTag checks a tag against the specification grammar.
func ValidateTag(tag string) error {
	if tag == "" {
		return fmt.Errorf("tag is empty")
	}
	if len(tag) > MaxTagLength {
		return fmt.Errorf("tag is %d bytes, limit is %d", len(tag), MaxTagLength)
	}
	if !tagRe.MatchString(tag) {
		return fmt.Errorf("tag must start with an alphanumeric or '_' and contain only alphanumerics, '.', '_' and '-'")
	}
	return nil
}

// IsDigestReference reports whether a manifest reference is a digest rather
// than a tag. The two are distinguished by the presence of the algorithm
// separator, which a valid tag can never contain.
func IsDigestReference(reference string) bool {
	return strings.Contains(reference, ":")
}

// ValidateReference checks a manifest reference, which the specification allows
// to be either a tag or a digest.
func ValidateReference(reference string) error {
	if IsDigestReference(reference) {
		_, err := ParseDigest(reference)
		return err
	}
	return ValidateTag(reference)
}
