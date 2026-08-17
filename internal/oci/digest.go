package oci

import (
	"crypto/sha256"
	"crypto/sha512"
	"crypto/subtle"
	"encoding"
	"fmt"
	"hash"
	"regexp"
	"strings"
)

// Algorithm is a digest algorithm name.
type Algorithm string

const (
	// SHA256 is required by the specification and is what every client emits.
	SHA256 Algorithm = "sha256"
	// SHA512 is accepted on push and served on pull, but never chosen by us.
	SHA512 Algorithm = "sha512"
)

// hexLength is the expected encoded length for each supported algorithm. Any
// algorithm absent from this map is rejected with UNSUPPORTED (REQ-OCI-01) —
// the map is the allowlist, not a lookup optimisation.
var hexLength = map[Algorithm]int{
	SHA256: sha256.Size * 2,
	SHA512: sha512.Size * 2,
}

// New returns a running hash for the algorithm.
func (a Algorithm) New() (hash.Hash, error) {
	switch a {
	case SHA256:
		return sha256.New(), nil
	case SHA512:
		return sha512.New(), nil
	default:
		return nil, fmt.Errorf("unsupported digest algorithm %q", a)
	}
}

// Available reports whether the algorithm is one Mantle accepts.
func (a Algorithm) Available() bool {
	_, ok := hexLength[a]
	return ok
}

// Digest is a validated content address in its canonical "algorithm:hex" form.
// The zero value is invalid; construct one only through ParseDigest or
// FromBytes so that an unvalidated string can never be used as an address.
type Digest struct {
	algorithm Algorithm
	encoded   string
}

var digestRe = regexp.MustCompile(`^[a-z0-9]+(?:[.+_-][a-z0-9]+)*:[a-zA-Z0-9=_-]+$`)

// ParseDigest validates and parses a digest string.
//
// Two distinct failures are worth distinguishing to the caller: a malformed
// digest is DIGEST_INVALID, whereas a well-formed digest naming an algorithm we
// do not implement is UNSUPPORTED. ErrUnsupportedAlgorithm carries that apart.
func ParseDigest(s string) (Digest, error) {
	if !digestRe.MatchString(s) {
		return Digest{}, fmt.Errorf("digest %q is malformed: expected 'algorithm:hex'", s)
	}
	algo, encoded, _ := strings.Cut(s, ":")
	alg := Algorithm(algo)
	want, ok := hexLength[alg]
	if !ok {
		return Digest{}, &ErrUnsupportedAlgorithm{Algorithm: algo}
	}
	if len(encoded) != want {
		return Digest{}, fmt.Errorf("digest %q has a %d-character value, %s requires %d",
			s, len(encoded), algo, want)
	}
	// The generic digest grammar permits base64url characters, but every
	// algorithm we support is hex. Reject the rest here rather than letting a
	// non-hex value reach a storage path.
	for i := 0; i < len(encoded); i++ {
		c := encoded[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return Digest{}, fmt.Errorf("digest %q contains a non-lowercase-hex character at offset %d", s, i)
		}
	}
	return Digest{algorithm: alg, encoded: encoded}, nil
}

// ErrUnsupportedAlgorithm is returned for a well-formed digest naming an
// algorithm Mantle does not implement, so the handler can answer UNSUPPORTED
// rather than DIGEST_INVALID.
type ErrUnsupportedAlgorithm struct{ Algorithm string }

func (e *ErrUnsupportedAlgorithm) Error() string {
	return fmt.Sprintf("unsupported digest algorithm %q: supported algorithms are sha256 and sha512", e.Algorithm)
}

// MustParseDigest is ParseDigest for constants and tests. It panics.
func MustParseDigest(s string) Digest {
	d, err := ParseDigest(s)
	if err != nil {
		panic(err)
	}
	return d
}

// FromBytes computes the SHA-256 digest of b.
func FromBytes(b []byte) Digest {
	sum := sha256.Sum256(b)
	return Digest{algorithm: SHA256, encoded: fmt.Sprintf("%x", sum)}
}

func (d Digest) String() string {
	if d.encoded == "" {
		return ""
	}
	return string(d.algorithm) + ":" + d.encoded
}

// Algorithm returns the digest algorithm.
func (d Digest) Algorithm() Algorithm { return d.algorithm }

// Encoded returns the hex portion without the algorithm prefix.
func (d Digest) Encoded() string { return d.encoded }

