package identity

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// Secret prefixes (§9.2). Every machine credential Mantle issues carries one, so
// that leaked-credential scanners can recognise it in a public repository and
// so our own log scrubber can redact it without knowing the value (SEC-12).
const (
	PrefixPAT         = "mantle_pat_"
	PrefixDeployToken = "mantle_dep_"
	PrefixRobot       = "mantle_rob_"
)

// AllPrefixes is the set the log scrubber and the credential parser share.
var AllPrefixes = []string{PrefixPAT, PrefixDeployToken, PrefixRobot}

// PrefixForKind returns the secret prefix for an identity kind.
func PrefixForKind(kind Kind) (string, error) {
	switch kind {
	case KindPAT:
		return PrefixPAT, nil
	case KindDeployToken:
		return PrefixDeployToken, nil
	case KindRobot:
		return PrefixRobot, nil
	default:
		return "", fmt.Errorf("identity kind %q does not use a generated secret", kind)
	}
}

const (
	// selectorBytes is the indexed lookup half of a credential. It is not a
	// secret and does not need to resist offline attack — it only needs to be
	// unique, so 8 bytes is ample.
	selectorBytes = 8
	// verifierBytes is the secret half, compared against an Argon2id hash.
	verifierBytes = 32
)

// argon2id parameters. These follow the OWASP recommendation for interactive
// authentication: 19 MiB of memory, one iteration, one lane. Memory hardness is
// what matters against an attacker with a dump of the identities table, and
// this costs a few milliseconds per login.
const (
	argonTime    uint32 = 1
	argonMemory  uint32 = 19 * 1024
	argonThreads uint8  = 1
	argonKeyLen  uint32 = 32
	argonSaltLen        = 16
)

// Credential is a freshly generated secret, in the two forms the system needs:
// the plaintext to show the user exactly once, and the selector and hash to
// store.
type Credential struct {
	// Plaintext is shown once at creation and never recoverable afterwards.
	Plaintext string
	Selector  string
	Hash      string
}

// GenerateCredential mints a machine credential of the given kind.
//
// The secret has two parts. The selector is stored in plaintext and indexed, so
// authentication is one indexed lookup followed by exactly one Argon2id
// evaluation. Hashing a presented secret against every row instead would make
// every failed login cost the whole table — slow enough to be a denial-of-
// service primitive by the time an instance has a few hundred credentials.
func GenerateCredential(kind Kind) (*Credential, error) {
	prefix, err := PrefixForKind(kind)
	if err != nil {
		return nil, err
	}

	selectorRaw := make([]byte, selectorBytes)
	if _, err := rand.Read(selectorRaw); err != nil {
		return nil, fmt.Errorf("generating credential selector: %w", err)
	}
	verifierRaw := make([]byte, verifierBytes)
	if _, err := rand.Read(verifierRaw); err != nil {
		return nil, fmt.Errorf("generating credential secret: %w", err)
	}

	// The selector is hex, not base64url, and that is load-bearing rather than
	// arbitrary: the two halves are joined with '_', and the base64url alphabet
	// contains '_'. A base64url selector would carry a separator inside itself
	// roughly a third of the time, and the credential would then split at the
	// wrong offset and fail to authenticate. Hex has no character in common
	// with the separator, so the split is unambiguous.
	selector := hex.EncodeToString(selectorRaw)
	verifier := base64.RawURLEncoding.EncodeToString(verifierRaw)

	hash, err := HashSecret(verifier)
	if err != nil {
		return nil, err
	}
	return &Credential{
		Plaintext: prefix + selector + "_" + verifier,
		Selector:  selector,
		Hash:      hash,
	}, nil
}

// ParsedCredential is a presented secret split into its lookup and secret parts.
type ParsedCredential struct {
	Prefix   string
	Selector string
	Verifier string
}

// ParseCredential splits a presented machine credential.
func ParseCredential(secret string) (*ParsedCredential, bool) {
	for _, prefix := range AllPrefixes {
		if !strings.HasPrefix(secret, prefix) {
			continue
		}
		body := secret[len(prefix):]
		// Cut at the first '_'. The selector is hex and therefore cannot
		// contain one; the verifier is base64url and may, which is why the
		// split is anchored at the front rather than the back.
		selector, verifier, found := strings.Cut(body, "_")
		if !found || selector == "" || verifier == "" {
			return nil, false
		}
		if _, err := hex.DecodeString(selector); err != nil {
			return nil, false
		}
		return &ParsedCredential{Prefix: prefix, Selector: selector, Verifier: verifier}, true
	}
	return nil, false
}

// LooksLikeCredential reports whether a string carries one of Mantle's secret
// prefixes. The log scrubber uses this to redact values it has no other way to
// recognise.
func LooksLikeCredential(s string) bool {
	for _, prefix := range AllPrefixes {
		if strings.Contains(s, prefix) {
			return true
		}
	}
	return false
}

// HashSecret produces an Argon2id PHC-format hash.
//
// The encoded form carries its own parameters, so the cost can be raised in a
// future release without invalidating existing hashes — old hashes verify
// against the parameters they were made with and are rewritten on next use.
func HashSecret(secret string) (string, error) {
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("generating password salt: %w", err)
	}
	key := argon2.IDKey([]byte(secret), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	encoding := base64.RawStdEncoding
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argonMemory, argonTime, argonThreads,
		encoding.EncodeToString(salt), encoding.EncodeToString(key)), nil
}

// VerifySecret checks a presented secret against an encoded hash in constant
// time.
func VerifySecret(secret, encoded string) bool {
	params, salt, want, err := decodeHash(encoded)
	if err != nil {
		return false
	}
	got := argon2.IDKey([]byte(secret), salt, params.time, params.memory, params.threads, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1
}

type argonParams struct {
	memory  uint32
	time    uint32
	threads uint8
}

func decodeHash(encoded string) (argonParams, []byte, []byte, error) {
	parts := strings.Split(encoded, "$")
	// "", "argon2id", "v=19", "m=…,t=…,p=…", salt, key
	if len(parts) != 6 || parts[1] != "argon2id" {
		return argonParams{}, nil, nil, fmt.Errorf("not an argon2id hash")
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil {
		return argonParams{}, nil, nil, fmt.Errorf("malformed argon2 version")
	}
	if version != argon2.Version {
		return argonParams{}, nil, nil, fmt.Errorf("unsupported argon2 version %d", version)
	}
	var p argonParams
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &p.memory, &p.time, &p.threads); err != nil {
		return argonParams{}, nil, nil, fmt.Errorf("malformed argon2 parameters")
	}
	encoding := base64.RawStdEncoding
	salt, err := encoding.DecodeString(parts[4])
	if err != nil {
		return argonParams{}, nil, nil, fmt.Errorf("malformed argon2 salt")
	}
	key, err := encoding.DecodeString(parts[5])
	if err != nil {
		return argonParams{}, nil, nil, fmt.Errorf("malformed argon2 key")
	}
	return p, salt, key, nil
}
