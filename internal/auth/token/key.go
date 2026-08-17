package token

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
)

// KeySize is the RSA modulus size for the token signing key. 2048 is the
// floor every JWT library accepts and is what registry clients expect; larger
// keys cost signing time on a path that runs on every push and pull.
const KeySize = 2048

// SigningKey is the RS256 key pair used to sign registry tokens, along with the
// key id clients see in the JWT header and in JWKS.
type SigningKey struct {
	Private *rsa.PrivateKey
	KeyID   string
}

// LoadOrCreateKey reads the signing key from disk, generating one if absent.
//
// The key is generated at install and lives for the instance's lifetime.
// Publishing the public half at /auth/jwks.json means a future federated
// deployment can verify tokens without being handed the private key, which is
// the reason this is asymmetric rather than an HMAC secret.
func LoadOrCreateKey(path string) (*SigningKey, error) {
	data, err := os.ReadFile(path)
	if err == nil {
		return parseKey(data, path)
	}
	if !os.IsNotExist(err) {
		return nil, fmt.Errorf("reading token signing key %s: %w", path, err)
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("creating the key directory for %s: %w", path, err)
	}
	key, err := rsa.GenerateKey(rand.Reader, KeySize)
	if err != nil {
		return nil, fmt.Errorf("generating a token signing key: %w", err)
	}
	encoded := pem.EncodeToMemory(&pem.Block{
		Type:  "PRIVATE KEY",
		Bytes: mustMarshalPKCS8(key),
	})
	// 0600 and written before anything else uses it. A signing key readable by
	// other local accounts lets any of them mint a token for any repository.
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		return nil, fmt.Errorf("writing the token signing key to %s: %w", path, err)
	}
	return &SigningKey{Private: key, KeyID: keyID(&key.PublicKey)}, nil
}

func parseKey(data []byte, path string) (*SigningKey, error) {
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, fmt.Errorf("token signing key %s is not valid PEM", path)
	}
	var key *rsa.PrivateKey
	switch block.Type {
	case "RSA PRIVATE KEY":
		parsed, err := x509.ParsePKCS1PrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("parsing token signing key %s: %w", path, err)
		}
		key = parsed
	case "PRIVATE KEY":
		parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("parsing token signing key %s: %w", path, err)
		}
		rsaKey, ok := parsed.(*rsa.PrivateKey)
		if !ok {
			return nil, fmt.Errorf("token signing key %s is a %T, but RS256 requires an RSA key", path, parsed)
		}
		key = rsaKey
	default:
		return nil, fmt.Errorf("token signing key %s has unexpected PEM block type %q", path, block.Type)
	}
	if key.N.BitLen() < 2048 {
		return nil, fmt.Errorf("token signing key %s is %d bits; RS256 requires at least 2048",
			path, key.N.BitLen())
	}
	return &SigningKey{Private: key, KeyID: keyID(&key.PublicKey)}, nil
}

func mustMarshalPKCS8(key *rsa.PrivateKey) []byte {
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		// Marshalling a freshly generated RSA key cannot fail.
		panic(fmt.Sprintf("marshalling generated RSA key: %v", err))
	}
	return der
}

// keyID derives a stable identifier from the public key, so that a key rotation
// changes the kid and clients holding a cached JWKS notice.
func keyID(pub *rsa.PublicKey) string {
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return "unknown"
	}
	sum := sha256.Sum256(der)
	return base64.RawURLEncoding.EncodeToString(sum[:16])
}

// JWKS is the JSON Web Key Set published at /auth/jwks.json.
type JWKS struct {
	Keys []JWK `json:"keys"`
}

// JWK is one public key in the set.
type JWK struct {
	Kty string `json:"kty"`
	Use string `json:"use"`
	Alg string `json:"alg"`
	Kid string `json:"kid"`
	N   string `json:"n"`
	E   string `json:"e"`
}

// PublicJWKS renders the public half of the signing key as a JWKS.
func (k *SigningKey) PublicJWKS() JWKS {
	pub := &k.Private.PublicKey
	return JWKS{Keys: []JWK{{
		Kty: "RSA",
		Use: "sig",
		Alg: "RS256",
		Kid: k.KeyID,
		N:   base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
		E:   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes()),
	}}}
}