// Valid reports whether the digest was constructed through validation.
func (d Digest) Valid() bool { return d.encoded != "" }

// Prefix returns the two-character shard directory used by the storage layout
// (§10.2). Sharding keeps directory entry counts sane on ext4 and XFS.
func (d Digest) Prefix() string { return d.encoded[:2] }

// Equal compares two digests in constant time (REQ-OCI-01).
//
// Constant time matters here despite the values being public: the comparison
// gates whether uploaded content is accepted under a client-chosen address, and
// a timing oracle on that comparison is a content-substitution primitive. It
// costs nothing to be careful.
func (d Digest) Equal(other Digest) bool {
	if d.algorithm != other.algorithm {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(d.encoded), []byte(other.encoded)) == 1
}

// Verifier computes a digest over a stream and compares it to an expected
// value. Its state is checkpointable, which is what makes a resumable upload
// across stateless nodes affordable (§10.4): without it, resuming a 2 GB layer
// means re-reading 2 GB to rebuild the running hash.
type Verifier struct {
	algorithm Algorithm
	hash      hash.Hash
	written   int64
}

// NewVerifier starts a verifier for the given algorithm.
func NewVerifier(algorithm Algorithm) (*Verifier, error) {
	h, err := algorithm.New()
	if err != nil {
		return nil, err
	}
	return &Verifier{algorithm: algorithm, hash: h}, nil
}

// Write feeds bytes into the running hash. It never returns an error, matching
// the hash.Hash contract, so a Verifier is usable as an io.Writer in a MultiWriter.
func (v *Verifier) Write(p []byte) (int, error) {
	n, err := v.hash.Write(p)
	v.written += int64(n)
	return n, err
}

// Written reports how many bytes have passed through the verifier.
func (v *Verifier) Written() int64 { return v.written }

// Digest returns the digest of everything written so far, without ending the
// stream — the underlying hashes support this and uploads rely on it.
func (v *Verifier) Digest() Digest {
	return Digest{algorithm: v.algorithm, encoded: fmt.Sprintf("%x", v.hash.Sum(nil))}
}

// Verify compares the computed digest to expected in constant time.
func (v *Verifier) Verify(expected Digest) bool { return v.Digest().Equal(expected) }

// MarshalState serialises the running hash so it can be resumed in another
// process. Go's SHA-2 implementations implement encoding.BinaryMarshaler; the
// type assertion is checked rather than assumed because a future standard
// library change that dropped it would otherwise fail as a silent panic on the
// push path.
func (v *Verifier) MarshalState() ([]byte, error) {
	m, ok := v.hash.(encoding.BinaryMarshaler)
	if !ok {
		return nil, fmt.Errorf("hash for %s does not support state checkpointing", v.algorithm)
	}
	state, err := m.MarshalBinary()
	if err != nil {
		return nil, fmt.Errorf("checkpointing %s state: %w", v.algorithm, err)
	}
	// The byte count is not part of the hash state, so it is prefixed here. An
	// upload that resumed with the right hash but the wrong offset would accept
	// a truncated layer under a valid-looking digest.
	out := make([]byte, 8, 8+len(state))
	putInt64(out, v.written)
	return append(out, state...), nil
}

// RestoreVerifier rebuilds a verifier from MarshalState output.
func RestoreVerifier(algorithm Algorithm, state []byte) (*Verifier, error) {
	if len(state) < 8 {
		return nil, fmt.Errorf("hash state is %d bytes, too short to be valid", len(state))
	}
	h, err := algorithm.New()
	if err != nil {
		return nil, err
	}
	u, ok := h.(encoding.BinaryUnmarshaler)
	if !ok {
		return nil, fmt.Errorf("hash for %s does not support state restoration", algorithm)
	}
	if err := u.UnmarshalBinary(state[8:]); err != nil {
		return nil, fmt.Errorf("restoring %s state: %w", algorithm, err)
	}
	return &Verifier{algorithm: algorithm, hash: h, written: getInt64(state)}, nil
}

func putInt64(b []byte, v int64) {
	for i := 7; i >= 0; i-- {
		b[i] = byte(v)
		v >>= 8
	}
}

func getInt64(b []byte) int64 {
	var v int64
	for i := 0; i < 8; i++ {
		v = v<<8 | int64(b[i])
	}
	return v
}
