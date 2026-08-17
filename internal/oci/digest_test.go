package oci

import (
	"bytes"
	"crypto/rand"
	"testing"
)

// TestVerifierStateSurvivesRoundTrip is the M0 spike from §10.4, kept as a
// permanent test. Resumable uploads across stateless nodes are affordable only
// if the running SHA-256 can be checkpointed and restored in another process;
// if this ever fails, multi-node push degrades to re-reading every prior byte
// on resume and the storage design has to change.
func TestVerifierStateSurvivesRoundTrip(t *testing.T) {
	payload := make([]byte, 1<<20)
	if _, err := rand.Read(payload); err != nil {
		t.Fatalf("generating payload: %v", err)
	}
	want := FromBytes(payload)

	for _, split := range []int{0, 1, 4095, 4096, 65536, len(payload) - 1, len(payload)} {
		v, err := NewVerifier(SHA256)
		if err != nil {
			t.Fatalf("new verifier: %v", err)
		}
		if _, err := v.Write(payload[:split]); err != nil {
			t.Fatalf("write first part: %v", err)
		}

		// Checkpoint, discard the verifier entirely, and resume — this models
		// the client being routed to a different mantled between chunks.
		state, err := v.MarshalState()
		if err != nil {
			t.Fatalf("marshal state at %d: %v", split, err)
		}
		resumed, err := RestoreVerifier(SHA256, state)
		if err != nil {
			t.Fatalf("restore state at %d: %v", split, err)
		}
		if resumed.Written() != int64(split) {
			t.Errorf("split %d: resumed offset = %d, want %d", split, resumed.Written(), split)
		}
		if _, err := resumed.Write(payload[split:]); err != nil {
			t.Fatalf("write second part: %v", err)
		}

		if got := resumed.Digest(); !got.Equal(want) {
			t.Errorf("split %d: digest = %s, want %s", split, got, want)
		}
		if resumed.Written() != int64(len(payload)) {
			t.Errorf("split %d: final offset = %d, want %d", split, resumed.Written(), len(payload))
		}
	}
}

func TestVerifierStateRoundTripsSHA512(t *testing.T) {
	v, err := NewVerifier(SHA512)
	if err != nil {
		t.Fatalf("new verifier: %v", err)
	}
	if _, err := v.Write([]byte("mantle")); err != nil {
		t.Fatal(err)
	}
	state, err := v.MarshalState()
	if err != nil {
		t.Fatalf("marshal sha512 state: %v", err)
	}
	if _, err := RestoreVerifier(SHA512, state); err != nil {
		t.Fatalf("restore sha512 state: %v", err)
	}
}

// A truncated or corrupt state must fail loudly. Silently starting from a fresh
// hash would accept a partial layer under a digest the client chose.
func TestRestoreVerifierRejectsBadState(t *testing.T) {
	v, _ := NewVerifier(SHA256)
	_, _ = v.Write([]byte("mantle"))
	good, err := v.MarshalState()
	if err != nil {
		t.Fatal(err)
	}

	for name, state := range map[string][]byte{
		"empty":     {},
		"too short": good[:4],
		"truncated": good[:len(good)-3],
		"corrupt":   append(bytes.Clone(good[:8]), []byte("not a hash state")...),
	} {
		if _, err := RestoreVerifier(SHA256, state); err == nil {
			t.Errorf("%s state: expected an error, got none", name)
		}
	}
}

func TestParseDigest(t *testing.T) {
	valid := []string{
		"sha256:" + hex64,
		"sha512:" + hex128,
	}
	for _, s := range valid {
		if _, err := ParseDigest(s); err != nil {
			t.Errorf("ParseDigest(%q) = %v, want nil", s, err)
		}
	}

	invalid := map[string]string{
		"empty":            "",
		"no algorithm":     hex64,
		"no value":         "sha256:",
		"short value":      "sha256:abcd",
		"long value":       "sha256:" + hex64 + "ab",
		"uppercase hex":    "sha256:" + "A" + hex64[1:],
		"non-hex":          "sha256:" + "z" + hex64[1:],
		"path traversal":   "sha256:../../../etc/passwd",
		"embedded slash":   "sha256:" + hex64[:32] + "/" + hex64[33:],
		"unknown algo":     "md5:d41d8cd98f00b204e9800998ecf8427e",
		"trailing newline": "sha256:" + hex64 + "\n",
	}
	for name, s := range invalid {
		if _, err := ParseDigest(s); err == nil {
			t.Errorf("%s: ParseDigest(%q) succeeded, want an error", name, s)
		}
	}
}

// A well-formed digest naming an algorithm we do not implement must be
// distinguishable, so the handler can answer UNSUPPORTED rather than
// DIGEST_INVALID (REQ-OCI-01).
func TestParseDigestDistinguishesUnsupportedAlgorithm(t *testing.T) {
	_, err := ParseDigest("md5:d41d8cd98f00b204e9800998ecf8427e")
	if err == nil {
		t.Fatal("expected an error")
	}
	var unsupported *ErrUnsupportedAlgorithm
	if !asErr(err, &unsupported) {
		t.Fatalf("error is %T, want *ErrUnsupportedAlgorithm", err)
	}
	if unsupported.Algorithm != "md5" {
		t.Errorf("Algorithm = %q, want md5", unsupported.Algorithm)
	}
}

func TestDigestEqual(t *testing.T) {
	a := FromBytes([]byte("mantle"))
	b := FromBytes([]byte("mantle"))
	c := FromBytes([]byte("mantled"))
	if !a.Equal(b) {
		t.Error("identical content produced unequal digests")
	}
	if a.Equal(c) {
		t.Error("different content produced equal digests")
	}
	// Same encoded value, different algorithm, must not compare equal.
	sha512Digest := Digest{algorithm: SHA512, encoded: a.Encoded()}
	if a.Equal(sha512Digest) {
		t.Error("digests with different algorithms compared equal")
	}
}

const (
	hex64  = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	hex128 = hex64 + hex64
)

func asErr[T error](err error, target *T) bool {
	for err != nil {
		if t, ok := err.(T); ok {
			*target = t
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}
