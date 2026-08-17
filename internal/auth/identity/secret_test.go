package identity

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"strings"
	"testing"
)

// A generated credential must always parse back into the halves it was built
// from.
//
// This is a regression test with a specific history. The selector was once
// base64url-encoded, and base64url's alphabet contains the '_' that separates
// the two halves — so roughly a third of all credentials split at the wrong
// offset and could never authenticate. The failure was intermittent and looked
// like flakiness rather than a bug, which is exactly why the iteration count
// here is high: one round trip would have passed.
func TestGeneratedCredentialsAlwaysRoundTrip(t *testing.T) {
	// The split is what regressed, and it is cheap to exercise, so it gets the
	// iterations. Hashing is what makes generation slow, so the full
	// generate-parse-verify path is checked a handful of times per kind.
	for _, kind := range []Kind{KindPAT, KindDeployToken, KindRobot} {
		prefix, err := PrefixForKind(kind)
		if err != nil {
			t.Fatalf("%s: %v", kind, err)
		}

		for i := 0; i < 2000; i++ {
			selectorRaw := make([]byte, selectorBytes)
			verifierRaw := make([]byte, verifierBytes)
			if _, err := rand.Read(selectorRaw); err != nil {
				t.Fatal(err)
			}
			if _, err := rand.Read(verifierRaw); err != nil {
				t.Fatal(err)
			}
			selector := hex.EncodeToString(selectorRaw)
			verifier := base64.RawURLEncoding.EncodeToString(verifierRaw)

			parsed, ok := ParseCredential(prefix + selector + "_" + verifier)
			if !ok {
				t.Fatalf("%s: credential with selector %q did not parse", kind, selector)
			}
			if parsed.Selector != selector {
				t.Fatalf("%s: parsed selector %q, want %q", kind, parsed.Selector, selector)
			}
			if parsed.Verifier != verifier {
				t.Fatalf("%s: parsed verifier %q, want %q", kind, parsed.Verifier, verifier)
			}
		}

		for i := 0; i < 3; i++ {
			credential, err := GenerateCredential(kind)
			if err != nil {
				t.Fatalf("%s: generating credential: %v", kind, err)
			}
			parsed, ok := ParseCredential(credential.Plaintext)
			if !ok {
				t.Fatalf("%s: credential %q did not parse", kind, credential.Plaintext)
			}
			if parsed.Selector != credential.Selector {
				t.Fatalf("%s: parsed selector %q, want %q", kind, parsed.Selector, credential.Selector)
			}
			if !VerifySecret(parsed.Verifier, credential.Hash) {
				t.Fatalf("%s: the parsed verifier did not match the stored hash", kind)
			}
		}
	}
}

// The prefix is what leaked-credential scanners and the log scrubber match on
// (SEC-12), so each kind must carry its own.
func TestCredentialPrefixes(t *testing.T) {
	for kind, wantPrefix := range map[Kind]string{
		KindPAT:         PrefixPAT,
		KindDeployToken: PrefixDeployToken,
		KindRobot:       PrefixRobot,
	} {
		credential, err := GenerateCredential(kind)
		if err != nil {
			t.Fatalf("%s: %v", kind, err)
		}
		if !strings.HasPrefix(credential.Plaintext, wantPrefix) {
			t.Errorf("%s credential %q does not start with %q", kind, credential.Plaintext, wantPrefix)
		}
		if !LooksLikeCredential(credential.Plaintext) {
			t.Errorf("%s credential is not recognised by the scrubber", kind)
		}
	}

	if _, err := GenerateCredential(KindUser); err == nil {
		t.Error("a generated secret was produced for a user identity, which authenticates by password")
	}
}

func TestParseCredentialRejectsMalformed(t *testing.T) {
	for name, secret := range map[string]string{
		"empty":            "",
		"no prefix":        "abcdef_ghijkl",
		"unknown prefix":   "mantle_xyz_abcdef_ghijkl",
		"no separator":     "mantle_pat_abcdefghijkl",
		"empty selector":   "mantle_pat__verifier",
		"empty verifier":   "mantle_pat_abcdef_",
		"non-hex selector": "mantle_pat_zzzzzz_verifier",
	} {
		if _, ok := ParseCredential(secret); ok {
			t.Errorf("%s: %q parsed but should not have", name, secret)
		}
	}
}

func TestHashSecretRoundTrip(t *testing.T) {
	hash, err := HashSecret("correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	if !VerifySecret("correct horse battery staple", hash) {
		t.Error("the correct secret did not verify")
	}
	if VerifySecret("wrong secret", hash) {
		t.Error("an incorrect secret verified")
	}

	// The same secret must produce different hashes, or the salt is not being
	// applied and identical passwords would be identifiable in a dump.
	other, err := HashSecret("correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	if hash == other {
		t.Error("hashing the same secret twice produced identical output; the salt is not random")
	}
}

func TestVerifySecretRejectsMalformedHashes(t *testing.T) {
	for name, hash := range map[string]string{
		"empty":              "",
		"not a hash":         "hunter2",
		"wrong algorithm":    "$argon2i$v=19$m=19456,t=1,p=1$c2FsdA$a2V5",
		"missing parameters": "$argon2id$v=19$$c2FsdA$a2V5",
		"bad base64":         "$argon2id$v=19$m=19456,t=1,p=1$!!!$!!!",
	} {
		if VerifySecret("anything", hash) {
			t.Errorf("%s: a malformed hash %q verified", name, hash)
		}
	}
}
